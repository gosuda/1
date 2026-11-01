package session

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
)

func (transactions *Transactions) flushDeferredTriggerNames(ctx context.Context, names map[constraintModeID]struct{}, all bool) (err error) {
	if len(transactions.deferredTriggers) == 0 {
		return nil
	}
	if transactions.transaction == nil || transactions.transaction.State() != txn.Active {
		return errSessionTransactionState
	}
	originalCount := len(transactions.deferredTriggers)
	selected := make([]deferredRoutineTrigger, 0, originalCount)
	remaining := make([]deferredRoutineTrigger, 0, originalCount)
	keptBefore := make([]int, originalCount+1)
	for i, pending := range transactions.deferredTriggers[:originalCount] {
		_, match := names[constraintModeID{Object: pending.trigger.Context.ConstraintTable, Constraint: pending.trigger.Context.ConstraintID}]
		if all || match {
			selected = append(selected, pending)
		} else {
			remaining = append(remaining, pending)
			keptBefore[i+1]++
		}
		keptBefore[i+1] += keptBefore[i]
	}
	for _, mark := range transactions.deferredMarks {
		if mark < 0 || mark > originalCount {
			return errSessionTransactionState
		}
	}
	if len(selected) == 0 {
		return nil
	}
	if err := transactions.transaction.Statement(ctx, func(statement context.Context) (runErr error) {
		view, err := transactions.transaction.ReadView(statement, nil)
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, view.Close()) }()
		scope := &preparedMutationScope{transaction: transactions.transaction, ambient: true}
		scope.active.Store(true)
		defer scope.active.Store(false)
		statement = context.WithValue(statement, preparedScopeKey{}, scope)
		for _, pending := range selected {
			if pending.trigger.Program == nil || pending.catalog == nil || pending.options.Planning == nil || pending.options.NewQuery == nil {
				return errPreparedInvocation
			}
			current, exists, err := transactions.currentDeferredTrigger(statement, view, pending)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			pending.trigger.Context.Name = current.Name
			pending.trigger.Context.SchemaName = current.SchemaName
			configured := pending.options.Capabilities
			settings := transactions.settingRuntime(statement)
			executor := &routineExecutor{
				transactions: transactions, transaction: transactions.transaction, view: view, scope: scope,
				environment: pending.environment, template: pending.environment, execution: pending.execution,
				sourceCatalog: pending.sourceCatalog, catalog: pending.catalog, options: &pending.options,
				settings: settings, catalogSettings: settings,
				baseFunctions: configured.BaseFunctions, baseBuiltin: configured.BaseBuiltin, baseKeys: configured.BaseKeys,
				definitions: append([]procedural.Definition(nil), pending.definitions...),
			}
			frame, buildErr := executor.buildCapabilityFrame(statement, view, false)
			if buildErr != nil {
				return buildErr
			}
			executeErr := func() (err error) {
				executor.installFrame(frame)
				defer func() { err = errors.Join(err, executor.closeCapabilities()) }()
				invocation := procedural.WithSQLExecutor(statement, executor)
				_, _, err = pending.trigger.Program.ExecuteTrigger(invocation, pending.trigger.Context)
				return err
			}()
			if executeErr != nil {
				return executeErr
			}
		}
		return nil
	}); err != nil {
		return err
	}
	added := slices.Clone(transactions.deferredTriggers[originalCount:])
	clear(transactions.deferredTriggers)
	transactions.deferredTriggers = append(remaining, added...)
	for i, mark := range transactions.deferredMarks {
		transactions.deferredMarks[i] = keptBefore[mark]
	}
	return nil
}
func (transactions *Transactions) currentDeferredTrigger(ctx context.Context, view txn.ReadView, pending deferredRoutineTrigger) (model.TriggerDescriptor, bool, error) {
	tableID := pending.trigger.Context.ConstraintTable
	constraintID := pending.trigger.Context.ConstraintID
	if tableID == 0 || constraintID == 0 {
		return model.TriggerDescriptor{}, false, model.ErrInvalidDescriptor
	}
	var descriptor *model.Descriptor
	if temporary := transactions.named.temporary[tableID]; temporary != nil {
		descriptor = temporary
	} else {
		snapshot, err := pending.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return model.TriggerDescriptor{}, false, err
		}
		published, resolveErr := snapshot.Descriptor(ctx, tableID)
		closeErr := snapshot.Close()
		if resolveErr != nil {
			if errors.Is(resolveErr, catalog.ErrUnknownObject) {
				return model.TriggerDescriptor{}, false, closeErr
			}
			return model.TriggerDescriptor{}, false, errors.Join(resolveErr, closeErr)
		}
		if closeErr != nil {
			return model.TriggerDescriptor{}, false, closeErr
		}
		descriptor = published.Descriptor
	}
	if descriptor == nil || descriptor.State() != model.StatePublic {
		return model.TriggerDescriptor{}, false, nil
	}
	spec, err := descriptor.Spec(ctx)
	if err != nil {
		return model.TriggerDescriptor{}, false, err
	}
	var triggers []model.TriggerDescriptor
	if spec.Table != nil {
		triggers = spec.Table.Triggers
	} else if spec.View != nil {
		triggers = spec.View.Triggers
	} else {
		return model.TriggerDescriptor{}, false, model.ErrInvalidDescriptor
	}
	for _, trigger := range triggers {
		if trigger.ConstraintID == constraintID {
			return trigger, trigger.EnableMode.Fires(false), nil
		}
	}
	return model.TriggerDescriptor{}, false, nil
}

func namedConstraintMode(modes map[constraintModeID]bool, id constraintModeID) (bool, bool) {
	mode, found := modes[id]
	return mode, found
}

func cloneConstraintModes(source constraintModeState) constraintModeState {
	target := constraintModeState{}
	if source.all != nil {
		value := *source.all
		target.all = &value
	}
	target.named = maps.Clone(source.named)
	return target
}

func (executor *routineExecutor) TriggerDeferred(ctx context.Context, trigger model.TriggerDescriptor) (bool, error) {
	if err := executor.scope.check(ctx); err != nil {
		return false, err
	}
	if !trigger.Constraint {
		return false, nil
	}
	mode := trigger.InitiallyDeferred
	if explicit, ok := namedConstraintMode(executor.transactions.constraintModes.named, constraintModeID{Object: trigger.ConstraintTable, Constraint: trigger.ConstraintID}); ok {
		mode = explicit
	} else if executor.transactions.constraintModes.all != nil {
		mode = *executor.transactions.constraintModes.all
	}
	if mode && !trigger.Deferrable {
		return false, &model.Error{Code: "55000", Message: "constraint is not deferrable"}
	}
	return mode, nil
}

func (transactions *Transactions) selectedDeferredEvents(names map[constraintModeID]struct{}, all bool) bool {
	if all {
		return len(transactions.deferredConstraints) != 0 || len(transactions.deferredTriggers) != 0
	}
	for _, pending := range transactions.deferredConstraints {
		identity := pending.event.Identity()
		if _, selected := names[constraintModeID{Object: identity.Object, Constraint: identity.Constraint}]; selected {
			return true
		}
	}
	for _, pending := range transactions.deferredTriggers {
		if _, selected := names[constraintModeID{Object: pending.trigger.Context.ConstraintTable, Constraint: pending.trigger.Context.ConstraintID}]; selected {
			return true
		}
	}
	return false
}

func (transactions *Transactions) flushDeferredEvents(ctx context.Context, names map[constraintModeID]struct{}, all bool) error {
	for transactions.selectedDeferredEvents(names, all) {
		if err := transactions.flushDeferredConstraintNames(ctx, names, all); err != nil {
			return err
		}
		if err := transactions.flushDeferredTriggerNames(ctx, names, all); err != nil {
			return err
		}
	}
	return nil
}

func (transactions *Transactions) executeConstraintMode(ctx context.Context, command bind.ConstraintModeCommand) (ExecutionResult, error) {
	result := ExecutionResult{Tag: "SET CONSTRAINTS"}
	if !transactions.active {
		result.Warnings = append(result.Warnings, model.Error{Code: "25P01", Message: "SET CONSTRAINTS can only be used in transaction blocks"})
		return result, nil
	}
	if command.All {
		if len(command.Names) != 0 {
			return ExecutionResult{}, errSessionTransactionState
		}
	} else if len(command.Names) == 0 {
		return ExecutionResult{}, errSessionTransactionState
	}
	before := cloneConstraintModes(transactions.constraintModes)
	names := make(map[constraintModeID]struct{}, len(command.Names))
	if command.All {
		value := command.Deferred
		transactions.constraintModes.all = &value
		transactions.constraintModes.named = nil
	} else {
		if transactions.constraintModes.named == nil {
			transactions.constraintModes.named = make(map[constraintModeID]bool, len(command.Names))
		}
		for _, resolved := range command.Names {
			id := constraintModeID{Object: resolved.Object, Constraint: resolved.Constraint}
			if id.Object == 0 || id.Constraint == 0 {
				return ExecutionResult{}, errSessionTransactionState
			}
			names[id] = struct{}{}
			transactions.constraintModes.named[id] = command.Deferred
		}
	}
	if command.Deferred {
		return result, nil
	}
	all := command.All
	deferredConstraints := slices.Clone(transactions.deferredConstraints)
	deferredConstraintMarks := slices.Clone(transactions.deferredConstraintMarks)
	deferredConstraintBytes := transactions.deferredConstraintBytes
	deferredTriggers := slices.Clone(transactions.deferredTriggers)
	deferredMarks := slices.Clone(transactions.deferredMarks)
	restore := func() {
		clear(transactions.deferredConstraints)
		transactions.deferredConstraints = deferredConstraints
		transactions.deferredConstraintMarks = deferredConstraintMarks
		transactions.deferredConstraintBytes = deferredConstraintBytes
		clear(transactions.deferredTriggers)
		transactions.deferredTriggers = deferredTriggers
		transactions.deferredMarks = deferredMarks
	}
	if err := transactions.flushDeferredEvents(ctx, names, all); err != nil {
		restore()
		transactions.constraintModes = before
		return ExecutionResult{}, err
	}
	return result, nil
}
