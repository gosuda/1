package schema

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errIndexLifecycle        = &model.Error{Code: "22023", Message: "invalid index lifecycle operation", Cause: storage.ErrInvalidMutation}
	errIndexPublicationProof = &model.Error{Code: "40001", Message: "index publication validation is missing or stale", Cause: ErrJobChanged}
)

// IndexPublicationProof retains no reader. Its owned binding remains usable only
// while the job revision and both write-policy descriptor targets are unchanged.
type IndexPublicationProof struct {
	jobs  *JobStore
	job   Job
	table storage.ObjectRef
	index storage.ObjectRef
}

// ValidateJobIndex borrows one fixed reader for both the durable job and parity
// scans. The caller must not hold a writer barrier during these scans.
func (s *IndexSet) ValidateJobIndex(ctx context.Context, reader storage.SnapshotReader, jobs *JobStore, expected Job, limits IndexValidationLimits) (IndexPublicationProof, error) {
	if err := ctx.Err(); err != nil {
		return IndexPublicationProof{}, err
	}
	if s == nil || s.table == nil || reader == nil || jobs == nil || jobs.normalizer == nil {
		return IndexPublicationProof{}, errIndexLifecycle
	}
	if err := validJob(expected, jobs.options.RecordBytes); err != nil {
		return IndexPublicationProof{}, err
	}
	if expected.Kind != AddIndexJob || expected.State != JobValidating {
		return IndexPublicationProof{}, ErrJobTransition
	}
	current, err := jobs.Get(ctx, reader, expected.ID)
	if err != nil {
		return IndexPublicationProof{}, err
	}
	if !sameBackfillJob(current, expected) {
		return IndexPublicationProof{}, ErrJobChanged
	}
	if err := s.ValidateDescriptor(ctx, s.table); err != nil {
		return IndexPublicationProof{}, err
	}
	binding, found := s.index(current.MutationID)
	if !found || binding.plan == nil || binding.target != binding.plan.Target() || s.target.ObjectID != current.TableID {
		return IndexPublicationProof{}, errIndexSetDescriptor
	}
	if binding.plan.tableID != current.TableID || binding.plan.tableVersion != s.target.DescriptorVersion || binding.plan.State() != model.StateWriteOnly {
		return IndexPublicationProof{}, errIndexSetDescriptor
	}
	if _, err := s.ValidateIndex(ctx, reader, current.MutationID, limits); err != nil {
		return IndexPublicationProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return IndexPublicationProof{}, err
	}
	return IndexPublicationProof{jobs: jobs, job: current, table: s.target, index: binding.target}, nil
}

type IndexLifecycleOptions struct {
	Catalog *catalog.Store
	Jobs    *JobStore
	Limits  model.DescriptorLimits
}

type IndexLifecycle struct {
	catalog          *catalog.Store
	jobs             *JobStore
	limits           model.DescriptorLimits
	descriptorTarget storage.ObjectRef
	descriptorKeys   []storage.KeyColumn
}

func NewIndexLifecycle(options IndexLifecycleOptions) (*IndexLifecycle, error) {
	if options.Catalog == nil || options.Jobs == nil || options.Jobs.normalizer == nil {
		return nil, errIndexLifecycle
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	for _, layout := range options.Catalog.Layouts() {
		if layout.Target.ObjectID == catalog.DescriptorObjectID {
			return &IndexLifecycle{
				catalog: options.Catalog, jobs: options.Jobs, limits: options.Limits,
				descriptorTarget: layout.Target, descriptorKeys: options.Catalog.KeyLayout(),
			}, nil
		}
	}
	return nil, storage.ErrInvalidTarget
}

type IndexTransition struct {
	Expected Job
	Next     Job
	Table    storage.ObjectRef
	// Protected is borrowed from an external owner. Keep its pin until durable
	// job protection takes ownership, including after an indeterminate commit.
	Protected    txn.ReadView
	Dependencies []model.ObjectID
	Validation   *IndexPublicationProof
}

// Stage owns one statement, not its transaction. The schema barrier remains held
// until the caller commits or aborts; a failed statement publishes nothing.
func (l *IndexLifecycle) Stage(ctx context.Context, transaction *txn.Transaction, change IndexTransition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.catalog == nil || l.jobs == nil || transaction == nil {
		return errIndexLifecycle
	}
	if err := l.validateTransition(change); err != nil {
		return err
	}
	if err := transaction.LockSchema(ctx, change.Table); err != nil {
		return err
	}
	return transaction.Statement(ctx, func(ctx context.Context) error {
		changes, err := l.catalogChanges(ctx, transaction, change)
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
		return l.jobs.Update(ctx, transaction, change.Expected.Revision, change.Next)
	})
}

func (l *IndexLifecycle) validateTransition(change IndexTransition) error {
	if err := change.Table.Validate(); err != nil {
		return err
	}
	if change.Table.ObjectID != change.Expected.TableID || change.Expected.TableID == change.Expected.MutationID || change.Expected.Kind != AddIndexJob {
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
	for i, dependency := range change.Dependencies {
		if dependency == 0 || slices.Contains(change.Dependencies[:i], dependency) {
			return errIndexLifecycle
		}
	}
	// Resume advancement belongs to BackfillChunk's row/checkpoint statement.
	if change.Expected.State == change.Next.State || !bytes.Equal(change.Expected.ResumeKey, change.Next.ResumeKey) {
		return ErrJobTransition
	}
	starting := change.Expected.State == JobWriteOnly && change.Next.State == JobBackfilling
	if change.Expected.SnapshotTS == 0 && change.Next.SnapshotTS != 0 && !starting {
		return ErrJobTransition
	}
	if starting {
		if change.Protected == nil {
			return storage.ErrSnapshotNotClosed
		}
		timestamp := change.Protected.Timestamp()
		if timestamp <= 0 || timestamp != change.Next.SnapshotTS || change.Next.ProtectedTimestamp != change.Next.SnapshotTS {
			return storage.ErrSnapshotNotClosed
		}
	}
	if change.Next.State == JobPublic {
		proof := change.Validation
		if proof == nil || proof.jobs != l.jobs || !sameBackfillJob(proof.job, change.Expected) || proof.table != change.Table {
			return errIndexPublicationProof
		}
	}
	return nil
}

func (l *IndexLifecycle) dependencies(change IndexTransition) []txn.ReadDependency {
	var keys [2][8]byte
	binary.BigEndian.PutUint64(keys[0][:], uint64(change.Expected.TableID))
	binary.BigEndian.PutUint64(keys[1][:], uint64(change.Expected.MutationID))
	spans := make([]storage.KeySpan, len(keys))
	for i := range keys {
		bound := storage.KeyBound{Values: []model.Value{model.BytesVal(keys[i][:])}, Inclusive: true}
		spans[i] = storage.KeySpan{Lower: bound, Upper: bound}
	}
	result := []txn.ReadDependency{
		{Target: l.descriptorTarget, KeyLayout: l.descriptorKeys, Spans: spans},
		l.jobs.Dependencies(change.Expected.ID, change.Expected.TableID),
	}
	for _, dependency := range change.Dependencies {
		result = append(result, l.catalog.DescriptorDependency(dependency))
	}
	return result
}

func (l *IndexLifecycle) catalogChanges(ctx context.Context, transaction *txn.Transaction, change IndexTransition) (changes [2]catalog.Change, err error) {
	view, err := transaction.ReadView(ctx, l.dependencies(change))
	if err != nil {
		return changes, err
	}
	snapshot, err := l.catalog.TransactionSnapshot(view, catalog.OwnReader)
	if err != nil {
		return changes, errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	current, err := l.jobs.Get(ctx, view, change.Expected.ID)
	if err != nil {
		return changes, err
	}
	if !sameBackfillJob(current, change.Expected) {
		return changes, ErrJobChanged
	}
	if change.Expected.State == JobWriteOnly && change.Next.State == JobBackfilling {
		if change.Protected.Timestamp() > view.Timestamp() {
			return changes, storage.ErrSnapshotNotClosed
		}
		// Timestamp alone survives Close; the job read checks a live pin and
		// proves that its snapshot includes the write-only publication.
		protectedJob, err := l.jobs.Get(ctx, change.Protected, current.ID)
		if err != nil {
			return changes, err
		}
		if !sameBackfillJob(protectedJob, current) {
			return changes, ErrJobChanged
		}
	}
	table, err := snapshot.Descriptor(ctx, current.TableID)
	if err != nil {
		return changes, errors.Join(catalog.ErrDescriptorChanged, err)
	}
	index, err := snapshot.Descriptor(ctx, current.MutationID)
	if err != nil {
		return changes, errors.Join(catalog.ErrDescriptorChanged, err)
	}
	if !tableShapedKind(table.Descriptor.Kind()) || table.Descriptor.State() != model.StatePublic || index.Descriptor.Kind() != model.ObjectIndex || lifecycleTarget(table.Descriptor) != change.Table {
		return changes, catalog.ErrDescriptorChanged
	}
	if change.Next.State == JobPublic && lifecycleTarget(index.Descriptor) != change.Validation.index {
		return changes, errors.Join(errIndexPublicationProof, catalog.ErrDescriptorChanged)
	}
	return l.deriveChanges(ctx, snapshot, table.Descriptor, index.Descriptor, change)
}

func lifecycleTarget(descriptor *model.Descriptor) storage.ObjectRef {
	physical := descriptor.Physical()
	return storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
}

func (l *IndexLifecycle) deriveChanges(ctx context.Context, snapshot *catalog.CatalogSnapshot, table, index *model.Descriptor, change IndexTransition) (changes [2]catalog.Change, err error) {
	tableSpec, err := table.SpecWithin(ctx, l.limits)
	if err != nil {
		return changes, err
	}
	indexSpec, err := index.SpecWithin(ctx, l.limits)
	if err != nil {
		return changes, err
	}
	if tableSpec.Table == nil || indexSpec.Index == nil || indexSpec.Index.Table != table.ID() {
		return changes, model.ErrInvalidDescriptor
	}
	var expectedState model.SchemaState
	switch change.Expected.State {
	case JobPending:
		expectedState = model.StateAbsent
	case JobDeleteOnly, JobReverting:
		expectedState = model.StateDeleteOnly
	case JobWriteOnly, JobBackfilling, JobValidating:
		expectedState = model.StateWriteOnly
	case JobPublic:
		expectedState = model.StatePublic
	default:
		return changes, ErrJobTransition
	}
	if index.State() != expectedState {
		return changes, errors.Join(ErrJobTransition, catalog.ErrDescriptorChanged)
	}
	position := slices.Index(tableSpec.Table.Indexes, index.ID())
	if position < 0 && change.Expected.State != JobPending {
		return changes, catalog.ErrDescriptorChanged
	}
	pending := indexSpec.Index.PendingConstraint
	pendingDependencies := indexSpec.Index.PendingDependencies
	constraintPosition := -1
	if pending != nil {
		constraintPosition = slices.IndexFunc(tableSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
			return constraint.ID == pending.ID
		})
	}
	if change.Expected.State == JobReverting {
		if constraintPosition >= 0 {
			for _, dependency := range pendingDependencies {
				tableSpec.Dependencies = slices.DeleteFunc(tableSpec.Dependencies, func(existing model.Dependency) bool {
					return existing == dependency
				})
			}
			tableSpec.Table.Constraints = slices.Delete(tableSpec.Table.Constraints, constraintPosition, constraintPosition+1)
		}
		// Catalog tombstones retain old descriptors and index rows under MVCC.
		// Reclaiming their physical history is a separate retention operation.
		changes[1], err = l.catalog.Drop(ctx, snapshot, index.ID(), index.Version())
		if err != nil {
			return changes, err
		}
		tableSpec.Table.Indexes = slices.Delete(tableSpec.Table.Indexes, position, position+1)
		changes[0], err = l.successorChange(ctx, snapshot, table, tableSpec)
		return changes, err
	}
	nextState := expectedState
	switch change.Next.State {
	case JobDeleteOnly, JobReverting:
		nextState = model.StateDeleteOnly
	case JobWriteOnly:
		nextState = model.StateWriteOnly
	case JobPublic:
		nextState = model.StatePublic
	}
	if nextState == expectedState {
		return changes, nil
	}
	if position < 0 {
		if len(tableSpec.Table.Indexes) >= l.limits.MaxItems {
			return changes, model.ErrDescriptorLimit
		}
		tableSpec.Table.Indexes = append(tableSpec.Table.Indexes, index.ID())
	}
	if pending != nil {
		switch nextState {
		case model.StateDeleteOnly:
			if constraintPosition >= 0 {
				for _, dependency := range pendingDependencies {
					tableSpec.Dependencies = slices.DeleteFunc(tableSpec.Dependencies, func(existing model.Dependency) bool {
						return existing == dependency
					})
				}
				tableSpec.Table.Constraints = slices.Delete(tableSpec.Table.Constraints, constraintPosition, constraintPosition+1)
			}
		case model.StateWriteOnly:
			if constraintPosition >= 0 {
				return changes, catalog.ErrDescriptorChanged
			}
			transient := *pending
			transient.Deferrable = false
			transient.InitiallyDeferred = false
			transient.Validated = false
			transient.Mutation = true
			for _, dependency := range pendingDependencies {
				if !slices.Contains(tableSpec.Dependencies, dependency) {
					tableSpec.Dependencies = append(tableSpec.Dependencies, dependency)
				}
			}
			slices.SortFunc(tableSpec.Dependencies, func(left, right model.Dependency) int {
				if order := cmp.Compare(left.Object, right.Object); order != 0 {
					return order
				}
				if order := cmp.Compare(left.Version, right.Version); order != 0 {
					return order
				}
				return cmp.Compare(left.Column, right.Column)
			})
			tableSpec.Table.Constraints = append(tableSpec.Table.Constraints, transient)
		case model.StatePublic:
			public := *pending
			if constraintPosition < 0 {
				return changes, catalog.ErrDescriptorChanged
			}
			tableSpec.Table.Constraints[constraintPosition] = public
			if public.Kind == model.ConstraintPrimaryKey {
				for i := range tableSpec.Table.Columns {
					if slices.Contains(public.Columns, tableSpec.Table.Columns[i].ID) {
						tableSpec.Table.Columns[i].Nullable = false
					}
				}
			}
			indexSpec.Index.PendingConstraint = nil
			indexSpec.Index.PendingDependencies = nil
		}
	}
	indexSpec.State = nextState
	switch nextState {
	case model.StateAbsent, model.StateDeleteOnly:
		indexSpec.Index.Invalid = true
		indexSpec.Index.NotReady = true
		indexSpec.Index.NotLive = false
	case model.StateWriteOnly:
		indexSpec.Index.Invalid = true
		indexSpec.Index.NotReady = false
		indexSpec.Index.NotLive = false
	case model.StatePublic:
		indexSpec.Index.Invalid = false
		indexSpec.Index.NotReady = false
		indexSpec.Index.NotLive = false
	}
	changes[1], err = l.successorChange(ctx, snapshot, index, indexSpec)
	if err != nil {
		return changes, err
	}
	// Every policy change fences prepared base writers, even with identical columns.
	changes[0], err = l.successorChange(ctx, snapshot, table, tableSpec)
	return changes, err
}

func (l *IndexLifecycle) successorChange(ctx context.Context, snapshot *catalog.CatalogSnapshot, previous *model.Descriptor, spec model.DescriptorSpec) (catalog.Change, error) {
	if previous.Version() == ^uint64(0) {
		return catalog.Change{}, model.ErrInvalidDescriptor
	}
	spec.Version = previous.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, l.limits)
	if err != nil {
		return catalog.Change{}, err
	}
	return l.catalog.Update(ctx, snapshot, previous.Version(), next)
}
