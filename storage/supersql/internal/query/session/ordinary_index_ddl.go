package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runCreateIndex(ctx context.Context, statement *zpgparse.IndexStmt, options ObjectCommandOptions, cascade *TypeCascadeOptions, invocation StatementExecution) (result ExecutionResult, err error) {
	if transactions == nil || statement == nil || transactions.sequences == nil {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
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
	err = transactions.routineAtomicStatement(operation, func(ctx context.Context) (statementErr error) {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute object DDL in a read-only transaction")
		}
		temporaryMark := len(transactions.named.temporaryChanges)
		defer func() {
			if statementErr != nil {
				transactions.named.rollbackTemporary(temporaryMark)
			}
		}()
		options.Binding.Bind.SearchPath = declarationSearchPath(transactions.settings.SearchPath(), options.Binding.SchemaContext)
		options.Binding.Owner = transactions.settings.Principal().OID()
		options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = transactions.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
		options.Binding.Encoder = transactions.options.KeyEncoder
		if len(transactions.named.temporary) != 0 && !slices.Contains(options.Binding.Bind.SearchPath, temporarySchemaName) {
			options.Binding.Bind.SearchPath = append([]string{temporarySchemaName}, options.Binding.Bind.SearchPath...)
		}
		bound, err := transactions.bindCreateIndexTransactional(ctx, statement, options)
		if err != nil {
			return err
		}
		if hierarchy := bound.HierarchyIndexCommand(); hierarchy != nil {
			result.Tag, result.Warnings = hierarchy.CommandTag(), hierarchy.Warnings()
			if len(hierarchy.Creates()) == 0 && len(hierarchy.Updates()) == 0 {
				if len(result.Warnings) == 0 {
					return model.ErrInvalidDescriptor
				}
				return rejectPreparedEnvironment(invocation.Execution.Environment, nil)
			}
			if !hierarchy.Populate() {
				if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
					return err
				}
			}
			temporary := objectCommandTemporary(ctx, hierarchy, options.Binding.Descriptors)
			if temporary {
				transactions.named.temporaryOptions = &options
				if err := transactions.named.applyTemporaryCommand(hierarchy); err != nil {
					return err
				}
				if err := transactions.stageTemporaryObjectCommand(ctx, hierarchy, options); err != nil {
					return err
				}
			} else {
				if err := transactions.lockObjectCommandDescriptors(ctx, hierarchy); err != nil {
					return err
				}
				if err := transactions.stageObjectCommand(ctx, hierarchy, options); err != nil {
					return err
				}
			}
			if hierarchy.Populate() {
				environment := &invocation.Execution.Environment
				if environment.Query == nil || invocation.Execution.Catalog == nil {
					return errors.Join(errPreparedInvocation, rejectPreparedEnvironment(*environment, nil))
				}
				environment.Builtin = transactions.settings.BuiltinOptions(environment.Builtin.Collation, environment.Builtin.DefaultCollation)
				resources := transactions.settings.Resources()
				if err := environment.Query.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); err != nil {
					return rejectPreparedEnvironment(*environment, err)
				}
				environment.Settings = transactions.settingRuntime(ctx)
				environment.Catalog, err = transactions.attachCatalogSettings(ctx, environment.Catalog)
				if err != nil {
					return rejectPreparedEnvironment(*environment, err)
				}
				environment.Temporal = transactions.temporal
				affected, populateErr := transactions.populateHierarchyIndexes(ctx, hierarchy, invocation)
				result.RowsAffected = affected
				return rejectPreparedEnvironment(*environment, populateErr)
			}
			return nil
		}
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return err
		}
		result.Tag, result.Warnings = "CREATE INDEX", bound.Warnings()
		if bound.MetadataOnly() != nil {
			return nil
		}
		if cascade == nil || cascade.Policies == nil {
			return settingError("22023", "transactional index build services are not configured")
		}
		command, err := bound.TransactionalIndexCommand()
		if err != nil {
			return err
		}
		return transactions.stageGenerationRewrite(ctx, bound, command, options, *cascade)
	})
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

func (transactions *Transactions) bindCreateIndexTransactional(ctx context.Context, statement *zpgparse.IndexStmt, options ObjectCommandOptions) (*bind.BoundSchemaDDL, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	compiler, err := NewIndexExpressionCompiler(snapshot, options.Indexes)
	if err == nil {
		var bound *bind.BoundSchemaDDL
		bound, err = bind.BindCreateIndexTransactional(ctx, statement, snapshot, options.Binding, compiler)
		return bound, errors.Join(err, snapshot.Close(), view.Close())
	}
	return nil, errors.Join(err, snapshot.Close(), view.Close())
}
