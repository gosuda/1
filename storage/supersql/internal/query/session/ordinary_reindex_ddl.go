package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runReindex(ctx context.Context, statement *zpgparse.ReindexStmt, options ObjectCommandOptions, cascade *TypeCascadeOptions) (result ExecutionResult, err error) {
	if transactions == nil || statement == nil || transactions.sequences == nil || options.MaxDescriptors == 0 {
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
			if automatic && transactions.active {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	var hierarchy []model.ObjectID
	var expected map[reindexDependency]uint64
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
		configureReindexBinding(transactions, &options)
		bound, err := transactions.bindReindexTransactional(ctx, statement, options)
		if err != nil {
			return err
		}
		if bound.NoOp() {
			result.Tag, result.Warnings = "REINDEX", bound.Warnings()
			return nil
		}
		if cascade == nil || cascade.Policies == nil {
			return settingError("22023", "transactional index rebuild services are not configured")
		}
		if bound.ReindexHierarchy() {
			if !automatic {
				return settingError("25001", "REINDEX of a partitioned relation cannot run inside a transaction block")
			}
			hierarchy = reindexDefinitionTables(bound.Definitions())
			expected = reindexDependencies(bound.Dependencies())
			result.Tag, result.Warnings = "REINDEX", bound.Warnings()
			return nil
		}
		command, err := bound.TransactionalReindexCommand()
		if err != nil {
			return err
		}
		result.Tag, result.Warnings = command.CommandTag(), command.Warnings()
		return transactions.stageGenerationRewrite(ctx, bound, command, options, *cascade)
	})
	if err == nil && len(hierarchy) != 0 {
		err = transactions.endRollback(context.WithoutCancel(operation))
		if err == nil {
			result.Commit, err = transactions.runReindexHierarchy(operation, statement, options, *cascade, hierarchy, expected)
		}
		complete = true
		return result, err
	}
	err = errors.Join(err, transactions.markDataAccess())
	complete = true
	if err != nil {
		transactions.markFailed(err)
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
	}
	return result, err
}

type reindexDependency struct {
	object model.ObjectID
	column model.ColumnID
}

func configureReindexBinding(transactions *Transactions, options *ObjectCommandOptions) {
	options.Binding.Bind.SearchPath = declarationSearchPath(transactions.settings.SearchPath(), options.Binding.SchemaContext)
	options.Binding.Owner = transactions.settings.Principal().OID()
	options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
	options.Binding.Bind.Temporal = transactions.temporal
	options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
	options.Binding.Encoder = transactions.options.KeyEncoder
	if len(transactions.named.temporary) != 0 && !slices.Contains(options.Binding.Bind.SearchPath, temporarySchemaName) {
		options.Binding.Bind.SearchPath = append([]string{temporarySchemaName}, options.Binding.Bind.SearchPath...)
	}
}

func reindexDefinitionTables(definitions []schema.JobDefinition) []model.ObjectID {
	result := make([]model.ObjectID, len(definitions))
	for i, definition := range definitions {
		if definition.SourceTable != nil {
			result[i] = definition.SourceTable.ID()
		}
	}
	return result
}

func reindexDependencies(dependencies []model.Dependency) map[reindexDependency]uint64 {
	result := make(map[reindexDependency]uint64, len(dependencies))
	for _, dependency := range dependencies {
		result[reindexDependency{object: dependency.Object, column: dependency.Column}] = dependency.Version
	}
	return result
}

func sameReindexTopology(bound *bind.BoundSchemaDDL, tables []model.ObjectID, expected map[reindexDependency]uint64) bool {
	if bound == nil || !slices.Equal(reindexDefinitionTables(bound.Definitions()), tables) {
		return false
	}
	current := reindexDependencies(bound.Dependencies())
	if len(current) != len(expected) {
		return false
	}
	for identity, version := range expected {
		if current[identity] != version {
			return false
		}
	}
	return true
}

func (transactions *Transactions) runReindexHierarchy(ctx context.Context, statement *zpgparse.ReindexStmt, options ObjectCommandOptions, cascade TypeCascadeOptions, tables []model.ObjectID, expected map[reindexDependency]uint64) (result EndResult, err error) {
	for _, table := range tables {
		if err := transactions.begin(ctx, TransactionStart{}); err != nil {
			return result, err
		}
		var command *bind.BoundObjectCommand
		err = transactions.routineAtomicStatement(ctx, func(ctx context.Context) error {
			configureReindexBinding(transactions, &options)
			bound, bindErr := transactions.bindReindexTransactional(ctx, statement, options)
			if bindErr != nil {
				return bindErr
			}
			if !sameReindexTopology(bound, tables, expected) {
				return catalog.ErrDescriptorChanged
			}
			selected, selectErr := bound.SelectReindexDefinition(table)
			if selectErr != nil {
				return selectErr
			}
			command, selectErr = selected.TransactionalReindexCommand()
			if selectErr != nil {
				return selectErr
			}
			return transactions.stageGenerationRewrite(ctx, selected, command, options, cascade)
		})
		err = errors.Join(err, transactions.markDataAccess())
		if err != nil {
			transactions.markFailed(err)
			return result, errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		result, err = transactions.commit(ctx, false)
		if err != nil {
			return result, err
		}
		for _, update := range command.Updates() {
			for identity := range expected {
				if identity.object == update.Base.ID() {
					expected[identity] = update.Next.Version()
				}
			}
		}
	}
	return result, nil
}

func (transactions *Transactions) bindReindexTransactional(ctx context.Context, statement *zpgparse.ReindexStmt, options ObjectCommandOptions) (*bind.BoundSchemaDDL, error) {
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
	bound, bindErr := bind.BindReindexTransactional(ctx, statement, snapshot, options.Binding)
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}
