package session

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// IsDefinitionCommand reports whether the statement belongs to the SQL-defined
// cast/operator/aggregate family.
func IsDefinitionCommand(node *zpgparse.Node) bool {
	return bind.IsDefinitionCommand(node)
}

// runDefinitionCommand executes CREATE/DROP/ALTER/RENAME/COMMENT for SQL-defined
// casts, operators, and aggregates. The objects are real catalog descriptors:
// identity, namespace, and dependency validation happen during binding, and the
// resulting changes are staged into the caller transaction so rollback and
// savepoints behave like every other ordinary object DDL.
func (transactions *Transactions) runDefinitionCommand(ctx context.Context, statement *zpgparse.RawStmt, invocation StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil || !bind.IsDefinitionCommand(statement.Stmt) {
		return ExecutionResult{}, false, nil
	}
	if invocation.Execution.ObjectDDL == nil {
		return ExecutionResult{}, true, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if len(invocation.Parameters) != 0 || len(invocation.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "definition command does not accept parameters"), rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	options := *invocation.Execution.ObjectDDL
	result, err := ExecuteDefinitionCommand(ctx, transactions, statement.Stmt, options)
	return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
}

// DefinitionCommandResult carries the command tag, warnings, and autocommit
// outcome for one definition command.
type DefinitionCommandResult struct {
	Tag      string
	Warnings []model.Error
	Commit   EndResult
}

// ExecuteDefinitionCommand stages cast/operator/aggregate declarations and
// lifecycle mutations under one statement boundary. An autocommit statement
// begins and commits its own transaction; an explicit transaction retains the
// staged mutations until its own COMMIT or ROLLBACK.
func ExecuteDefinitionCommand(ctx context.Context, transactions *Transactions, node *zpgparse.Node, options ObjectCommandOptions) (result DefinitionCommandResult, err error) {
	if transactions == nil || node == nil || options.MaxDescriptors == 0 {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if transactions.sequences == nil {
		return result, transactions.fail(settingError("0A000", "catalog DDL services are not configured"))
	}
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(operation, TransactionStart{}); err != nil {
			return result, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	complete := false
	defer func() {
		if !complete {
			transactions.markFailed(errSessionStatementPanic)
			if automatic {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	var bound *bind.BoundObjectCommand
	err = transactions.routineAtomicStatement(operation, func(ctx context.Context) error {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute definition DDL in a read-only transaction")
		}
		options.Binding.Bind.SearchPath = declarationSearchPath(transactions.settings.SearchPath(), options.Binding.SchemaContext)
		options.Binding.Owner = transactions.settings.Principal().OID()
		options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = transactions.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
		options.Binding.Encoder = transactions.options.KeyEncoder
		options.Binding.CurrentSetting = func(name string) (string, error) {
			value, err := transactions.settings.Show(name)
			if err != nil {
				return "", err
			}
			if value.IsNull || value.Type != model.TypeString {
				return "", settingError("22023", "session setting has no textual representation")
			}
			return value.StrVal, nil
		}
		bindErr := transactions.bindDefinitionCommand(ctx, node, options, &bound)
		if bindErr == nil {
			result.Tag, result.Warnings = bound.CommandTag(), bound.Warnings()
		}
		return bindErr
	})
	if err == nil {
		if len(bound.Creates()) == 0 && len(bound.Updates()) == 0 && len(bound.Drops()) == 0 {
			if len(bound.Warnings()) == 0 {
				err = model.ErrInvalidDescriptor
			}
		} else {
			if err = transactions.lockObjectCommandDescriptors(operation, bound); err == nil {
				err = transactions.stageObjectCommand(operation, bound, options)
			}
		}
	}
	err = errors.Join(err, transactions.markDataAccess())
	complete = true
	if err != nil {
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
	}
	return result, err
}

func (transactions *Transactions) bindDefinitionCommand(ctx context.Context, node *zpgparse.Node, options ObjectCommandOptions, bound **bind.BoundObjectCommand) error {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return errors.Join(err, snapshot.Close(), view.Close())
	}
	// Definition namespaces are read through the same pinned snapshot the binder
	// already depends on; no separate admission step is needed because these
	// descriptors carry no physical generation.
	command, handled, bindErr := bind.BindDefinitionCommand(ctx, proto.Clone(node).(*zpgparse.Node), snapshot, options.Binding)
	if !handled && bindErr == nil {
		bindErr = errPreparedInvocation
	}
	if command != nil {
		*bound = command
	}
	return errors.Join(bindErr, snapshot.Close(), view.Close())
}
