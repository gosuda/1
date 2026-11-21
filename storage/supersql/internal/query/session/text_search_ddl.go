package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runTextSearchCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil {
		return ExecutionResult{}, false, nil
	}
	define := statement.Stmt.GetDefineStmt()
	create := define != nil && (define.Kind == zpgparse.ObjectType_OBJECT_TSDICTIONARY || define.Kind == zpgparse.ObjectType_OBJECT_TSCONFIGURATION)
	dictionary := statement.Stmt.GetAlterTsdictionaryStmt()
	configuration := statement.Stmt.GetAlterTsconfigurationStmt()
	if !create && dictionary == nil && configuration == nil {
		return ExecutionResult{}, false, nil
	}
	if options.Execution.TextSearchDDL == nil {
		return ExecutionResult{}, true, errPreparedInvocation
	}
	if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "text search DDL does not accept parameters"), rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	if err := rejectPreparedEnvironment(options.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	var result TableCommandResult
	var err error
	switch {
	case create:
		result, err = ExecuteCreateTextSearch(ctx, transactions, define, *options.Execution.TextSearchDDL)
	case dictionary != nil:
		result, err = ExecuteAlterTextSearchDictionary(ctx, transactions, dictionary, *options.Execution.TextSearchDDL)
	default:
		result, err = ExecuteAlterTextSearchConfiguration(ctx, transactions, configuration, *options.Execution.TextSearchDDL)
	}
	return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
}

func ExecuteCreateTextSearch(ctx context.Context, transactions *Transactions, statement *zpgparse.DefineStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing CREATE TEXT SEARCH declaration")
	}
	var tag string
	switch statement.Kind {
	case zpgparse.ObjectType_OBJECT_TSDICTIONARY:
		tag = "CREATE TEXT SEARCH DICTIONARY"
	case zpgparse.ObjectType_OBJECT_TSCONFIGURATION:
		tag = "CREATE TEXT SEARCH CONFIGURATION"
	default:
		return TableCommandResult{}, settingError("42809", "declaration is not a native text search dictionary or configuration")
	}
	return executeTableDeclaration(ctx, transactions, options, tag, false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindCreateTextSearch(ctx, statement, snapshot, declaration)
	})
}

func ExecuteAlterTextSearchDictionary(ctx context.Context, transactions *Transactions, statement *zpgparse.AlterTSDictionaryStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing ALTER TEXT SEARCH DICTIONARY")
	}
	return executeTableDeclaration(ctx, transactions, options, "ALTER TEXT SEARCH DICTIONARY", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindAlterTextSearchDictionary(ctx, statement, snapshot, declaration)
	})
}

func ExecuteAlterTextSearchConfiguration(ctx context.Context, transactions *Transactions, statement *zpgparse.AlterTSConfigurationStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing ALTER TEXT SEARCH CONFIGURATION")
	}
	return executeTableDeclaration(ctx, transactions, options, "ALTER TEXT SEARCH CONFIGURATION", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindAlterTextSearchConfiguration(ctx, statement, snapshot, declaration)
	})
}

// ExecuteBootstrapNativeTextSearch atomically publishes the initdb-defined
// Snowball objects using durable catalog identity allocation.
func ExecuteBootstrapNativeTextSearch(ctx context.Context, transactions *Transactions, options TableCommandOptions) (TableCommandResult, error) {
	return executeTableDeclaration(ctx, transactions, options, "BOOTSTRAP TEXT SEARCH", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindBootstrapNativeTextSearch(ctx, snapshot, declaration)
	})
}
