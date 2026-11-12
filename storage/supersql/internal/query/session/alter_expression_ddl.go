package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runAlterTableExpression(ctx context.Context, statement *zpgparse.AlterTableStmt, options ObjectCommandOptions, cascade *TypeCascadeOptions) (result ExecutionResult, err error) {
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
	sequenceMark := len(transactions.sequences.changes)
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
		metadata, rewrite, err := transactions.bindAlterTableExpression(ctx, statement, options)
		if err != nil {
			return err
		}
		rewrite, err = transactions.finalizeAlterTableSequences(ctx, rewrite, options)
		if err != nil {
			return err
		}
		if metadata != nil {
			result.Tag, result.Warnings = metadata.CommandTag(), metadata.Warnings()
			return transactions.stageValidatedMetadataCommand(ctx, metadata, options)
		}
		if rewrite != nil && rewrite.NoOp() {
			result.Tag, result.Warnings = "ALTER TABLE", rewrite.Warnings()
			return nil
		}
		if rewrite != nil && rewrite.MetadataOnly() != nil {
			command, commandErr := rewrite.TransactionalMetadataCommand()
			if commandErr != nil {
				return commandErr
			}
			result.Tag, result.Warnings = command.CommandTag(), command.Warnings()
			return transactions.stageValidatedMetadataCommand(ctx, command, options)
		}
		if rewrite == nil || cascade == nil || cascade.Policies == nil {
			return settingError("22023", "transactional generation rewrite services are not configured")
		}
		result.Tag, result.Warnings = "ALTER TABLE", rewrite.Warnings()
		if bind.TransactionalGenerationAlter(statement) || bind.SchemaJobAlter(statement) {
			command, commandErr := rewrite.TransactionalSchemaCommand()
			if commandErr != nil {
				return commandErr
			}
			return transactions.stageGenerationRewrite(ctx, rewrite, command, options, *cascade)
		}
		return transactions.stageAlterTableExpressionRewrite(ctx, rewrite, options, *cascade)
	})
	if err == nil {
		for i := sequenceMark; i < len(transactions.sequences.changes); i++ {
			transactions.sequences.changes[i].published = true
		}
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

func (transactions *Transactions) bindAlterTableExpression(ctx context.Context, statement *zpgparse.AlterTableStmt, options ObjectCommandOptions) (*bind.BoundObjectCommand, *bind.BoundSchemaDDL, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return nil, nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	var metadata *bind.BoundObjectCommand
	var rewrite *bind.BoundSchemaDDL
	var bindErr error
	if bind.TransactionalGenerationAlter(statement) || bind.SchemaJobAlter(statement) {
		rewrite, bindErr = bind.BindAlterTableTransactional(ctx, statement, snapshot, options.Binding)
	} else {
		metadata, rewrite, bindErr = bind.BindAlterTableExpression(ctx, statement, snapshot, options.Binding)
	}
	return metadata, rewrite, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) stageAlterTableExpressionRewrite(ctx context.Context, bound *bind.BoundSchemaDDL, options ObjectCommandOptions, cascade TypeCascadeOptions) error {
	command, err := bound.TransactionalExpressionCommand()
	if err != nil {
		return err
	}
	return transactions.stageGenerationRewrite(ctx, bound, command, options, cascade)
}

func (transactions *Transactions) finalizeAlterTableSequences(ctx context.Context, bound *bind.BoundSchemaDDL, options ObjectCommandOptions) (*bind.BoundSchemaDDL, error) {
	if bound == nil || len(bound.PendingSequences()) == 0 {
		return bound, nil
	}
	definitions := bound.PendingSequences()
	if len(definitions) > cap(transactions.sequences.changes)-len(transactions.sequences.changes) {
		return nil, &model.Error{Code: "54000", Message: "owned sequence declaration limit exceeded"}
	}
	descriptors := make([]*model.Descriptor, len(definitions))
	for i, definition := range definitions {
		index, err := transactions.sequences.reserve(0)
		if err != nil {
			return nil, err
		}
		config := definition.Configuration()
		state, err := transactions.sequences.config.Service.Initialize(ctx, schema.SequenceConfig{
			Start: config.Start, Increment: config.Increment, Min: config.Min, Max: config.Max, Cache: config.Cache, Cycle: config.Cycle,
		})
		change := &transactions.sequences.changes[index]
		change.created = state.Incarnation
		if err != nil {
			var pending *schema.PendingSequenceError
			if errors.As(err, &pending) {
				change.initialization = pending.Transaction
			}
			return nil, err
		}
		descriptors[i], err = definition.Finalize(ctx, uint64(state.Incarnation))
		if err != nil {
			return nil, err
		}
	}
	return bound.FinalizeSequences(ctx, descriptors, options.Binding.Descriptors)
}

func (transactions *Transactions) stageGenerationRewrite(ctx context.Context, bound *bind.BoundSchemaDDL, command *bind.BoundObjectCommand, options ObjectCommandOptions, cascade TypeCascadeOptions) error {
	if !objectCommandTemporary(ctx, command, options.Binding.Descriptors) {
		for _, drop := range command.Drops() {
			if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, drop.ID()); err != nil {
				return err
			}
		}
		for _, update := range command.Updates() {
			if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, update.Base.ID()); err != nil {
				return err
			}
		}
	}
	store := transactions.sequences.config.Catalog
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range bound.Dependencies() {
		if transactions.named.temporary[dependency.Object] == nil {
			dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
		}
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := addObjectCommandOverlay(snapshot, command); err != nil {
		return errors.Join(err, snapshot.Close(), view.Close())
	}
	definitions := bound.Definitions()
	if len(definitions) == 0 {
		return errors.Join(model.ErrInvalidDescriptor, snapshot.Close(), view.Close())
	}
	type rewritePlan struct {
		plan  *schema.GenerationPlan
		codec schema.KeyCodec
	}
	plans := make([]rewritePlan, len(definitions))
	for i, definition := range definitions {
		plan, codec, planErr := cascade.Policies.GenerationPlan(ctx, snapshot, definition)
		if planErr != nil {
			err = planErr
			break
		}
		plans[i] = rewritePlan{plan: plan, codec: codec}
		dependencies = append(dependencies, txn.ReadDependency{
			Target: plan.Source(), KeyLayout: plan.SourceKeyLayout(),
			Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		})
	}
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		return err
	}
	view, err = transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err = store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return errors.Join(err, snapshot.Close(), view.Close())
	}
	// Revalidate bind-recorded versions against the published catalog before
	// the command overlay lands: the overlay carries this command's own pending
	// descriptors, which necessarily differ from the versions binding saw.
	for _, dependency := range bound.Dependencies() {
		if transactions.named.temporary[dependency.Object] != nil {
			continue
		}
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			err = resolveErr
			break
		}
		if published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
			err = catalog.ErrDescriptorChanged
			break
		}
	}
	if err == nil {
		err = addObjectCommandOverlay(snapshot, command)
	}
	var runtime model.ExpressionRuntime
	if err == nil {
		sequences, runtimeErr := NewSequenceRuntime(snapshot, transactions.sequences.config.Session)
		if runtimeErr != nil {
			err = runtimeErr
		} else {
			runtime.Sequences = sequences
		}
	}
	for _, rewrite := range plans {
		if err != nil {
			break
		}
		err = schema.RewriteGenerationInTransactionWithRuntime(ctx, transactions.transaction, view, view.Timestamp(), rewrite.plan, rewrite.codec, cascade.Scan, nil, runtime)
	}
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		return err
	}
	if len(command.Validations()) != 0 {
		if err := transactions.validateObjectConstraints(ctx, command, options); err != nil {
			return err
		}
	}
	return transactions.stageAlterTableExpressionCommand(ctx, command, options, true)
}

func (transactions *Transactions) stageValidatedMetadataCommand(ctx context.Context, command *bind.BoundObjectCommand, options ObjectCommandOptions) error {
	temporary := objectCommandTemporary(ctx, command, options.Binding.Descriptors)
	if !temporary {
		if err := transactions.lockObjectCommandDescriptors(ctx, command); err != nil {
			return err
		}
	}
	if len(command.Validations()) != 0 {
		if err := transactions.validateObjectConstraints(ctx, command, options); err != nil {
			return err
		}
	}
	return transactions.stageAlterTableExpressionCommand(ctx, command, options, !temporary)
}

func addObjectCommandOverlay(snapshot *catalog.CatalogSnapshot, command *bind.BoundObjectCommand) error {
	if snapshot == nil || command == nil {
		return model.ErrInvalidDescriptor
	}
	descriptors := command.Creates()
	for _, update := range command.Updates() {
		if update.Next != nil {
			descriptors = append(descriptors, update.Next)
		}
	}
	if len(descriptors) == 0 {
		return nil
	}
	return snapshot.AddSessionOverlay(descriptors)
}

func (transactions *Transactions) stageAlterTableExpressionCommand(ctx context.Context, command *bind.BoundObjectCommand, options ObjectCommandOptions, locked bool) error {
	if objectCommandTemporary(ctx, command, options.Binding.Descriptors) {
		transactions.named.temporaryOptions = &options
		if err := transactions.named.applyTemporaryCommand(command); err != nil {
			return err
		}
		return transactions.stageTemporaryObjectCommand(ctx, command, options)
	}
	if !locked {
		for _, update := range command.Updates() {
			if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, update.Base.ID()); err != nil {
				return err
			}
		}
	}
	return transactions.stageObjectCommand(ctx, command, options)
}
