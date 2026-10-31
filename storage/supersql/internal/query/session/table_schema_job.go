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

type OnlineSchemaOptions struct {
	Binding   bind.CreateDeclarationOptions
	Catalog   *catalog.Store
	JobID     schema.JobID
	Lifecycle *schema.IndexLifecycle
	Policies  *schema.IndexPolicyProvider
	Scheduler *schema.IndexJobScheduler
}

type SchemaCommandResult struct {
	Tag        string
	Descriptor *model.Descriptor
	Job        schema.Job
	Warnings   []model.Error
	Commit     EndResult
}

var errOnlineSchemaOptions = &model.Error{Code: "22023", Message: "invalid online schema command options"}

func ExecuteAlterTableOnline(ctx context.Context, transactions *Transactions, statement *zpgparse.AlterTableStmt, snapshot bind.CatalogSnapshot, options OnlineSchemaOptions) (SchemaCommandResult, error) {
	return executeOnlineSchema(ctx, transactions, snapshot, options, "ALTER TABLE", func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundSchemaDDL, error) {
		return bind.BindAlterTableOnline(ctx, statement, snapshot, options)
	})
}

func ExecuteDropIndexConcurrently(ctx context.Context, transactions *Transactions, statement *zpgparse.DropStmt, snapshot bind.CatalogSnapshot, options OnlineSchemaOptions) (SchemaCommandResult, error) {
	return executeOnlineSchema(ctx, transactions, snapshot, options, "DROP INDEX", func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundSchemaDDL, error) {
		return bind.BindDropIndexConcurrently(ctx, statement, snapshot, options)
	})
}
func ExecuteReindexConcurrently(ctx context.Context, transactions *Transactions, statement *zpgparse.ReindexStmt, snapshot bind.CatalogSnapshot, options OnlineSchemaOptions) (SchemaCommandResult, error) {
	return executeOnlineSchema(ctx, transactions, snapshot, options, "REINDEX", func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundSchemaDDL, error) {
		return bind.BindReindexConcurrent(ctx, statement, snapshot, options)
	})
}

type onlineSchemaBinder func(context.Context, bind.CatalogSnapshot, bind.CreateDeclarationOptions) (*bind.BoundSchemaDDL, error)

func executeOnlineSchema(ctx context.Context, transactions *Transactions, snapshot bind.CatalogSnapshot, options OnlineSchemaOptions, tag string, bindCommand onlineSchemaBinder) (result SchemaCommandResult, err error) {
	missingSession := transactions == nil || snapshot == nil
	missingServices := options.Catalog == nil || options.Lifecycle == nil || options.Policies == nil || options.Scheduler == nil
	if missingSession || missingServices {
		return result, errOnlineSchemaOptions
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if err := transactions.indexAutocommit(); err != nil {
		return result, err
	}
	options.Binding.Bind.SearchPath = transactions.settings.SearchPath()
	options.Binding.Owner = transactions.settings.Principal().OID()
	options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
	options.Binding.Bind.Temporal = transactions.temporal
	resources := transactions.settings.Resources()
	if resources.EffectiveMaintenanceMemoryBytes <= 0 {
		return result, settingError("53200", "online schema change has no maintenance memory")
	}
	options.Binding.Bind.Limits.MaxBytes = min(options.Binding.Bind.Limits.MaxBytes, uint64(resources.EffectiveMaintenanceMemoryBytes))
	options.Binding.Descriptors.MaxBytes = min(options.Binding.Descriptors.MaxBytes, uint64(resources.EffectiveMaintenanceMemoryBytes))
	bound, err := bindCommand(operation, snapshot, options.Binding)
	if err != nil {
		return result, sessionTransactionError(err)
	}
	result.Warnings = bound.Warnings()
	if bound.NoOp() {
		result.Tag = tag
		return result, nil
	}
	if bound.ReindexHierarchy() {
		result, err = transactions.executeOnlineReindexHierarchy(operation, bound, options, tag)
		return result, sessionTransactionError(err)
	}
	if index := bound.IndexAddition(); index != nil {
		if options.JobID == 0 {
			return result, errOnlineSchemaOptions
		}
		source := bound.Source()
		physical := source.Physical()
		table := storage.ObjectRef{ObjectID: source.ID(), Generation: physical.Generation, DescriptorVersion: source.Version(), EncodingVersion: physical.EncodingVersion}
		job, jobErr := schema.NewAddIndexJob(options.JobID, table, index.ID())
		if jobErr != nil {
			return result, sessionTransactionError(jobErr)
		}
		id, submitErr := transactions.submitIndex(operation, options.Lifecycle, schema.IndexSubmissionOptions{Policies: options.Policies, Table: table, Index: index, Job: job})
		result.Job.ID = id
		if submitErr != nil {
			return result, submitErr
		}
		completed, waitErr := options.Scheduler.Wait(operation, id)
		if completed.ID != 0 {
			result.Job = completed
		}
		if waitErr != nil {
			return result, sessionTransactionError(waitErr)
		}
		result.Descriptor = bound.MetadataOnly()
		result.Tag = tag
		return result, nil
	}
	if target := bound.MetadataOnly(); target != nil {
		result.Descriptor = target
		result.Commit, err = applyMetadataSchemaChange(operation, transactions, options.Catalog, options.Policies, bound)
		if err != nil {
			return result, sessionTransactionError(err)
		}
		result.Tag = tag
		return result, nil
	}
	if options.JobID == 0 {
		return result, errOnlineSchemaOptions
	}
	job, definition, err := schema.NewDefinedJob(operation, options.JobID, bound.MutationID(), bound.Definition(), options.Binding.Descriptors.MaxBytes)
	if err != nil {
		return result, sessionTransactionError(err)
	}
	id, err := transactions.submitDefined(operation, options.Lifecycle, schema.DefinedSubmissionOptions{Policies: options.Policies, Job: job, Definition: definition})
	result.Job.ID = id
	if err != nil {
		return result, err
	}
	completed, err := options.Scheduler.Wait(operation, id)
	if completed.ID != 0 {
		result.Job = completed
	}
	if err != nil {
		return result, sessionTransactionError(err)
	}
	result.Tag = tag
	return result, nil
}

func (transactions *Transactions) executeOnlineReindexHierarchy(ctx context.Context, bound *bind.BoundSchemaDDL, options OnlineSchemaOptions, tag string) (result SchemaCommandResult, err error) {
	definitions := bound.Definitions()
	if len(definitions) == 0 || options.JobID == 0 || transactions.sequences == nil || transactions.sequences.config.Identities == nil {
		return result, errOnlineSchemaOptions
	}
	expected := reindexDependencies(bound.Dependencies())
	for i, definition := range definitions {
		jobID := options.JobID
		if i == 0 {
			if err := transactions.validateOnlineReindexTopology(ctx, expected); err != nil {
				return result, err
			}
		} else {
			allocated, err := transactions.reserveOnlineReindexJob(ctx, expected)
			if err != nil {
				return result, err
			}
			jobID = allocated
		}
		mutation := definition.SourceTable.ID()
		job, fixed, err := schema.NewDefinedJob(ctx, jobID, mutation, definition, options.Binding.Descriptors.MaxBytes)
		if err != nil {
			return result, err
		}
		id, err := transactions.submitDefined(ctx, options.Lifecycle, schema.DefinedSubmissionOptions{
			Policies: options.Policies, Job: job, Definition: fixed,
			ExpectedDependencies: onlineReindexPreconditions(expected),
		})
		result.Job.ID = id
		if err != nil {
			return result, err
		}
		completed, err := options.Scheduler.Wait(ctx, id)
		if completed.ID != 0 {
			result.Job = completed
		}
		if err != nil {
			return result, err
		}
		for identity := range expected {
			switch identity.object {
			case definition.SourceTable.ID():
				expected[identity] = definition.TargetTable.Version()
			default:
				for index, source := range definition.SourceIndexes {
					if identity.object == source.ID() {
						expected[identity] = definition.TargetIndexes[index].Version()
						break
					}
				}
			}
		}
	}
	if err := transactions.validateOnlineReindexTopology(ctx, expected); err != nil {
		return result, err
	}
	result.Tag = tag
	result.Warnings = bound.Warnings()
	return result, nil
}

func onlineReindexPreconditions(expected map[reindexDependency]uint64) []model.Dependency {
	versions := make(map[model.ObjectID]uint64, len(expected))
	for identity, version := range expected {
		versions[identity.object] = version
	}
	result := make([]model.Dependency, 0, len(versions))
	for object, version := range versions {
		result = append(result, model.Dependency{Object: object, Version: version})
	}
	slices.SortFunc(result, func(left, right model.Dependency) int {
		if left.Object < right.Object {
			return -1
		}
		if left.Object > right.Object {
			return 1
		}
		return 0
	})
	return result
}

func (transactions *Transactions) validateOnlineReindexTopology(ctx context.Context, expected map[reindexDependency]uint64) (err error) {
	if err := transactions.begin(ctx, TransactionStart{}); err != nil {
		return err
	}
	defer func() {
		if transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}()
	return transactions.checkOnlineReindexTopology(ctx, expected)
}

func (transactions *Transactions) checkOnlineReindexTopology(ctx context.Context, expected map[reindexDependency]uint64) (err error) {
	dependencies := make([]txn.ReadDependency, 0, len(expected))
	seen := make(map[model.ObjectID]bool, len(expected))
	for identity := range expected {
		if !seen[identity.object] {
			seen[identity.object] = true
			dependencies = append(dependencies, transactions.sequences.config.Catalog.DescriptorDependency(identity.object))
		}
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	for identity, version := range expected {
		published, resolveErr := snapshot.Descriptor(ctx, identity.object)
		if resolveErr != nil || published.Descriptor.Version() != version || published.Descriptor.State() != model.StatePublic {
			return errors.Join(catalog.ErrDescriptorChanged, resolveErr)
		}
	}
	return nil
}

func (transactions *Transactions) reserveOnlineReindexJob(ctx context.Context, expected map[reindexDependency]uint64) (id schema.JobID, err error) {
	if err := transactions.begin(ctx, TransactionStart{}); err != nil {
		return 0, err
	}
	defer func() {
		if transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}()
	if err := transactions.checkOnlineReindexTopology(ctx, expected); err != nil {
		return 0, err
	}
	err = transactions.transaction.Statement(ctx, func(ctx context.Context) error {
		allocated, allocateErr := transactions.sequences.config.Identities.AllocateIdentity(ctx, transactions.transaction)
		id = allocated.JobID
		return allocateErr
	})
	if err != nil {
		return 0, err
	}
	_, err = transactions.commit(ctx, false)
	return id, err
}

func applyMetadataSchemaChange(ctx context.Context, transactions *Transactions, store *catalog.Store, policies *schema.IndexPolicyProvider, bound *bind.BoundSchemaDDL) (result EndResult, err error) {
	source, target := bound.Source(), bound.MetadataOnly()
	if source == nil || target == nil {
		return result, errOnlineSchemaOptions
	}
	expectedVersion := bound.ExpectedVersion()
	if expectedVersion == 0 {
		return result, errOnlineSchemaOptions
	}
	if err := transactions.begin(ctx, TransactionStart{}); err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if !complete && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}()
	physical := source.Physical()
	ref := storage.ObjectRef{ObjectID: source.ID(), Generation: physical.Generation, DescriptorVersion: expectedVersion, EncodingVersion: physical.EncodingVersion}
	if err := transactions.transaction.LockSchema(ctx, ref); err != nil {
		return result, err
	}
	err = transactions.routineAtomicStatement(ctx, func(ctx context.Context) (err error) {
		dependencies := []txn.ReadDependency{store.DescriptorDependency(source.ID())}
		for _, dependency := range bound.Dependencies() {
			dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
		}
		if constraintID := bound.ValidationID(); constraintID != 0 {
			targetSpec, specErr := target.Spec(ctx)
			if specErr != nil || targetSpec.Table == nil {
				return errors.Join(model.ErrInvalidDescriptor, specErr)
			}
			position := slices.IndexFunc(targetSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.ID == constraintID })
			if position < 0 {
				return model.ErrInvalidDescriptor
			}
			constraint := targetSpec.Table.Constraints[position]
			if constraint.Index != 0 {
				dependencies = append(dependencies, store.DescriptorDependency(constraint.Index))
			}
			if constraint.ReferencedObject != 0 {
				dependencies = append(dependencies, store.DescriptorDependency(constraint.ReferencedObject))
			}
			if constraint.ReferencedIndex != 0 {
				dependencies = append(dependencies, store.DescriptorDependency(constraint.ReferencedIndex))
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
		defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
		if constraint := bound.ValidationID(); constraint != 0 {
			if policies == nil {
				return errOnlineSchemaOptions
			}
			if err := policies.ValidateConstraint(ctx, transactions.transaction, snapshot, expectedVersion, source, target, constraint); err != nil {
				return err
			}
		}
		change, err := store.Update(ctx, snapshot, expectedVersion, target)
		if err != nil {
			return err
		}
		return transactions.transaction.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		return result, err
	}
	result, err = transactions.commit(ctx, false)
	complete = true
	return result, err
}
