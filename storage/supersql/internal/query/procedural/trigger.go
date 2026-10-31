package procedural

import (
	"context"
	"fmt"
	"google.golang.org/protobuf/proto"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type pendingTrigger struct {
	descriptor model.TriggerDescriptor
	table      *model.Descriptor
	event      model.TriggerEvent
	old        *model.Row
	new        *model.Row
	layout     model.TypeLayout
}

type triggerStatement struct {
	table           model.ObjectID
	event           model.TriggerEvent
	updated         []model.ColumnID
	pending         []pendingTrigger
	oldTransition   []model.Row
	newTransition   []model.Row
	transitionRows  int
	transitionBytes uint64
}

// TriggerRuntime is owned by one session transaction. Session execution
// serializes calls; nested statements are represented by the stack.
type TriggerRuntime struct {
	resolver *Resolver
	limits   Limits
	depth    int
	stack    []triggerStatement
}

func (runtime *TriggerRuntime) TriggerDepth(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if runtime == nil {
		return 0, ErrLimit
	}
	return runtime.depth, nil
}

func NewTriggerRuntime(resolver *Resolver, limits Limits) (*TriggerRuntime, error) {
	if resolver == nil {
		return nil, ErrInvalidProgram
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &TriggerRuntime{resolver: resolver, limits: limits}, nil
}

func (runtime *TriggerRuntime) BeginStatement(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, updated []model.ColumnID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if table == nil || event == 0 || event > model.TriggerTruncate || len(runtime.stack) >= runtime.limits.MaxTriggerDepth {
		return ErrLimit
	}
	runtime.stack = append(runtime.stack, triggerStatement{table: table.ID(), event: event, updated: slices.Clone(updated)})
	if err := runtime.fireStatement(ctx, table, event, "before"); err != nil {
		runtime.removeStatement(len(runtime.stack) - 1)
		return err
	}
	return nil
}

func (runtime *TriggerRuntime) BeforeRow(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, old, new *model.Row, maxBytes uint64) (*model.Row, bool, error) {
	triggers, layout, err := runtime.triggers(ctx, table, event, "before", true, maxBytes)
	if err != nil {
		return nil, false, err
	}
	current, err := copyTriggerRow(ctx, new, maxBytes)
	if err != nil {
		return nil, false, err
	}
	for _, trigger := range triggers {
		invocationNew := current
		if event == model.TriggerDelete {
			invocationNew = nil
		}
		invocation := runtime.context(table, trigger, event, layout, old, invocationNew)
		current, _, err = runtime.invoke(ctx, trigger, invocation)
		if err != nil {
			return nil, false, err
		}
		if current == nil {
			return nil, false, nil
		}
	}
	instead, _, err := runtime.triggers(ctx, table, event, "instead", true, maxBytes)
	if err != nil {
		return nil, false, err
	}
	if len(instead) != 0 {
		candidate := current
		if event == model.TriggerDelete {
			candidate, err = copyTriggerRow(ctx, old, maxBytes)
			if err != nil {
				return nil, false, err
			}
		}
		for _, trigger := range instead {
			invocationOld, invocationNew := old, candidate
			if event == model.TriggerDelete {
				invocationNew = nil
			}
			invocation := runtime.context(table, trigger, event, layout, invocationOld, invocationNew)
			candidate, _, err = runtime.invoke(ctx, trigger, invocation)
			if err != nil {
				return nil, false, err
			}
			if candidate == nil {
				return nil, false, nil
			}
		}
		return candidate, true, nil
	}
	if event == model.TriggerDelete {
		return nil, true, nil
	}
	return current, true, nil
}

func (runtime *TriggerRuntime) AfterRow(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, old, new *model.Row, maxBytes uint64) error {
	triggers, layout, err := runtime.triggers(ctx, table, event, "after", true, maxBytes)
	if err != nil {
		return err
	}
	if err := runtime.captureTransitions(ctx, table, event, old, new, maxBytes); err != nil {
		return err
	}
	for _, trigger := range triggers {
		oldCopy, err := copyTriggerRow(ctx, old, maxBytes)
		if err != nil {
			return err
		}
		newCopy, err := copyTriggerRow(ctx, new, maxBytes)
		if err != nil {
			return err
		}
		deferred := false
		if trigger.Constraint && trigger.Deferrable {
			executor, err := SQLExecutorFromContext(ctx)
			if err != nil {
				return err
			}
			deferred, err = executor.TriggerDeferred(ctx, trigger)
			if err != nil {
				return err
			}
		}
		invocation := runtime.context(table, trigger, event, layout, oldCopy, newCopy)
		eligible, err := runtime.when(ctx, trigger, invocation)
		if err != nil {
			return err
		}
		if !eligible {
			continue
		}
		trigger.When = nil
		invocation.OldTransitionName, invocation.NewTransitionName = trigger.OldTransitionTable, trigger.NewTransitionTable
		if deferred {
			if err := runtime.deferTrigger(ctx, trigger, invocation); err != nil {
				return err
			}
			continue
		}
		index := runtime.statementIndex(table.ID(), event)
		if index < 0 || len(runtime.stack[index].pending) >= runtime.limits.MaxDeferredEvents {
			return ErrLimit
		}
		pending := pendingTrigger{descriptor: trigger, table: table, event: event, old: oldCopy, new: newCopy, layout: layout}
		runtime.stack[index].pending = append(runtime.stack[index].pending, pending)
	}
	return nil
}

func (runtime *TriggerRuntime) deferTrigger(ctx context.Context, trigger model.TriggerDescriptor, invocation TriggerContext) error {
	function := trigger.Function
	plan, ok := runtime.resolver.plans[function]

	if trigger.Routine == 0 || function == 0 || !ok || plan.program == nil {
		return sqlError("42883", fmt.Sprintf("trigger routine %d is not present in the pinned snapshot", trigger.Routine), nil)
	}
	executor, err := SQLExecutorFromContext(ctx)
	if err != nil {
		return err
	}
	return executor.DeferTrigger(ctx, DeferredTrigger{Program: plan.program, Context: invocation})
}

func (runtime *TriggerRuntime) statementIndex(table model.ObjectID, event model.TriggerEvent) int {
	for i := len(runtime.stack) - 1; i >= 0; i-- {
		if runtime.stack[i].table == table && runtime.stack[i].event == event {
			return i
		}
	}
	return -1
}

func (runtime *TriggerRuntime) removeStatement(index int) {
	clear(runtime.stack[index].updated)
	clear(runtime.stack[index].pending)
	clear(runtime.stack[index].oldTransition)
	clear(runtime.stack[index].newTransition)
	copy(runtime.stack[index:], runtime.stack[index+1:])
	runtime.stack[len(runtime.stack)-1] = triggerStatement{}
	runtime.stack = runtime.stack[:len(runtime.stack)-1]
}

func (runtime *TriggerRuntime) AbortStatement(table *model.Descriptor, event model.TriggerEvent) {
	if table == nil {
		return
	}
	index := runtime.statementIndex(table.ID(), event)
	if index >= 0 {
		runtime.removeStatement(index)
	}
}

func (runtime *TriggerRuntime) FinishStatement(ctx context.Context, table *model.Descriptor, event model.TriggerEvent) error {
	if table == nil {
		return ErrInvalidProgram
	}
	index := runtime.statementIndex(table.ID(), event)
	if index < 0 {
		return ErrInvalidProgram
	}
	statement := runtime.stack[index]
	defer runtime.removeStatement(index)
	for _, pending := range statement.pending {
		if _, _, err := runtime.invoke(ctx, pending.descriptor, runtime.context(pending.table, pending.descriptor, pending.event, pending.layout, pending.old, pending.new)); err != nil {
			return err
		}
	}
	return runtime.fireStatement(ctx, table, event, "after")
}

func (runtime *TriggerRuntime) fireStatement(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, timing string) error {
	triggers, layout, err := runtime.triggers(ctx, table, event, timing, false, runtime.limits.MaxResultBytes)
	if err != nil {
		return err
	}
	for _, trigger := range triggers {
		if _, _, err := runtime.invoke(ctx, trigger, runtime.context(table, trigger, event, layout, nil, nil)); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *TriggerRuntime) triggers(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, timing string, row bool, maxBytes uint64) ([]model.TriggerDescriptor, model.TypeLayout, error) {
	if table == nil || maxBytes == 0 {
		return nil, model.TypeLayout{}, ErrInvalidProgram
	}
	spec, err := table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: maxBytes, MaxItems: runtime.limits.MaxStatements, MaxDepth: 64})
	if err != nil {
		return nil, model.TypeLayout{}, err
	}
	columns, descriptors, ok := relationTriggers(spec)
	if !ok {
		return nil, model.TypeLayout{}, model.ErrInvalidDescriptor
	}
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}}
	for _, column := range columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			layout.Children = append(layout.Children, column.Layout)
			layout.FieldNames = append(layout.FieldNames, column.Name)
		}
	}
	name := triggerEventName(event)
	var selected []model.TriggerDescriptor
	for _, trigger := range descriptors {
		if !trigger.EnableMode.Fires(false) || trigger.Timing != timing || trigger.ForEachRow != row || !slices.Contains(trigger.Events, name) {
			continue
		}
		if event == model.TriggerUpdate && len(trigger.Columns) != 0 {
			index := runtime.statementIndex(table.ID(), event)
			if index < 0 || !slices.ContainsFunc(trigger.Columns, func(column model.ColumnID) bool {
				return slices.Contains(runtime.stack[index].updated, column)
			}) {
				continue
			}
		}
		selected = append(selected, trigger)
	}
	slices.SortFunc(selected, func(left, right model.TriggerDescriptor) int { return stringsCompare(left.Name, right.Name) })
	return selected, layout, nil
}

func relationTriggers(spec model.DescriptorSpec) ([]model.ColumnDescriptor, []model.TriggerDescriptor, bool) {
	switch {
	case spec.Table != nil:
		return spec.Table.Columns, spec.Table.Triggers, true
	case spec.View != nil:
		return spec.View.Columns, spec.View.Triggers, true
	default:
		return nil, nil, false
	}
}

func (runtime *TriggerRuntime) captureTransitions(ctx context.Context, table *model.Descriptor, event model.TriggerEvent, old, new *model.Row, maxBytes uint64) error {
	index := -1
	if table != nil {
		index = runtime.statementIndex(table.ID(), event)
	}
	if index < 0 {
		return ErrInvalidProgram
	}
	statement := &runtime.stack[index]
	spec, err := table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: max(maxBytes, runtime.limits.MaxResultBytes), MaxItems: runtime.limits.MaxStatements, MaxDepth: 64})
	if err != nil {
		return err
	}
	_, descriptors, ok := relationTriggers(spec)
	if !ok {
		return model.ErrInvalidDescriptor
	}
	eventName := triggerEventName(event)
	required := false
	for _, trigger := range descriptors {
		if trigger.EnableMode.Fires(false) && slices.Contains(trigger.Events, eventName) && (trigger.OldTransitionTable != "" || trigger.NewTransitionTable != "") {
			required = true
			break
		}
	}
	if !required {
		return nil
	}
	if statement.transitionRows >= runtime.limits.MaxTransitionRows || statement.transitionBytes >= runtime.limits.MaxResultBytes {
		return ErrLimit
	}
	remaining := runtime.limits.MaxResultBytes - statement.transitionBytes
	if old != nil {
		copy, err := copyTriggerRow(ctx, old, remaining)
		if err != nil {
			return err
		}
		size, err := triggerRowBytes(ctx, copy, remaining)
		if err != nil || size > remaining {
			return ErrLimit
		}
		statement.oldTransition = append(statement.oldTransition, *copy)
		statement.transitionBytes += size
		remaining -= size
	}
	if new != nil {
		copy, err := copyTriggerRow(ctx, new, remaining)
		if err != nil {
			return err
		}
		size, err := triggerRowBytes(ctx, copy, remaining)
		if err != nil || size > remaining {
			return ErrLimit
		}
		statement.newTransition = append(statement.newTransition, *copy)
		statement.transitionBytes += size
	}
	statement.transitionRows++
	return nil
}

func triggerRowBytes(ctx context.Context, row *model.Row, limit uint64) (uint64, error) {
	if row == nil {
		return 0, nil
	}
	if uint64(len(row.Key)) > limit {
		return 0, ErrLimit
	}
	used := uint64(len(row.Key))
	for _, value := range row.Values {
		size, err := model.ValueBytes(ctx, value, limit-used)
		if err != nil || size > limit-used {
			return 0, err
		}
		used += size
	}
	return used, nil
}

func (runtime *TriggerRuntime) context(table *model.Descriptor, trigger model.TriggerDescriptor, event model.TriggerEvent, layout model.TypeLayout, old, new *model.Row) TriggerContext {
	level := "statement"
	if trigger.ForEachRow {
		level = "row"
	}
	invocation := TriggerContext{Name: trigger.Name, Timing: trigger.Timing, Event: triggerOperationName(event), Level: level, TableName: table.Name().Name, SchemaName: trigger.SchemaName, Relation: table.OID(), ConstraintTable: trigger.ConstraintTable, ConstraintID: trigger.ConstraintID, Arguments: slices.Clone(trigger.Arguments), Layout: layout, Old: old, New: new, OldTransitionName: trigger.OldTransitionTable, NewTransitionName: trigger.NewTransitionTable}
	if index := runtime.statementIndex(table.ID(), event); index >= 0 {
		statement := &runtime.stack[index]
		invocation.OldTransition = statement.oldTransition
		invocation.NewTransition = statement.newTransition
	}
	return invocation
}

func (runtime *TriggerRuntime) invoke(ctx context.Context, trigger model.TriggerDescriptor, invocation TriggerContext) (*model.Row, bool, error) {
	if runtime.depth >= runtime.limits.MaxTriggerDepth {
		return nil, false, ErrLimit
	}
	eligible, err := runtime.when(ctx, trigger, invocation)
	if err != nil {
		return nil, false, err
	}
	if !eligible {
		if invocation.New != nil {
			return invocation.New, false, nil
		}
		return invocation.Old, false, nil
	}
	runtime.depth++
	defer func() { runtime.depth-- }()
	if trigger.Routine == 0 {
		executor, err := SQLExecutorFromContext(ctx)
		if err != nil {
			return nil, false, err
		}
		return executor.InvokeNativeTrigger(ctx, trigger.Function, invocation)
	}
	function := runtime.resolver.objects[trigger.Routine]
	if function == 0 || function != trigger.Function {
		return nil, false, sqlError("42883", fmt.Sprintf("trigger routine %d is not present in the pinned snapshot", trigger.Routine), nil)
	}
	return runtime.resolver.InvokeTrigger(ctx, function, invocation)
}

func (runtime *TriggerRuntime) when(ctx context.Context, trigger model.TriggerDescriptor, invocation TriggerContext) (bool, error) {
	if trigger.When == nil {
		return true, nil
	}
	if trigger.When.SemanticVersion != 1 || len(trigger.When.Encoding) == 0 {
		return false, ErrInvalidProgram
	}
	statement := &zpgparse.RawStmt{}
	if err := proto.Unmarshal(trigger.When.Encoding, statement); err != nil {
		return false, ErrInvalidProgram
	}
	parameters := make([]Parameter, 0, len(invocation.Layout.Children)*2)
	appendRow := func(row *model.Row) error {
		for i, layout := range invocation.Layout.Children {
			value := model.NullVal(layout.Type.Storage)
			if row != nil {
				if i >= len(row.Values) {
					return ErrInvalidProgram
				}
				value = row.Values[i]
			}
			parameters = append(parameters, Parameter{Layout: layout, Value: value})
		}
		return nil
	}
	if err := appendRow(invocation.New); err != nil {
		return false, err
	}
	if err := appendRow(invocation.Old); err != nil {
		return false, err
	}
	executor, err := SQLExecutorFromContext(ctx)
	if err != nil {
		return false, err
	}
	result, err := executor.ExecuteSQL(ctx, SQLRequest{Statement: statement, Parameters: parameters, MaxRows: 2, MaxBytes: runtime.limits.MaxResultBytes})
	if err != nil {
		return false, err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		return false, ErrInvalidProgram
	}
	value := result.Rows[0][0]
	if value.IsNull {
		return false, nil
	}
	if value.Type != model.TypeBoolean {
		return false, ErrInvalidProgram
	}
	return value.IntVal != 0, nil
}

func triggerEventName(event model.TriggerEvent) string {
	switch event {
	case model.TriggerInsert:
		return "insert"
	case model.TriggerUpdate:
		return "update"
	case model.TriggerDelete:
		return "delete"
	case model.TriggerTruncate:
		return "truncate"
	default:
		return ""
	}
}

// triggerOperationName is the TG_OP spelling PostgreSQL exposes to trigger
// bodies; trigger.Events keeps the lowercase catalog spelling.
func triggerOperationName(event model.TriggerEvent) string {
	return strings.ToUpper(triggerEventName(event))
}

func copyTriggerRow(ctx context.Context, row *model.Row, maxBytes uint64) (*model.Row, error) {
	if row == nil {
		return nil, nil
	}
	copy := &model.Row{Key: slices.Clone(row.Key), Values: make([]model.Value, len(row.Values))}
	used := uint64(len(copy.Key))
	for i := range row.Values {
		if used > maxBytes {
			return nil, ErrLimit
		}
		value, size, err := model.CopyValue(ctx, row.Values[i], maxBytes-used)
		if err != nil {
			return nil, err
		}
		copy.Values[i], used = value, used+size
	}
	return copy, nil
}

func stringsCompare(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

var _ model.TriggerRuntime = (*TriggerRuntime)(nil)
