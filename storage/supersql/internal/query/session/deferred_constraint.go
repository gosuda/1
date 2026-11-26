package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type deferredRelationalConstraint struct {
	event txn.DeferredConstraint
	bytes uint64
}

// DeferConstraint implements txn.ConstraintDeferral. A true result transfers
// ownership of event to the active transaction.
func (transactions *Transactions) DeferConstraint(ctx context.Context, event txn.DeferredConstraint) (bool, error) {
	operation, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation)
	if !ok || operation.owner != transactions || !operation.active.Load() {
		return false, errSessionTransactionReentry
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if transactions.transaction == nil || transactions.transaction.State() != txn.Active || event == nil {
		return false, errSessionTransactionState
	}
	identity := event.Identity()
	if identity.Object == 0 || identity.Constraint == 0 || identity.Name == "" {
		return false, errSessionTransactionState
	}
	if !identity.Deferrable {
		return false, nil
	}
	deferred := identity.InitiallyDeferred
	if explicit, exists := namedConstraintMode(transactions.constraintModes.named, constraintModeID{Object: identity.Object, Constraint: identity.Constraint}); exists {
		deferred = explicit
	} else if transactions.constraintModes.all != nil {
		deferred = *transactions.constraintModes.all
	}
	if !deferred {
		return false, nil
	}
	retained := event.RetainedBytes()
	limit := uint64(transactions.settings.Resources().EffectiveWorkMemoryBytes)
	if retained == 0 || retained > limit || transactions.deferredConstraintBytes > limit-retained {
		return false, model.ErrValueBudget
	}
	transactions.deferredConstraints = append(transactions.deferredConstraints, deferredRelationalConstraint{event: event, bytes: retained})
	transactions.deferredConstraintBytes += retained
	return true, nil
}

func (transactions *Transactions) flushDeferredConstraintNames(ctx context.Context, names map[constraintModeID]struct{}, all bool) error {
	if len(transactions.deferredConstraints) == 0 {
		return nil
	}
	if transactions.transaction == nil || transactions.transaction.State() != txn.Active {
		return errSessionTransactionState
	}
	originalCount := len(transactions.deferredConstraints)
	selected := make([]deferredRelationalConstraint, 0, originalCount)
	remaining := make([]deferredRelationalConstraint, 0, originalCount)
	keptBefore := make([]int, originalCount+1)
	for i, pending := range transactions.deferredConstraints[:originalCount] {
		identity := pending.event.Identity()
		_, match := names[constraintModeID{Object: identity.Object, Constraint: identity.Constraint}]
		if all || match {
			selected = append(selected, pending)
		} else {
			remaining = append(remaining, pending)
			keptBefore[i+1]++
		}
		keptBefore[i+1] += keptBefore[i]
	}
	for _, mark := range transactions.deferredConstraintMarks {
		if mark < 0 || mark > originalCount {
			return errSessionTransactionState
		}
	}
	if len(selected) == 0 {
		return nil
	}
	if err := transactions.transaction.Statement(ctx, func(statement context.Context) error {
		for _, pending := range selected {
			if err := pending.event.Validate(statement, transactions.transaction); err != nil {
				return relationalConstraintError(err, pending.event.Identity())
			}
		}
		return nil
	}); err != nil {
		return err
	}
	added := append([]deferredRelationalConstraint(nil), transactions.deferredConstraints[originalCount:]...)
	clear(transactions.deferredConstraints)
	transactions.deferredConstraints = append(remaining, added...)
	transactions.deferredConstraintBytes = 0
	for _, pending := range transactions.deferredConstraints {
		if ^uint64(0)-transactions.deferredConstraintBytes < pending.bytes {
			return errSessionTransactionState
		}
		transactions.deferredConstraintBytes += pending.bytes
	}
	for i, mark := range transactions.deferredConstraintMarks {
		transactions.deferredConstraintMarks[i] = keptBefore[mark]
	}
	return nil
}

func relationalConstraintError(err error, identity txn.ConstraintIdentity) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Constraint != "" {
		return err
	}
	copy := *typed
	copy.Constraint = identity.Name
	return &copy
}

func (transactions *Transactions) bindConstraintModeCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (prepared *Prepared, err error) {
	if !transactions.active || transactions.transaction == nil {
		return nil, settingError("25P01", "SET CONSTRAINTS can only be used in transaction blocks")
	}
	if options.Execution.Catalog == nil || options.Planning == nil {
		return nil, &model.Error{Code: "55000", Message: "SET CONSTRAINTS requires catalog planning"}
	}
	builtins := transactions.settings.BuiltinOptions(options.Execution.Environment.Builtin.Collation, options.Execution.Environment.Builtin.DefaultCollation)
	dependencies := options.Execution.Dependencies
	if dependencies == nil {
		// Binding only reads the catalog; the broad span covers name
		// resolution and descriptor reads a public caller cannot pre-enumerate.
		dependencies = catalogLayoutDependencies(options.Execution.Catalog)
	}
	err = transactions.transaction.Statement(ctx, func(operation context.Context) (statementErr error) {
		view, statementErr := transactions.transaction.ReadView(operation, dependencies)
		if statementErr != nil {
			return statementErr
		}
		defer func() { statementErr = errors.Join(statementErr, view.Close()) }()
		snapshot, planning, statementErr := transactions.statementPlanning(operation, view, builtins, options)
		if statementErr != nil {
			return statementErr
		}
		defer func() { statementErr = errors.Join(statementErr, snapshot.Close()) }()
		bound, statementErr := bind.Bind(operation, statement, snapshot, planning.Bind, nil)
		if statementErr == nil {
			prepared = &Prepared{bound: bound, requiredTimestamp: transactions.historical}
		}
		return statementErr
	})
	return prepared, err
}
