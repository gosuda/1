package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) bindTableLockCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (prepared *Prepared, err error) {
	if !transactions.active || transactions.transaction == nil {
		return nil, settingError("25P01", "LOCK TABLE can only be used in transaction blocks")
	}
	if options.Execution.Catalog == nil || options.Planning == nil {
		return nil, &model.Error{Code: "55000", Message: "LOCK TABLE requires catalog planning"}
	}
	builtins := transactions.settings.BuiltinOptions(options.Execution.Environment.Builtin.Collation, options.Execution.Environment.Builtin.DefaultCollation)
	dependencies := options.Execution.Dependencies
	if dependencies == nil {
		// Binding only reads the catalog; the broad span covers name
		// resolution and descriptor reads a public caller cannot pre-enumerate.
		dependencies = catalogLayoutDependencies(options.Execution.Catalog)
	}
	err = transactions.routineAtomicStatement(ctx, func(operation context.Context) (statementErr error) {
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
