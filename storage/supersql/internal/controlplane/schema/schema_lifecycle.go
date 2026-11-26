package schema

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type SchemaPublicationProof struct {
	jobs       *JobStore
	job        Job
	table      storage.ObjectRef
	target     storage.ObjectRef
	definition JobDefinition
}

type SchemaTransition struct {
	Expected   Job
	Next       Job
	Table      storage.ObjectRef
	Protected  txn.ReadView
	Validation *SchemaPublicationProof
}

// CreateDefinedIntent validates the current source descriptor and persists the
// job and its target definition atomically. It never publishes a partial target.
func (l *IndexLifecycle) CreateDefinedIntent(ctx context.Context, transaction *txn.Transaction, job Job, definition JobDefinition) error {
	return l.createDefinedIntent(ctx, transaction, job, definition, nil)
}

func (l *IndexLifecycle) createDefinedIntent(ctx context.Context, transaction *txn.Transaction, job Job, definition JobDefinition, expected []model.Dependency) error {
	if l == nil || l.catalog == nil || l.jobs == nil || transaction == nil || job.Kind == AddIndexJob {
		return errIndexLifecycle
	}
	if err := validateJobDefinition(ctx, job, definition, l.jobs.options.RecordBytes); err != nil {
		return err
	}
	source, err := descriptorTarget(definition.SourceTable)
	if err != nil {
		return err
	}
	if err := transaction.LockSchema(ctx, source); err != nil {
		return err
	}
	return transaction.Statement(ctx, func(ctx context.Context) (err error) {
		dependencies, err := l.definitionDependencies(ctx, job, definition)
		if err != nil {
			return err
		}
		for _, dependency := range expected {
			if dependency.Object == 0 || dependency.Version == 0 {
				return errJobDefinition
			}
			dependencies = append(dependencies, l.catalog.DescriptorDependency(dependency.Object))
		}
		view, err := transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := l.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
		for _, dependency := range expected {
			published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
			if resolveErr != nil || published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
				return errors.Join(catalog.ErrDescriptorChanged, resolveErr)
			}
		}
		current, err := snapshot.Descriptor(ctx, job.TableID)
		if err != nil {
			return err
		}
		if !sameDescriptorIdentity(current.Descriptor, definition.SourceTable) {
			return catalog.ErrDescriptorChanged
		}
		for _, expected := range definition.SourceIndexes {
			published, err := snapshot.Descriptor(ctx, expected.ID())
			if err != nil {
				return err
			}
			if !sameDescriptorIdentity(published.Descriptor, expected) {
				return catalog.ErrDescriptorChanged
			}
		}
		return l.jobs.CreateDefined(ctx, transaction, job, definition)
	})
}

func sameDescriptorIdentity(left, right *model.Descriptor) bool {
	return left != nil && right != nil && left.ID() == right.ID() && left.Version() == right.Version() && left.Physical() == right.Physical() && left.State() == right.State() && left.Name() == right.Name()
}

func (l *IndexLifecycle) definitionDependencies(ctx context.Context, job Job, definition JobDefinition) ([]txn.ReadDependency, error) {
	dependencies := []txn.ReadDependency{l.jobs.Dependencies(job.ID, job.TableID)}
	seen := make(map[model.ObjectID]struct{})
	appendObject := func(id model.ObjectID) {
		if id == 0 {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		dependencies = append(dependencies, l.catalog.DescriptorDependency(id))
	}
	descriptors := []*model.Descriptor{definition.SourceTable, definition.TargetTable}
	descriptors = append(descriptors, definition.SourceIndexes...)
	descriptors = append(descriptors, definition.TargetIndexes...)
	for _, descriptor := range descriptors {
		if descriptor == nil {
			return nil, errJobDefinition
		}
		appendObject(descriptor.ID())
		spec, err := descriptor.SpecWithin(ctx, l.limits)
		if err != nil {
			return nil, err
		}
		for _, dependency := range spec.Dependencies {
			appendObject(dependency.Object)
		}
	}
	return dependencies, nil
}

func (l *IndexLifecycle) StageDefined(ctx context.Context, transaction *txn.Transaction, change SchemaTransition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.catalog == nil || l.jobs == nil || transaction == nil || change.Expected.Kind == AddIndexJob {
		return errIndexLifecycle
	}
	if err := change.Table.Validate(); err != nil {
		return err
	}
	if change.Table.ObjectID != change.Expected.TableID || change.Expected.TableID == 0 {
		return errIndexLifecycle
	}
	if err := validJob(change.Expected, l.jobs.options.RecordBytes); err != nil {
		return err
	}
	if err := validJob(change.Next, l.jobs.options.RecordBytes); err != nil {
		return err
	}
	if err := validateJobSuccessor(change.Expected, change.Next); err != nil {
		return err
	}
	if change.Expected.State == change.Next.State || !bytes.Equal(change.Expected.ResumeKey, change.Next.ResumeKey) {
		return ErrJobTransition
	}
	starting := change.Expected.State == JobWriteOnly && change.Next.State == JobBackfilling
	if starting {
		if change.Protected == nil || change.Protected.Timestamp() != change.Next.SnapshotTS || change.Next.ProtectedTimestamp != change.Next.SnapshotTS {
			return storage.ErrSnapshotNotClosed
		}
	}
	if change.Next.State == JobPublic {
		proof := change.Validation
		if proof == nil || proof.jobs != l.jobs || !sameBackfillJob(proof.job, change.Expected) || proof.table != change.Table {
			return errIndexPublicationProof
		}
	}
	if err := transaction.LockSchema(ctx, change.Table); err != nil {
		return err
	}
	return transaction.Statement(ctx, func(ctx context.Context) error {
		changes, err := l.definedChanges(ctx, transaction, change)
		if err != nil {
			return err
		}
		for _, catalogChange := range changes {
			if len(catalogChange.Mutations) != 0 {
				if err := transaction.ApplyMutations(ctx, catalogChange.Mutations); err != nil {
					return err
				}
			}
		}
		if change.Next.State == JobPublic || change.Next.State == JobReverting {
			definition, err := l.jobs.Definition(ctx, transaction, change.Expected)
			if err != nil {
				return err
			}
			retired, err := transitionRetirements(definition, change.Next.State)
			if err != nil {
				return err
			}
			if err := l.jobs.recordRetirements(ctx, transaction, change.Next.ID, retired); err != nil {
				return err
			}
		}
		return l.jobs.Update(ctx, transaction, change.Expected.Revision, change.Next)
	})
}

func (l *IndexLifecycle) definedChanges(ctx context.Context, transaction *txn.Transaction, change SchemaTransition) (changes []catalog.Change, err error) {
	definition, err := l.jobs.Definition(ctx, transaction, change.Expected)
	if err != nil {
		return nil, err
	}
	dependencies, err := l.definitionDependencies(ctx, change.Expected, definition)
	if err != nil {
		return nil, err
	}
	view, err := transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := l.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	current, err := l.jobs.Get(ctx, view, change.Expected.ID)
	if err != nil {
		return nil, err
	}
	if !sameBackfillJob(current, change.Expected) {
		return nil, ErrJobChanged
	}
	if startingDefinedTransition(change) {
		if change.Protected.Timestamp() > view.Timestamp() {
			return nil, storage.ErrSnapshotNotClosed
		}
		protectedJob, err := l.jobs.Get(ctx, change.Protected, current.ID)
		if err != nil || !sameBackfillJob(protectedJob, current) {
			return nil, errors.Join(ErrJobChanged, err)
		}
	}
	table, err := snapshot.Descriptor(ctx, current.TableID)
	if err != nil {
		return nil, errors.Join(catalog.ErrDescriptorChanged, err)
	}
	if table.Descriptor.Kind() != model.ObjectTable || table.Descriptor.State() != model.StatePublic || lifecycleTarget(table.Descriptor) != change.Table {
		return nil, catalog.ErrDescriptorChanged
	}
	if change.Next.State == JobPublic {
		proof := change.Validation
		target, err := descriptorTarget(definition.TargetTable)
		if err != nil || proof.target != target || !sameDescriptorIdentity(proof.definition.TargetTable, definition.TargetTable) {
			return nil, errors.Join(errIndexPublicationProof, err)
		}
	}
	return l.deriveDefinedChanges(ctx, snapshot, table.Descriptor, definition, change)
}

func startingDefinedTransition(change SchemaTransition) bool {
	return change.Expected.State == JobWriteOnly && change.Next.State == JobBackfilling
}

func (l *IndexLifecycle) deriveDefinedChanges(ctx context.Context, snapshot *catalog.CatalogSnapshot, current *model.Descriptor, definition JobDefinition, change SchemaTransition) ([]catalog.Change, error) {
	if change.Expected.State == JobPublic || change.Expected.State == JobReverting || startingDefinedTransition(change) ||
		change.Expected.State == JobBackfilling && change.Next.State == JobValidating && definition.Operation != SchemaDropIndex {
		return nil, nil
	}
	if change.Expected.State == JobValidating && change.Next.State == JobPublic {
		return l.publishDefinition(ctx, snapshot, current, definition)
	}
	currentSpec, err := current.SpecWithin(ctx, l.limits)
	if err != nil {
		return nil, err
	}
	if currentSpec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	var extra []catalog.Change
	switch definition.Operation {
	case SchemaAddColumn:
		err = deriveAddColumnState(ctx, &currentSpec, definition, change, l.limits)
	case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey, SchemaReindex:
		// The source descriptor changes version only, fencing pre-policy writers.
	case SchemaDropIndex:
		extra, err = l.deriveDropIndexState(ctx, snapshot, &currentSpec, definition, change)
	default:
		err = errJobDefinition
	}
	if err != nil {
		return nil, err
	}
	if change.Expected.State == JobBackfilling && change.Next.State == JobValidating {
		return extra, nil
	}
	currentSpec.Version = current.Version() + 1
	next, err := model.NewDescriptor(ctx, currentSpec, l.limits)
	if err != nil {
		return nil, err
	}
	tableChange, err := l.catalog.Update(ctx, snapshot, current.Version(), next)
	if err != nil {
		return nil, err
	}
	return append([]catalog.Change{tableChange}, extra...), nil
}

func deriveAddColumnState(ctx context.Context, spec *model.DescriptorSpec, definition JobDefinition, change SchemaTransition, limits model.DescriptorLimits) error {
	target, err := definition.TargetTable.SpecWithin(ctx, limits)
	if err != nil || target.Table == nil {
		return errors.Join(errJobDefinition, err)
	}
	columnID := model.ColumnID(change.Expected.MutationID)
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == columnID })
	targetPosition := slices.IndexFunc(target.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == columnID })
	if targetPosition < 0 {
		return errJobDefinition
	}
	if change.Next.State == JobReverting {
		if position >= 0 {
			spec.Table.Columns = slices.Delete(spec.Table.Columns, position, position+1)
		}
		return nil
	}
	if position < 0 {
		if change.Expected.State != JobPending {
			return catalog.ErrDescriptorChanged
		}
		column := target.Table.Columns[targetPosition]
		column.State = model.StateDeleteOnly
		spec.Table.Columns = append(spec.Table.Columns, column)
		spec.Table.ColumnIDHighWatermark = max(spec.Table.ColumnIDHighWatermark, column.ID)
		return nil
	}
	switch change.Next.State {
	case JobWriteOnly:
		spec.Table.Columns[position].State = model.StateWriteOnly
	case JobReverting:
		spec.Table.Columns = slices.Delete(spec.Table.Columns, position, position+1)
	}
	return nil
}

func (l *IndexLifecycle) deriveDropIndexState(ctx context.Context, snapshot *catalog.CatalogSnapshot, table *model.DescriptorSpec, definition JobDefinition, change SchemaTransition) ([]catalog.Change, error) {
	indexID := change.Expected.MutationID
	if !slices.Contains(table.Table.Indexes, indexID) {
		return nil, catalog.ErrDescriptorChanged
	}
	published, err := snapshot.Descriptor(ctx, indexID)
	if err != nil {
		return nil, err
	}
	spec, err := published.Descriptor.SpecWithin(ctx, l.limits)
	if err != nil || spec.Index == nil {
		return nil, errors.Join(errJobDefinition, err)
	}
	switch change.Next.State {
	case JobDeleteOnly:
		spec.State = model.StateDropWriteOnly
		spec.Index.Invalid = true
		spec.Index.NotReady = false
		spec.Index.NotLive = false
	case JobWriteOnly:
		spec.State = model.StateDropDeleteOnly
		spec.Index.Invalid = true
		spec.Index.NotReady = true
		spec.Index.NotLive = false
	case JobValidating:
		spec.State = model.StateDropDeleteOnly
		spec.Index.Invalid = true
		spec.Index.NotReady = true
		spec.Index.NotLive = true
	case JobReverting:
		if len(definition.SourceIndexes) != 1 || definition.SourceIndexes[0].ID() != indexID {
			return nil, errJobDefinition
		}
		source, ok := definition.SourceIndexes[0].Index()
		if !ok {
			return nil, errJobDefinition
		}
		spec.State = definition.SourceIndexes[0].State()
		spec.Index.Invalid = source.Invalid
		spec.Index.NotReady = source.NotReady
		spec.Index.NotLive = source.NotLive
	default:
		return nil, nil
	}
	spec.Version = published.Descriptor.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, l.limits)
	if err != nil {
		return nil, err
	}
	indexChange, err := l.catalog.Update(ctx, snapshot, published.Descriptor.Version(), next)
	if err != nil {
		return nil, err
	}
	return []catalog.Change{indexChange}, nil
}

func (l *IndexLifecycle) publishDefinition(ctx context.Context, snapshot *catalog.CatalogSnapshot, current *model.Descriptor, definition JobDefinition) ([]catalog.Change, error) {
	if definition.TargetTable.Version() != current.Version()+1 {
		return nil, catalog.ErrDescriptorChanged
	}
	changes := make([]catalog.Change, 0, 1+len(definition.TargetIndexes)+len(definition.SourceIndexes))
	tableChange, err := l.catalog.Update(ctx, snapshot, current.Version(), definition.TargetTable)
	if err != nil {
		return nil, err
	}
	changes = append(changes, tableChange)
	switch definition.Operation {
	case SchemaAddColumn:
	case SchemaDropIndex:
		index := definition.SourceIndexes[0]
		published, err := snapshot.Descriptor(ctx, index.ID())
		if err != nil {
			return nil, err
		}
		change, err := l.catalog.Drop(ctx, snapshot, index.ID(), published.Descriptor.Version())
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey, SchemaReindex:
		if len(definition.SourceIndexes) != len(definition.TargetIndexes) {
			return nil, errJobDefinition
		}
		for i, target := range definition.TargetIndexes {
			source := definition.SourceIndexes[i]
			if source.ID() != target.ID() || target.Version() != source.Version()+1 {
				return nil, errJobDefinition
			}
			published, err := snapshot.Descriptor(ctx, source.ID())
			if err != nil || published.Descriptor.Version() != source.Version() {
				return nil, errors.Join(catalog.ErrDescriptorChanged, err)
			}
			change, err := l.catalog.Update(ctx, snapshot, source.Version(), target)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change)
		}
	default:
		return nil, errJobDefinition
	}
	return changes, nil
}
