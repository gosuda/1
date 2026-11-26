package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runTruncate(ctx context.Context, statement *zpgparse.TruncateStmt, options ObjectCommandOptions, invocation StatementExecution) (result ExecutionResult, err error) {
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
	sequenceMark := len(transactions.sequences.changes)
	temporaryMark := len(transactions.named.temporaryChanges)
	complete := false
	defer func() {
		if !complete {
			transactions.sequences.abandon(sequenceMark)
			transactions.markFailed(errSessionStatementPanic)
			if automatic {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	var bound *bind.BoundTruncate
	err = transactions.routineAtomicStatement(operation, func(statementContext context.Context) (statementErr error) {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute TRUNCATE in a read-only transaction")
		}
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
		bound, statementErr = transactions.bindTruncateTransactional(statementContext, statement, options)
		if statementErr == nil {
			result.Tag, result.Warnings = "TRUNCATE", bound.Warnings()
		}
		return statementErr
	})
	if err == nil {
		err = transactions.lockTruncateDescriptors(operation, bound, options.MaxDescriptors)
	}
	if err == nil {
		err = transactions.routineAtomicStatement(operation, func(statementContext context.Context) (statementErr error) {
			defer func() {
				if statementErr != nil {
					transactions.named.rollbackTemporary(temporaryMark)
				}
			}()
			tables := bound.Tables()
			dependencies := slicesCloneReadDependencies(options.Dependencies)
			store := transactions.sequences.config.Catalog
			boundDependencies := bound.Dependencies()
			seenDependencies := make(map[model.ObjectID]bool, len(boundDependencies))
			for _, dependency := range boundDependencies {
				if transactions.named.temporary[dependency.Object] == nil && !seenDependencies[dependency.Object] {
					seenDependencies[dependency.Object] = true
					dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
				}
			}
			rootScan, err := store.NamespaceScanDependency(statementContext, 0, model.ObjectSchema)
			if err != nil {
				return err
			}
			dependencies = append(dependencies, rootScan)
			scannedSchemas := bound.ScannedSchemas()
			sequencesScanned := bound.SequencesScanned()
			for _, schemaID := range scannedSchemas {
				tableScan, err := store.NamespaceScanDependency(statementContext, schemaID, model.ObjectTable)
				if err != nil {
					return err
				}
				dependencies = append(dependencies, tableScan)
				if sequencesScanned {
					sequenceScan, err := store.NamespaceScanDependency(statementContext, schemaID, model.ObjectSequence)
					if err != nil {
						return err
					}
					dependencies = append(dependencies, sequenceScan)
				}
			}
			view, err := transactions.transaction.ReadView(statementContext, dependencies)
			if err != nil {
				return err
			}
			defer func() { statementErr = errors.Join(statementErr, view.Close()) }()
			triggerTables, err := truncateTriggerTables(statementContext, tables, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			environment := invocation.Execution.Environment
			scope := &preparedMutationScope{transaction: transactions.transaction, ambient: true}
			scope.active.Store(true)
			defer scope.active.Store(false)
			statementContext = context.WithValue(statementContext, preparedScopeKey{}, scope)
			var triggerRuntime model.TriggerRuntime
			var executor *routineExecutor
			if len(triggerTables) != 0 {
				triggerRuntime = environment.Triggers
				executor, _ = statementContext.Value(routineInvocationKey{}).(*routineExecutor)
				if triggerRuntime != nil && executor != nil {
					if err := executor.replaceFrame(statementContext, view, false); err != nil {
						return err
					}
				}
				if triggerRuntime == nil {
					if invocation.Execution.Routines == nil {
						return settingError("22023", "TRUNCATE trigger runtime services are not configured")
					}
					triggerDependencies := make([]model.Dependency, len(triggerTables))
					for i, table := range triggerTables {
						triggerDependencies[i] = model.Dependency{Object: table.ID(), Version: table.Version()}
					}
					statementContext, executor, err = attachRoutineExecutionState(statementContext, transactions, transactions.transaction, view, scope, &environment, invocation.Execution.Environment.Catalog, invocation.Execution.Catalog, invocation.Execution, invocation.Execution.Routines, triggerDependencies, false)
					if err != nil {
						return err
					}
					defer func() { statementErr = errors.Join(statementErr, executor.closeCapabilities()) }()
					triggerRuntime = environment.Triggers
				}
				if triggerRuntime == nil || executor == nil {
					return settingError("22023", "TRUNCATE trigger runtime cannot refresh transaction capabilities")
				}
			}
			begun := make([]*model.Descriptor, 0, len(triggerTables))
			finished := false
			defer func() {
				if triggerRuntime != nil && !finished {
					for _, table := range begun {
						triggerRuntime.AbortStatement(table, model.TriggerTruncate)
					}
				}
			}()
			if triggerRuntime != nil {
				for _, table := range triggerTables {
					if err := triggerRuntime.BeginStatement(statementContext, table, model.TriggerTruncate, nil); err != nil {
						return err
					}
					begun = append(begun, table)
				}
			}
			for _, command := range bound.Commands() {
				if err := transactions.stageAlterTableExpressionCommand(statementContext, command, options, true); err != nil {
					return err
				}
			}
			for _, sequence := range bound.Sequences() {
				if _, err := transactions.stageSequenceDDL(statementContext, sequence, nil); err != nil {
					return err
				}
			}
			if triggerRuntime != nil {
				refreshed, err := transactions.transaction.ReadView(statementContext, dependencies)
				if err != nil {
					return err
				}
				if err := executor.replaceFrame(statementContext, refreshed, true); err != nil {
					return err
				}
				for _, table := range triggerTables {
					if err := triggerRuntime.FinishStatement(statementContext, table, model.TriggerTruncate); err != nil {
						return err
					}
				}
				finished = true
			}
			return nil
		})
	}
	err = errors.Join(err, transactions.markDataAccess())
	complete = true
	if err != nil {
		transactions.sequences.abandon(sequenceMark)
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		} else {
			err = errors.Join(err, transactions.sequences.clean(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
	}
	return result, err
}

func (transactions *Transactions) bindTruncateTransactional(ctx context.Context, statement *zpgparse.TruncateStmt, options ObjectCommandOptions) (*bind.BoundTruncate, error) {
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
	bound, bindErr := bind.BindTruncateTransactional(ctx, statement, snapshot, options.Binding)
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) lockTruncateDescriptors(ctx context.Context, bound *bind.BoundTruncate, maximum uint32) error {
	if bound == nil {
		return errPreparedInvocation
	}
	references := make([]storage.ObjectRef, 0)
	referenceIDs := make(map[model.ObjectID]bool)
	for _, command := range bound.Commands() {
		for _, update := range command.Updates() {
			if update.Base == nil || referenceIDs[update.Base.ID()] {
				continue
			}
			physical := update.Base.Physical()
			target := storage.ObjectRef{ObjectID: update.Base.ID(), Generation: physical.Generation, DescriptorVersion: update.Base.Version(), EncodingVersion: physical.EncodingVersion}
			if err := target.Validate(); err != nil {
				return err
			}
			referenceIDs[update.Base.ID()] = true
			references = append(references, target)
		}
	}
	for _, definition := range bound.Sequences() {
		base := definition.Base()
		if base == nil || referenceIDs[base.ID()] {
			return model.ErrInvalidDescriptor
		}
		physical := base.Physical()
		target := storage.ObjectRef{ObjectID: base.ID(), Generation: physical.Generation, DescriptorVersion: base.Version(), EncodingVersion: physical.EncodingVersion}
		if err := target.Validate(); err != nil {
			return err
		}
		referenceIDs[base.ID()] = true
		references = append(references, target)
	}
	slices.SortFunc(references, func(left, right storage.ObjectRef) int {
		if left.ObjectID < right.ObjectID {
			return -1
		}
		if left.ObjectID > right.ObjectID {
			return 1
		}
		return 0
	})
	for _, target := range references {
		if err := transactions.transaction.LockSchema(ctx, target); err != nil {
			return err
		}
	}
	ids := make([]model.ObjectID, 0)
	seen := make(map[model.ObjectID]bool)
	for _, command := range bound.Commands() {
		for _, update := range command.Updates() {
			if transactions.named.temporary[update.Base.ID()] == nil && !seen[update.Base.ID()] {
				seen[update.Base.ID()] = true
				ids = append(ids, update.Base.ID())
			}
		}
	}
	for _, definition := range bound.Sequences() {
		base := definition.Base()
		if transactions.named.temporary[base.ID()] == nil && !seen[base.ID()] {
			seen[base.ID()] = true
			ids = append(ids, base.ID())
		}
	}
	if maximum == 0 || uint64(len(ids)) > uint64(maximum) {
		return model.ErrDescriptorLimit
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, id); err != nil {
			return err
		}
	}
	return nil
}

func truncateTriggerTables(ctx context.Context, tables []*model.Descriptor, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	var result []*model.Descriptor
	for _, table := range tables {
		spec, err := table.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		if slices.ContainsFunc(spec.Table.Triggers, func(trigger model.TriggerDescriptor) bool {
			return trigger.EnableMode.Fires(false) && !trigger.ForEachRow && slices.Contains(trigger.Events, "truncate")
		}) {
			result = append(result, table)
		}
	}
	return result, ctx.Err()
}
