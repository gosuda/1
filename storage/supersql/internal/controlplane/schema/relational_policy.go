package schema

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errRelationalPolicy = &model.Error{Code: "55000", Message: "relational constraint policy is incomplete", Cause: storage.ErrInvalidTarget}
	errRelationalLimit  = &model.Error{Code: "54000", Message: "relational constraint execution exceeds resource limit", Cause: model.ErrValueBudget}
)

type relationalSet struct {
	tables        []relationalTable
	encoder       storage.KeyEncoder
	maxBytes      uint64
	retainedBytes uint64
}

type relationalTable struct {
	descriptor *model.Descriptor
	table      *model.TableMetadata
	schemaName string
	target     storage.ObjectRef
	layout     []storage.ColumnLayout
	keyLayout  []storage.KeyColumn
	indexes    *IndexSet
}

// LinkRelationalConstraints freezes a complete FK-connected set before any set
// is shared. Every outgoing referenced table must be present; callers include
// incoming referencing tables when referential actions are required.
func LinkRelationalConstraints(ctx context.Context, sets []*IndexSet, encoder storage.KeyEncoder, maxBytes uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(sets) == 0 || encoder == nil || maxBytes == 0 {
		return errRelationalPolicy
	}
	fixed := uint64(unsafe.Sizeof(relationalSet{})) + uint64(len(sets))*uint64(unsafe.Sizeof(relationalTable{}))
	if fixed > maxBytes {
		return errRelationalLimit
	}
	relations := &relationalSet{tables: make([]relationalTable, len(sets)), encoder: encoder}
	seen := make(map[model.ObjectID]struct{}, len(sets))
	for i, set := range sets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if set == nil || set.table == nil || set.schemaName == "" || set.relations != nil {
			return errRelationalPolicy
		}
		if _, duplicate := seen[set.target.ObjectID]; duplicate {
			return errRelationalPolicy
		}
		seen[set.target.ObjectID] = struct{}{}
		spec, specBytes, err := set.table.SpecWithinBytes(ctx, model.DescriptorLimits{MaxBytes: maxBytes - fixed, MaxItems: max(set.limits.MaxColumns, len(set.indexes)), MaxDepth: 64})
		if err != nil || spec.Table == nil || specBytes > maxBytes-fixed {
			return errors.Join(errRelationalPolicy, err)
		}
		fixed += specBytes
		table, used, err := newRelationalTable(ctx, set, spec.Table, maxBytes-fixed)
		if err != nil {
			return err
		}
		if used > maxBytes-fixed {
			return errRelationalLimit
		}
		fixed += used
		relations.tables[i] = table
	}
	slices.SortFunc(relations.tables, func(a, b relationalTable) int {
		switch {
		case a.target.ObjectID < b.target.ObjectID:
			return -1
		case a.target.ObjectID > b.target.ObjectID:
			return 1
		default:
			return 0
		}
	})
	for i := range relations.tables {
		table := &relations.tables[i]
		for _, constraint := range table.table.Constraints {
			if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced {
				continue
			}
			parent, ok := relations.table(constraint.ReferencedObject)
			if !ok {
				return errRelationalPolicy
			}
			binding, ok := parent.indexes.index(constraint.ReferencedIndex)
			if !ok || !binding.plan.Valid() || binding.plan.State() != model.StatePublic || len(binding.plan.keys) != len(constraint.ReferencedColumns) {
				return errRelationalPolicy
			}
			for key, column := range constraint.ReferencedColumns {
				if binding.plan.keys[key].expression != nil || binding.plan.baseColumns[binding.plan.keys[key].column].ID != column {
					return errRelationalPolicy
				}
			}
		}
	}
	for _, set := range sets {
		if set.limits.EntryBytes > ^uint64(0)/8 {
			return errRelationalLimit
		}
		relations.maxBytes = max(relations.maxBytes, 8*set.limits.EntryBytes)
	}
	relations.retainedBytes = fixed
	for _, set := range sets {
		set.relations = relations
	}
	return ctx.Err()
}

func newRelationalTable(ctx context.Context, set *IndexSet, table *model.TableMetadata, maxBytes uint64) (relationalTable, uint64, error) {
	result := relationalTable{descriptor: set.table, table: table, schemaName: set.schemaName, target: set.target, indexes: set}
	used := uint64(len(table.Columns))*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + uint64(len(table.PrimaryKey))*uint64(unsafe.Sizeof(storage.KeyColumn{}))
	if used > maxBytes {
		return relationalTable{}, 0, errRelationalLimit
	}
	for _, column := range table.Columns {
		switch column.State {
		case model.StatePublic, model.StateWriteOnly:
			result.layout = append(result.layout, storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable})
		case model.StateDeleteOnly, model.StateDropWriteOnly, model.StateDropDeleteOnly:
		default:
			return relationalTable{}, 0, errRelationalPolicy
		}
	}
	for _, key := range table.PrimaryKey {
		position := slices.IndexFunc(result.layout, func(column storage.ColumnLayout) bool { return column.ID == key.Column })
		if position < 0 {
			return relationalTable{}, 0, errRelationalPolicy
		}
		column := result.layout[position]
		result.keyLayout = append(result.keyLayout, storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: column.Children, Descending: key.Descending, NullsFirst: key.NullsFirst})
	}
	if len(result.layout) == 0 || len(result.keyLayout) == 0 {
		return relationalTable{}, 0, errRelationalPolicy
	}
	if err := ctx.Err(); err != nil {
		return relationalTable{}, 0, err
	}
	return result, used, nil
}

func (r *relationalSet) table(id model.ObjectID) (*relationalTable, bool) {
	position, found := slices.BinarySearchFunc(r.tables, id, func(table relationalTable, id model.ObjectID) int {
		switch {
		case table.target.ObjectID < id:
			return -1
		case table.target.ObjectID > id:
			return 1
		default:
			return 0
		}
	})
	if !found {
		return nil, false
	}
	return &r.tables[position], true
}

type relationalExecution struct {
	set         *relationalSet
	transaction txn.ConstraintTransaction
	deferral    txn.ConstraintDeferral
	triggers    model.TriggerRuntime
	runtime     model.ExpressionRuntime
	allocator   columnar.Allocator
	maxBytes    uint64
	visits      map[relationalVisit]struct{}
}

type relationalVisit struct {
	object model.ObjectID
	event  model.TriggerEvent
	key    string
}

// ExpandRelational runs after ordinary index expansion, inside the caller's
// statement rollback boundary. Deferred events own their final-overlay probes.
func (s *IndexSet) ExpandRelational(ctx context.Context, transaction txn.ConstraintTransaction, deferral txn.ConstraintDeferral, triggers model.TriggerRuntime, runtime model.ExpressionRuntime, allocator columnar.Allocator, maxBytes uint64, old, current *model.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || transaction == nil || allocator == nil || maxBytes == 0 {
		return errRelationalPolicy
	}
	if s.relations == nil {
		spec, err := s.table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: s.limits.MetadataBytes, MaxItems: max(s.limits.MaxColumns, len(s.indexes)), MaxDepth: 64})
		if err != nil {
			return err
		}
		for _, constraint := range spec.Table.Constraints {
			relational := constraint.Kind == model.ConstraintForeignKey || constraint.Kind == model.ConstraintExclude
			deferredKey := constraint.Deferrable && (constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique)
			if constraint.Enforced && (relational || deferredKey) {
				return errRelationalPolicy
			}
		}
		return ctx.Err()
	}
	table, ok := s.relations.table(s.target.ObjectID)
	if !ok || table.indexes != s {
		return errRelationalPolicy
	}
	execution := relationalExecution{set: s.relations, transaction: transaction, deferral: deferral, triggers: triggers, runtime: runtime, allocator: allocator, maxBytes: min(maxBytes, s.relations.maxBytes), visits: make(map[relationalVisit]struct{})}
	return execution.expand(ctx, table, old, current, 0)
}

// ExpandDeleteRange converts one encoded base-table interval into point
// deletions so indexes, relational constraints, cascades and row triggers see
// every affected row. The caller owns the surrounding statement rollback
// boundary and supplies the same deferral queue used by ordinary DELETE.
func (s *IndexSet) ExpandDeleteRange(ctx context.Context, transaction txn.ConstraintTransaction, deferral txn.ConstraintDeferral, triggers model.TriggerRuntime, runtime model.ExpressionRuntime, allocator columnar.Allocator, encoder storage.KeyEncoder, maxBytes uint64, mutation storage.Mutation) (err error) {
	if s == nil || transaction == nil || encoder == nil || mutation.Op != storage.MutationDeleteRange || mutation.Target != s.target || len(mutation.Key) == 0 || len(mutation.EndKey) == 0 || bytes.Compare(mutation.Key, mutation.EndKey) >= 0 || maxBytes == 0 {
		return errRelationalPolicy
	}
	if triggers != nil {
		if err := triggers.BeginStatement(ctx, s.table, model.TriggerDelete, nil); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				triggers.AbortStatement(s.table, model.TriggerDelete)
				return
			}
			err = triggers.FinishStatement(ctx, s.table, model.TriggerDelete)
		}()
	}
	spec, specErr := s.table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: s.limits.MetadataBytes, MaxItems: s.limits.MaxColumns, MaxDepth: 64})
	if specErr != nil || spec.Table == nil {
		return errors.Join(errRelationalPolicy, specErr)
	}
	table, _, err := newRelationalTable(ctx, s, spec.Table, s.limits.MetadataBytes)
	if err != nil {
		return err
	}
	return scanRelationalRows(ctx, transaction, &table, encoder, maxBytes, func(previous *model.Row) error {
		if bytes.Compare(previous.Key, mutation.Key) < 0 || bytes.Compare(previous.Key, mutation.EndKey) >= 0 {
			return nil
		}
		if triggers != nil {
			_, proceed, triggerErr := triggers.BeforeRow(ctx, s.table, model.TriggerDelete, previous, nil, maxBytes)
			if triggerErr != nil {
				return triggerErr
			}
			if !proceed {
				return nil
			}
		}
		point := storage.NewDeleteMutation(s.target, previous.Key)
		if applyErr := transaction.ApplyMutations(ctx, []storage.Mutation{point}); applyErr != nil {
			return applyErr
		}
		if expandErr := s.Expand(ctx, transaction, previous, nil); expandErr != nil {
			return expandErr
		}
		if expandErr := s.ExpandRelational(ctx, transaction, deferral, triggers, runtime, allocator, maxBytes, previous, nil); expandErr != nil {
			return expandErr
		}
		if triggers != nil {
			return triggers.AfterRow(ctx, s.table, model.TriggerDelete, previous, nil, maxBytes)
		}
		return nil
	})
}

func (e *relationalExecution) expand(ctx context.Context, table *relationalTable, old, current *model.Row, depth int) error {
	if depth >= 64 || uint64(len(e.visits)) >= e.maxBytes/uint64(unsafe.Sizeof(relationalVisit{})+16) {
		return errRelationalLimit
	}
	for _, constraint := range table.table.Constraints {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !constraint.Enforced {
			continue
		}
		switch constraint.Kind {
		case model.ConstraintPrimaryKey, model.ConstraintUnique:
			if current != nil && !current.Deleted && constraint.Deferrable {
				if err := e.checkIndexConstraint(ctx, table, constraint, current); err != nil {
					return err
				}
			}
		case model.ConstraintExclude:
			if current != nil && !current.Deleted {
				if err := e.checkIndexConstraint(ctx, table, constraint, current); err != nil {
					return err
				}
			}
		case model.ConstraintForeignKey:
			if current != nil && !current.Deleted {
				if err := e.checkForeignKey(ctx, table, constraint, current); err != nil {
					return err
				}
			}
		}
	}
	if old != nil && !old.Deleted {
		return e.applyReferencedActions(ctx, table, old, current, depth)
	}
	return ctx.Err()
}

func constraintIdentity(table *relationalTable, constraint model.ConstraintDescriptor) txn.ConstraintIdentity {
	return txn.ConstraintIdentity{Object: table.target.ObjectID, Constraint: constraint.ID, Schema: table.schemaName, Name: constraint.Name, Deferrable: constraint.Deferrable, InitiallyDeferred: constraint.InitiallyDeferred}
}

func (e *relationalExecution) deferOrValidate(ctx context.Context, constraint model.ConstraintDescriptor, event txn.DeferredConstraint) error {
	if e.deferral != nil {
		deferred, err := e.deferral.DeferConstraint(ctx, event)
		if err != nil {
			return err
		}
		if deferred {
			return ctx.Err()
		}
		return event.Validate(ctx, e.transaction)
	}
	if constraint.InitiallyDeferred {
		return &model.Error{Code: "55000", Message: "deferred constraint queue is unavailable"}
	}
	return event.Validate(ctx, e.transaction)
}

func (e *relationalExecution) checkIndexConstraint(ctx context.Context, table *relationalTable, constraint model.ConstraintDescriptor, row *model.Row) error {
	binding, found := table.indexes.index(constraint.Index)
	if !found || !constraint.Mutation && !binding.plan.Valid() || constraint.Mutation && !binding.plan.WriteMaintained() ||
		binding.plan.State() != model.StatePublic && (binding.plan.State() != model.StateWriteOnly || !constraint.Mutation) {
		return errRelationalPolicy
	}
	entry, err := binding.plan.Entry(ctx, row)
	if err != nil || entry == nil {
		return err
	}
	prefix := entry.Values[:len(binding.plan.keys)]
	if constraint.Kind == model.ConstraintPrimaryKey {
		for _, value := range prefix {
			if value.IsNull {
				return &model.Error{Code: "23502", Message: fmt.Sprintf("null value violates primary key constraint %q", constraint.Name)}
			}
		}
	}
	for _, value := range prefix {
		if value.IsNull && (constraint.Kind == model.ConstraintExclude || !binding.plan.nullsNotDistinct) {
			return nil
		}
	}
	code, message := "23505", fmt.Sprintf("duplicate key violates constraint %q", constraint.Name)
	if constraint.Kind == model.ConstraintExclude {
		code, message = "23P01", fmt.Sprintf("conflicting key violates exclusion constraint %q", constraint.Name)
	}
	event, err := newIndexConstraintEvent(ctx, constraintIdentity(table, constraint), binding, prefix, code, message, e.maxBytes)
	if err != nil {
		return err
	}
	return e.deferOrValidate(ctx, constraint, event)
}

func (e *relationalExecution) checkForeignKey(ctx context.Context, child *relationalTable, constraint model.ConstraintDescriptor, row *model.Row) error {
	values, nulls, err := child.values(constraint.Columns, row)
	if err != nil {
		return err
	}
	if nulls != 0 {
		if constraint.Match == "simple" || nulls == len(values) {
			return nil
		}
		return &model.Error{Code: "23503", Message: fmt.Sprintf("MATCH FULL constraint %q does not allow mixing null and nonnull key values", constraint.Name)}
	}
	parent, ok := e.set.table(constraint.ReferencedObject)
	if !ok {
		return errRelationalPolicy
	}
	binding, ok := parent.indexes.index(constraint.ReferencedIndex)
	if !ok {
		return errRelationalPolicy
	}
	event, err := newForeignKeyEvent(ctx, constraintIdentity(child, constraint), child, constraint, binding, row.Key, values, e.set.encoder, e.maxBytes)
	if err != nil {
		return err
	}
	return e.deferOrValidate(ctx, constraint, event)
}

func (t *relationalTable) position(id model.ColumnID) int {
	position := sort.Search(len(t.layout), func(i int) bool { return t.layout[i].ID >= id })
	if position == len(t.layout) || t.layout[position].ID != id {
		return -1
	}
	return position
}

func (t *relationalTable) values(columns []model.ColumnID, row *model.Row) ([]model.Value, int, error) {
	if row == nil || row.Deleted || len(row.Values) != len(t.layout) {
		return nil, 0, storage.ErrInvalidMutation
	}
	values := make([]model.Value, len(columns))
	nulls := 0
	for i, column := range columns {
		position := t.position(column)
		if position < 0 {
			return nil, 0, errRelationalPolicy
		}
		values[i] = row.Values[position]
		if values[i].IsNull {
			nulls++
		}
	}
	return values, nulls, nil
}

func (e *relationalExecution) applyReferencedActions(ctx context.Context, parent *relationalTable, old, current *model.Row, depth int) error {
	for i := range e.set.tables {
		child := &e.set.tables[i]
		for _, constraint := range child.table.Constraints {
			if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced || constraint.ReferencedObject != parent.target.ObjectID {
				continue
			}
			oldValues, _, err := parent.values(constraint.ReferencedColumns, old)
			if err != nil {
				return err
			}
			if current != nil && !current.Deleted {
				newValues, _, valuesErr := parent.values(constraint.ReferencedColumns, current)
				if valuesErr != nil {
					return valuesErr
				}
				equal, equalErr := encodedValuesEqual(ctx, e.set.encoder, parentIndexLayout(parent, constraint), oldValues, newValues, e.maxBytes)
				if equalErr != nil {
					return equalErr
				}
				if equal {
					continue
				}
			}
			action := constraint.OnDelete
			var replacements []model.Value
			if current != nil && !current.Deleted {
				action = constraint.OnUpdate
				replacements, _, err = parent.values(constraint.ReferencedColumns, current)
				if err != nil {
					return err
				}
			}
			switch action {
			case "restrict":
				found, scanErr := e.hasReferencingRow(ctx, child, constraint, oldValues)
				if scanErr != nil {
					return scanErr
				}
				if found {
					return foreignKeyViolation(constraint)
				}
			case "no action":
				event, eventErr := newReferencedKeyEvent(ctx, constraintIdentity(child, constraint), child, parent, constraint, oldValues, e.set.encoder, e.maxBytes)
				if eventErr != nil {
					return eventErr
				}
				if err := e.deferOrValidate(ctx, constraint, event); err != nil {
					return err
				}
			case "cascade", "set null", "set default":
				if err := e.applyAction(ctx, child, constraint, action, oldValues, replacements, depth+1); err != nil {
					return err
				}
			default:
				return errRelationalPolicy
			}
		}
	}
	return ctx.Err()
}

func parentIndexLayout(parent *relationalTable, constraint model.ConstraintDescriptor) []storage.KeyColumn {
	binding, ok := parent.indexes.index(constraint.ReferencedIndex)
	if !ok || len(binding.plan.keyLayout) < len(constraint.ReferencedColumns) {
		return nil
	}
	return binding.plan.keyLayout[:len(constraint.ReferencedColumns)]
}

func foreignKeyViolation(constraint model.ConstraintDescriptor) error {
	return &model.Error{Code: "23503", Message: fmt.Sprintf("foreign key constraint %q is violated", constraint.Name)}
}

func encodedValuesEqual(ctx context.Context, encoder storage.KeyEncoder, layout []storage.KeyColumn, left, right []model.Value, maxBytes uint64) (bool, error) {
	if encoder == nil || len(layout) == 0 || len(left) != len(layout) || len(right) != len(layout) {
		return false, errRelationalPolicy
	}
	leftKey, err := encoder.EncodePrefix(ctx, layout, left, maxBytes/2)
	if err != nil {
		return false, err
	}
	rightKey, err := encoder.EncodePrefix(ctx, layout, right, maxBytes/2)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftKey, rightKey), ctx.Err()
}

func (e *relationalExecution) hasReferencingRow(ctx context.Context, child *relationalTable, constraint model.ConstraintDescriptor, values []model.Value) (bool, error) {
	found := false
	err := scanRelationalRows(ctx, e.transaction, child, e.set.encoder, e.maxBytes, func(row *model.Row) error {
		candidate, nulls, err := child.values(constraint.Columns, row)
		if err != nil || nulls != 0 {
			return err
		}
		equal, err := encodedValuesEqual(ctx, e.set.encoder, parentIndexLayoutForSet(e.set, constraint), candidate, values, e.maxBytes)
		if err == nil && equal {
			found = true
			return errStopRelationalScan
		}
		return err
	})
	if errors.Is(err, errStopRelationalScan) {
		return true, nil
	}
	return found, err
}

var errStopRelationalScan = errors.New("relational scan complete")

func parentIndexLayoutForSet(set *relationalSet, constraint model.ConstraintDescriptor) []storage.KeyColumn {
	parent, ok := set.table(constraint.ReferencedObject)
	if !ok {
		return nil
	}
	return parentIndexLayout(parent, constraint)
}

func (e *relationalExecution) applyAction(ctx context.Context, child *relationalTable, constraint model.ConstraintDescriptor, action string, oldValues, replacements []model.Value, depth int) (err error) {
	event := model.TriggerUpdate
	if action == "cascade" && replacements == nil {
		event = model.TriggerDelete
	}
	actionColumns := constraint.Columns
	if event == model.TriggerDelete {
		actionColumns = nil
	} else if action == "set null" || action == "set default" {
		actionColumns = constraint.OnUpdateColumns
		if replacements == nil {
			actionColumns = constraint.OnDeleteColumns
		}
		if len(actionColumns) == 0 {
			actionColumns = constraint.Columns
		}
	}
	started := false
	if e.triggers != nil {
		if err := e.triggers.BeginStatement(ctx, child.descriptor, event, actionColumns); err != nil {
			return err
		}
		started = true
		defer func() {
			if err != nil {
				e.triggers.AbortStatement(child.descriptor, event)
			} else {
				err = e.triggers.FinishStatement(ctx, child.descriptor, event)
			}
		}()
	}
	err = scanRelationalRows(ctx, e.transaction, child, e.set.encoder, e.maxBytes, func(row *model.Row) error {
		candidate, nulls, valueErr := child.values(constraint.Columns, row)
		if valueErr != nil || nulls != 0 {
			return valueErr
		}
		equal, valueErr := encodedValuesEqual(ctx, e.set.encoder, parentIndexLayoutForSet(e.set, constraint), candidate, oldValues, e.maxBytes)
		if valueErr != nil || !equal {
			return valueErr
		}
		visit := relationalVisit{object: child.target.ObjectID, event: event, key: string(row.Key)}
		if _, duplicate := e.visits[visit]; duplicate {
			return nil
		}
		e.visits[visit] = struct{}{}
		if event == model.TriggerDelete {
			return e.deleteCascadeRow(ctx, child, row, depth)
		}
		columns := actionColumns
		values := replacements
		if action == "set null" || action == "set default" {
			if action == "set null" {
				values = make([]model.Value, len(columns))
				for i, column := range columns {
					position := child.position(column)
					if position < 0 {
						return errRelationalPolicy
					}
					values[i] = model.NullVal(child.layout[position].Type.Storage)
				}
			} else {
				values, valueErr = e.transaction.ConstraintDefaultValues(ctx, child.target, columns, e.runtime, e.maxBytes)
				if valueErr != nil {
					return valueErr
				}
			}
		}
		return e.updateCascadeRow(ctx, child, row, columns, values, depth)
	})
	if !started {
		return err
	}
	return err
}

func (e *relationalExecution) deleteCascadeRow(ctx context.Context, child *relationalTable, old *model.Row, depth int) error {
	if e.triggers != nil {
		_, skip, err := e.triggers.BeforeRow(ctx, child.descriptor, model.TriggerDelete, old, nil, e.maxBytes)
		if err != nil || skip {
			return err
		}
	}
	if err := e.transaction.ApplyMutations(ctx, []storage.Mutation{storage.NewDeleteMutation(child.target, old.Key)}); err != nil {
		return err
	}
	if err := child.indexes.Expand(ctx, e.transaction, old, nil); err != nil {
		return err
	}
	if err := e.expand(ctx, child, old, nil, depth); err != nil {
		return err
	}
	if e.triggers != nil {
		return e.triggers.AfterRow(ctx, child.descriptor, model.TriggerDelete, old, nil, e.maxBytes)
	}
	return ctx.Err()
}

func (e *relationalExecution) updateCascadeRow(ctx context.Context, child *relationalTable, old *model.Row, columns []model.ColumnID, values []model.Value, depth int) error {
	candidate, err := e.transaction.PrepareUpdate(ctx, txn.UpdatePreparation{Target: child.target, Previous: old, ColumnIDs: columns, Values: values, Allocator: e.allocator, MaxBytes: e.maxBytes, Expressions: e.runtime})
	if err != nil {
		return err
	}
	proposed, _, err := candidate.CopyRow(ctx, e.maxBytes)
	closeErr := candidate.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if e.triggers != nil {
		triggered, skip, triggerErr := e.triggers.BeforeRow(ctx, child.descriptor, model.TriggerUpdate, old, &proposed, e.maxBytes)
		if triggerErr != nil || skip {
			return triggerErr
		}
		if triggered != nil {
			proposed = *triggered
		}
	}
	assignColumns := make([]model.ColumnID, 0, len(child.layout))
	assignValues := make([]model.Value, 0, len(child.layout))
	for i, column := range child.layout {
		position := slices.IndexFunc(child.table.Columns, func(candidate model.ColumnDescriptor) bool { return candidate.ID == column.ID })
		if position < 0 || child.table.Columns[position].Generated != nil {
			continue
		}
		assignColumns = append(assignColumns, column.ID)
		assignValues = append(assignValues, proposed.Values[i])
	}
	candidate, err = e.transaction.PrepareUpdate(ctx, txn.UpdatePreparation{Target: child.target, Previous: old, ColumnIDs: assignColumns, Values: assignValues, Allocator: e.allocator, MaxBytes: e.maxBytes, Expressions: e.runtime})
	if err != nil {
		return err
	}
	defer candidate.Close()
	current, _, err := candidate.CopyRow(ctx, e.maxBytes)
	if err != nil {
		return err
	}
	if err := e.transaction.ApplyPreparedUpdate(ctx, candidate); err != nil {
		return err
	}
	if err := child.indexes.Expand(ctx, e.transaction, old, &current); err != nil {
		return err
	}
	if err := e.expand(ctx, child, old, &current, depth); err != nil {
		return err
	}
	if e.triggers != nil {
		return e.triggers.AfterRow(ctx, child.descriptor, model.TriggerUpdate, old, &current, e.maxBytes)
	}
	return ctx.Err()
}

func scanRelationalRows(ctx context.Context, transaction txn.ConstraintTransaction, table *relationalTable, encoder storage.KeyEncoder, maxBytes uint64, visit func(*model.Row) error) (err error) {
	span := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	dependency := txn.ReadDependency{Target: table.target, KeyLayout: table.keyLayout, Spans: []storage.KeySpan{span}}
	view, err := transaction.ReadView(ctx, []txn.ReadDependency{dependency})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	ids := make([]model.ColumnID, len(table.layout))
	for i := range table.layout {
		ids[i] = table.layout[i].ID
	}
	request := storage.PushdownRequest{Target: table.target, Layout: table.layout, KeyLayout: table.keyLayout, ColumnIDs: ids, Spans: []storage.KeySpan{span}}
	plan, err := view.PlanScan(ctx, request)
	if plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, view.ReleasePlan(plan)) }()
	}
	if err != nil {
		return err
	}
	cursor, err := view.VectorizedScan(ctx, plan)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	batch := columnar.Batch{MaxRows: 1}
	keyValues := make([]model.Value, len(table.keyLayout))
	for {
		batch.Reset()
		batch.MaxRows = 1
		if err := cursor.Next(ctx, &batch); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := batch.Validate(max(batch.Length, 1)); err != nil {
			return err
		}
		if batch.Rows() > 1 || len(batch.Columns) != len(table.layout) {
			return columnar.ErrInvalidBatch
		}
		if batch.Rows() == 0 {
			continue
		}
		row := model.Row{Values: make([]model.Value, len(table.layout))}
		remaining := maxBytes
		for i := range table.layout {
			value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(0), remaining)
			if err != nil {
				return err
			}
			remaining -= used
			row.Values[i] = value
		}
		for i, key := range table.keyLayout {
			position := table.position(key.ColumnID)
			if position < 0 {
				return errRelationalPolicy
			}
			keyValues[i] = row.Values[position]
		}
		row.Key, err = encoder.EncodePrefix(ctx, table.keyLayout, keyValues, remaining)
		if err != nil {
			return err
		}
		fresh, getErr := transaction.Get(ctx, storage.ReadRequest{Target: table.target, Key: row.Key, Layout: table.layout, ColumnIDs: ids})
		if errors.Is(getErr, storage.ErrRowNotFound) {
			continue
		}
		if getErr != nil {
			return getErr
		}
		if err := visit(&fresh); err != nil {
			return err
		}
	}
}

type indexConstraintEvent struct {
	identity txn.ConstraintIdentity
	target   storage.ObjectRef
	layout   []storage.ColumnLayout
	keys     []storage.KeyColumn
	prefix   []model.Value
	owner    model.ColumnID
	code     string
	message  string
	bytes    uint64
}

func newIndexConstraintEvent(ctx context.Context, identity txn.ConstraintIdentity, binding *indexBinding, prefix []model.Value, code, message string, maxBytes uint64) (*indexConstraintEvent, error) {
	values, used, err := copyConstraintValues(ctx, prefix, maxBytes)
	if err != nil {
		return nil, err
	}
	event := &indexConstraintEvent{identity: identity, target: binding.target, layout: cloneIndexPlanColumns(binding.plan.layout.Columns), keys: binding.plan.KeyLayout(), prefix: values, owner: binding.plan.OwnerColumn(), code: code, message: message}
	event.bytes = used + uint64(unsafe.Sizeof(*event)) + uint64(len(event.layout))*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + uint64(len(event.keys))*uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(len(code)+len(message)+len(identity.Name)+len(identity.Schema))
	if event.bytes > maxBytes {
		return nil, errRelationalLimit
	}
	return event, nil
}

func (e *indexConstraintEvent) Identity() txn.ConstraintIdentity { return e.identity }
func (e *indexConstraintEvent) RetainedBytes() uint64            { return e.bytes }
func (e *indexConstraintEvent) Validate(ctx context.Context, transaction txn.ConstraintTransaction) error {
	count, err := countIndexPrefix(ctx, transaction, e.target, e.layout, e.keys, e.owner, e.prefix, 2)
	if err != nil {
		return err
	}
	if count > 1 {
		return &model.Error{Code: e.code, Message: e.message}
	}
	return ctx.Err()
}

type foreignKeyEvent struct {
	identity       txn.ConstraintIdentity
	childTarget    storage.ObjectRef
	childLayout    []storage.ColumnLayout
	childKeyLayout []storage.KeyColumn
	childColumns   []model.ColumnID
	childKey       []byte
	expected       []model.Value
	parentTarget   storage.ObjectRef
	parentLayout   []storage.ColumnLayout
	parentKeys     []storage.KeyColumn
	parentOwner    model.ColumnID
	encoder        storage.KeyEncoder
	bytes          uint64
	workBytes      uint64
}

func newForeignKeyEvent(ctx context.Context, identity txn.ConstraintIdentity, child *relationalTable, constraint model.ConstraintDescriptor, parent *indexBinding, childKey []byte, expected []model.Value, encoder storage.KeyEncoder, maxBytes uint64) (*foreignKeyEvent, error) {
	values, used, err := copyConstraintValues(ctx, expected, maxBytes)
	if err != nil {
		return nil, err
	}
	event := &foreignKeyEvent{identity: identity, childTarget: child.target, childLayout: cloneIndexPlanColumns(child.layout), childKeyLayout: cloneConstraintKeys(child.keyLayout), childColumns: slices.Clone(constraint.Columns), childKey: bytes.Clone(childKey), expected: values, parentTarget: parent.target, parentLayout: cloneIndexPlanColumns(parent.plan.layout.Columns), parentKeys: parent.plan.KeyLayout(), parentOwner: parent.plan.OwnerColumn(), encoder: encoder, workBytes: maxBytes}
	event.bytes = used + uint64(len(event.childKey)) + uint64(unsafe.Sizeof(*event)) + constraintEventLayoutsBytes(event.childLayout, event.childKeyLayout, event.parentLayout, event.parentKeys) + uint64(len(event.childColumns))*4 + uint64(len(identity.Name)+len(identity.Schema))
	if event.bytes > maxBytes {
		return nil, errRelationalLimit
	}
	return event, nil
}

func (e *foreignKeyEvent) Identity() txn.ConstraintIdentity { return e.identity }
func (e *foreignKeyEvent) RetainedBytes() uint64            { return e.bytes }
func (e *foreignKeyEvent) Validate(ctx context.Context, transaction txn.ConstraintTransaction) (err error) {
	span := storage.KeySpan{Lower: storage.KeyBound{Values: e.expected, Inclusive: true}, Upper: storage.KeyBound{Values: e.expected, Inclusive: true}}
	dependencies := []txn.ReadDependency{
		{Target: e.childTarget, KeyLayout: e.childKeyLayout, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}},
		{Target: e.parentTarget, KeyLayout: e.parentKeys, Spans: []storage.KeySpan{span}},
	}
	view, err := transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	ids := make([]model.ColumnID, len(e.childLayout))
	for i := range e.childLayout {
		ids[i] = e.childLayout[i].ID
	}
	row, err := view.Get(ctx, storage.ReadRequest{Target: e.childTarget, Key: e.childKey, Layout: e.childLayout, ColumnIDs: ids})
	if errors.Is(err, storage.ErrRowNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	actual := make([]model.Value, len(e.childColumns))
	for i, column := range e.childColumns {
		position := sort.Search(len(e.childLayout), func(j int) bool { return e.childLayout[j].ID >= column })
		if position == len(e.childLayout) || e.childLayout[position].ID != column {
			return errRelationalPolicy
		}
		actual[i] = row.Values[position]
		if actual[i].IsNull {
			return nil
		}
	}
	equal, err := encodedValuesEqual(ctx, e.encoder, e.parentKeys[:len(e.expected)], actual, e.expected, e.workBytes)
	if err != nil || !equal {
		return err
	}
	count, err := countIndexPrefixView(ctx, view, e.parentTarget, e.parentLayout, e.parentKeys, e.parentOwner, e.expected, 1)
	if err != nil {
		return err
	}
	if count == 0 {
		return &model.Error{Code: "23503", Message: fmt.Sprintf("foreign key constraint %q is violated", e.identity.Name)}
	}
	return ctx.Err()
}

type referencedKeyEvent struct {
	identity     txn.ConstraintIdentity
	parentTarget storage.ObjectRef
	parentLayout []storage.ColumnLayout
	parentKeys   []storage.KeyColumn
	parentOwner  model.ColumnID
	childTarget  storage.ObjectRef
	childLayout  []storage.ColumnLayout
	childKeys    []storage.KeyColumn
	childColumns []model.ColumnID
	expected     []model.Value
	encoder      storage.KeyEncoder
	bytes        uint64
	workBytes    uint64
}

func newReferencedKeyEvent(ctx context.Context, identity txn.ConstraintIdentity, child, parent *relationalTable, constraint model.ConstraintDescriptor, expected []model.Value, encoder storage.KeyEncoder, maxBytes uint64) (*referencedKeyEvent, error) {
	binding, ok := parent.indexes.index(constraint.ReferencedIndex)
	if !ok {
		return nil, errRelationalPolicy
	}
	values, used, err := copyConstraintValues(ctx, expected, maxBytes)
	if err != nil {
		return nil, err
	}
	event := &referencedKeyEvent{identity: identity, parentTarget: binding.target, parentLayout: cloneIndexPlanColumns(binding.plan.layout.Columns), parentKeys: binding.plan.KeyLayout(), parentOwner: binding.plan.OwnerColumn(), childTarget: child.target, childLayout: cloneIndexPlanColumns(child.layout), childKeys: cloneConstraintKeys(child.keyLayout), childColumns: slices.Clone(constraint.Columns), expected: values, encoder: encoder, workBytes: maxBytes}
	event.bytes = used + uint64(unsafe.Sizeof(*event)) + constraintEventLayoutsBytes(event.parentLayout, event.parentKeys, event.childLayout, event.childKeys) + uint64(len(event.childColumns))*4 + uint64(len(identity.Name)+len(identity.Schema))
	if event.bytes > maxBytes {
		return nil, errRelationalLimit
	}
	return event, nil
}

func (e *referencedKeyEvent) Identity() txn.ConstraintIdentity { return e.identity }
func (e *referencedKeyEvent) RetainedBytes() uint64            { return e.bytes }
func (e *referencedKeyEvent) Validate(ctx context.Context, transaction txn.ConstraintTransaction) error {
	count, err := countIndexPrefix(ctx, transaction, e.parentTarget, e.parentLayout, e.parentKeys, e.parentOwner, e.expected, 1)
	if err != nil || count != 0 {
		return err
	}
	table := &relationalTable{target: e.childTarget, layout: e.childLayout, keyLayout: e.childKeys}
	err = scanRelationalRows(ctx, transaction, table, e.encoder, e.workBytes, func(row *model.Row) error {
		actual := make([]model.Value, len(e.childColumns))
		for i, column := range e.childColumns {
			position := table.position(column)
			if position < 0 {
				return errRelationalPolicy
			}
			actual[i] = row.Values[position]
			if actual[i].IsNull {
				return nil
			}
		}
		equal, err := encodedValuesEqual(ctx, e.encoder, e.parentKeys[:len(e.expected)], actual, e.expected, e.workBytes)
		if err == nil && equal {
			return errStopRelationalScan
		}
		return err
	})
	if errors.Is(err, errStopRelationalScan) {
		return &model.Error{Code: "23503", Message: fmt.Sprintf("foreign key constraint %q is violated", e.identity.Name)}
	}
	return err
}

func countIndexPrefix(ctx context.Context, transaction txn.ConstraintTransaction, target storage.ObjectRef, layout []storage.ColumnLayout, keys []storage.KeyColumn, owner model.ColumnID, prefix []model.Value, limit uint64) (count uint64, err error) {
	span := storage.KeySpan{Lower: storage.KeyBound{Values: prefix, Inclusive: true}, Upper: storage.KeyBound{Values: prefix, Inclusive: true}}
	view, err := transaction.ReadView(ctx, []txn.ReadDependency{{Target: target, KeyLayout: keys, Spans: []storage.KeySpan{span}}})
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	return countIndexPrefixView(ctx, view, target, layout, keys, owner, prefix, limit)
}

func countIndexPrefixView(ctx context.Context, view txn.ReadView, target storage.ObjectRef, layout []storage.ColumnLayout, keys []storage.KeyColumn, owner model.ColumnID, prefix []model.Value, limit uint64) (count uint64, err error) {
	span := storage.KeySpan{Lower: storage.KeyBound{Values: prefix, Inclusive: true}, Upper: storage.KeyBound{Values: prefix, Inclusive: true}}
	request := storage.PushdownRequest{Target: target, Layout: layout, KeyLayout: keys, ColumnIDs: []model.ColumnID{owner}, Spans: []storage.KeySpan{span}, SafeLimit: storage.ScanLimit{Count: limit, Enabled: true}}
	plan, err := view.PlanScan(ctx, request)
	if plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, view.ReleasePlan(plan)) }()
	}
	if err != nil {
		return 0, err
	}
	cursor, err := view.VectorizedScan(ctx, plan)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	batch := columnar.Batch{MaxRows: int(limit)}
	for count < limit {
		batch.Reset()
		batch.MaxRows = int(limit - count)
		if err := cursor.Next(ctx, &batch); err != nil {
			if err == io.EOF {
				return count, nil
			}
			return 0, err
		}
		if len(batch.Columns) != 1 || uint64(batch.Rows()) > limit-count {
			return 0, columnar.ErrInvalidBatch
		}
		count += uint64(batch.Rows())
	}
	return count, nil
}

func copyConstraintValues(ctx context.Context, source []model.Value, maxBytes uint64) ([]model.Value, uint64, error) {
	fixed := uint64(len(source)) * uint64(unsafe.Sizeof(model.Value{}))
	if fixed > maxBytes {
		return nil, 0, errRelationalLimit
	}
	values := make([]model.Value, len(source))
	used := fixed
	for i, value := range source {
		owned, size, err := model.CopyValue(ctx, value, maxBytes-used)
		if err != nil {
			return nil, 0, err
		}
		used += size
		values[i] = owned
	}
	return values, used, ctx.Err()
}

func cloneConstraintKeys(keys []storage.KeyColumn) []storage.KeyColumn {
	copy, _, err := storage.CopyKeyLayout(context.Background(), keys, ^uint64(0))
	if err != nil {
		panic(err)
	}
	return copy
}

func constraintEventLayoutsBytes(left []storage.ColumnLayout, leftKeys []storage.KeyColumn, right []storage.ColumnLayout, rightKeys []storage.KeyColumn) uint64 {
	return uint64(len(left)+len(right))*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + uint64(len(leftKeys)+len(rightKeys))*uint64(unsafe.Sizeof(storage.KeyColumn{}))
}
