package schema

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type restartClock struct {
	mu    sync.Mutex
	now   clock.Instant
	scale clock.ScaleDescriptor
	epoch clock.Epoch
}

func (c *restartClock) Read(ctx context.Context) (clock.Reading, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, ctx.Err()
}
func (c *restartClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if value < 0 || value == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.now = max(c.now, value+1)
	return nil
}
func (c *restartClock) Scale() clock.ScaleDescriptor {
	scale := c.scale
	scale.CanonicalLeapHistory = append([]byte(nil), scale.CanonicalLeapHistory...)
	return scale
}
func (*restartClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}
func (*restartClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(0, int64(value)).UTC()}, nil
}

type restartResolvers map[storage.ObjectRef]*txn.RowNormalizer

func (r restartResolvers) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer, ok := r[target]; ok {
		return normalizer, nil
	}
	return nil, storage.ErrInvalidTarget
}

type indexRestartFixture struct {
	config      neosepia.Config
	backend     *neosepia.Backend
	manager     *txn.Manager
	clock       *restartClock
	authority   *txn.FenceAuthority
	catalog     *catalog.Store
	jobs        *JobStore
	protections *JobProtections
	policies    *IndexPolicyProvider
	lifecycle   *IndexLifecycle
	keys        *builtin.KeyEncoder
	limits      model.DescriptorLimits
	indexLimits IndexLimits
	options     txn.TransactionOptions
	layout      []storage.ColumnLayout
	keyLayout   []storage.KeyColumn
}

func newIndexRestartFixture(t *testing.T) *indexRestartFixture {
	t.Helper()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	id := sha256.Sum256(history)
	f := &indexRestartFixture{clock: &restartClock{now: 100, scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: id, CanonicalLeapHistory: history}, epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: id}}, limits: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}, indexLimits: IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 64 << 10, MaxColumns: 16}}
	var err error
	f.keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	f.authority, err = txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	f.config = neosepia.Config{Directory: t.TempDir(), Scale: f.clock.Scale(), KeyEncoder: f.keys, FenceVerifier: f.authority}
	f.options = txn.TransactionOptions{Mode: txn.ReadWrite, KeyEncoder: f.keys, MemoryBytes: 16 << 20, DisableSpill: true, MaxViews: 16}
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	f.layout = []storage.ColumnLayout{{ID: 1, Type: integer}, {ID: 2, Type: integer}}
	f.keyLayout = []storage.KeyColumn{{ColumnID: 1, Type: integer}}
	t.Cleanup(func() {
		if err := f.close(); err != nil {
			t.Error(err)
		}
	})
	f.open(t)
	schema, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 2, OID: 10002, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: 1}}, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	table, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 1, OID: 10001, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "items"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}}, {ID: 2, Name: "value", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}}}, PrimaryKey: []model.KeyElement{{Column: 1, Type: integer}}}}, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	resolvers := restartResolvers{}
	for _, layout := range f.catalog.Layouts() {
		normalizer, err := txn.NewRowNormalizer(t.Context(), layout.Target, []txn.ColumnRule{{Layout: layout.Columns[0], KeyPosition: 1}, {Layout: layout.Columns[1]}}, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		resolvers[layout.Target] = normalizer
	}
	options := f.options
	options.Resolver = resolvers
	transaction, err := f.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := transaction.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for _, descriptor := range []*model.Descriptor{schema, table} {
		dependencies, err := f.catalog.CreateDependencies(t.Context(), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		view, err := transaction.ReadView(t.Context(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			t.Fatal(err)
		}
		change, err := f.catalog.Create(t.Context(), snapshot, descriptor)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := transaction.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *indexRestartFixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.backend, err = neosepia.Open(t.Context(), f.config)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = txn.NewManager(t.Context(), txn.ManagerConfig{Backend: f.backend, Clock: f.clock, Authority: f.authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
	if err != nil {
		t.Fatal(err)
	}
	f.catalog, err = catalog.New(f.keys, catalog.Limits{Descriptor: f.limits, MaxEncodedBytes: 2 << 20, MaxKeyBytes: 4096, ScanBatchRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.jobs, err = NewJobStore(t.Context(), JobStoreOptions{Target: storage.ObjectRef{ObjectID: 4, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.keys, MetadataBytes: 1 << 20, RecordBytes: 64 << 10, KeyBytes: 4096, MaxJobs: 16, BatchRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.protections, err = NewJobProtections(JobProtectionOptions{Jobs: f.jobs, Snapshots: f.manager, MaxJobs: 16, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(f.protections.Restore(t.Context(), view), view.Close())
	if err != nil {
		t.Fatal(err)
	}
	f.policies, err = NewIndexPolicyProvider(t.Context(), IndexPolicyOptions{Catalog: f.catalog, Views: f.manager, Jobs: f.jobs, Encoders: func(context.Context, *catalog.CatalogSnapshot) (KeyCodec, error) { return f.keys, nil }, Limits: f.indexLimits, Base: restartTablePolicy{layout: f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	f.lifecycle, err = NewIndexLifecycle(IndexLifecycleOptions{Catalog: f.catalog, Jobs: f.jobs, Limits: f.limits})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *indexRestartFixture) close() error {
	var err error
	if f.protections != nil {
		err = errors.Join(err, f.protections.Close())
		f.protections = nil
	}
	if f.manager != nil {
		err = errors.Join(err, f.manager.Close())
		f.manager = nil
	}
	if f.backend != nil {
		err = errors.Join(err, f.backend.Close())
		f.backend = nil
	}
	return err
}
func (f *indexRestartFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
}
func (f *indexRestartFixture) descriptor(t *testing.T, reader storage.SnapshotReader, timestamp model.Timestamp, id model.ObjectID) (*model.Descriptor, error) {
	t.Helper()
	snapshot, err := f.catalog.Snapshot(reader, timestamp, catalog.BorrowReader)
	if err != nil {
		return nil, err
	}
	descriptor, err := snapshot.Descriptor(t.Context(), id)
	return descriptor.Descriptor, errors.Join(err, snapshot.Close())
}
func (f *indexRestartFixture) policy(t *testing.T) IndexWritePolicy {
	t.Helper()
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	table, err := f.descriptor(t, view, view.Timestamp(), 1)
	err = errors.Join(err, view.Close())
	if err != nil {
		t.Fatal(err)
	}
	physical := table.Physical()
	policy, err := f.policies.TablePolicy(t.Context(), storage.ObjectRef{ObjectID: 1, Generation: physical.Generation, EncodingVersion: physical.EncodingVersion, DescriptorVersion: table.Version()})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
func (f *indexRestartFixture) request(t *testing.T, target storage.ObjectRef, id int64) storage.ReadRequest {
	t.Helper()
	key, err := f.keys.EncodePrefix(t.Context(), f.keyLayout, []model.Value{model.Int64Val(id)}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return storage.ReadRequest{Target: target, Key: key, ColumnIDs: []model.ColumnID{1, 2}, Layout: f.layout}
}
func (f *indexRestartFixture) write(t *testing.T, op storage.MutationOp, id, value int64) error {
	t.Helper()
	policy := f.policy(t)
	options := f.options
	options.Resolver = policy.Resolver
	transaction, err := f.manager.Begin(t.Context(), options)
	if err != nil {
		return err
	}
	defer func() {
		if err := transaction.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	request := f.request(t, policy.Table, id)
	err = transaction.Statement(t.Context(), func(ctx context.Context) error {
		var old *model.Row
		row, err := transaction.Get(ctx, request)
		if err == nil {
			old = &row
		} else if !errors.Is(err, storage.ErrRowNotFound) {
			return err
		}
		mutation := storage.Mutation{Op: op, Target: policy.Table, Key: request.Key}
		var current *model.Row
		if op != storage.MutationDelete {
			mutation.ColumnIDs = request.ColumnIDs
			mutation.Values = []model.Value{model.Int64Val(id), model.Int64Val(value)}
			current = &model.Row{Key: request.Key, Values: mutation.Values}
		}
		if err := transaction.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
			return err
		}
		return policy.Indexes.Expand(ctx, transaction, old, current)
	})
	if err != nil {
		return err
	}
	_, err = transaction.Commit(t.Context())
	return err
}
func (f *indexRestartFixture) submit(t *testing.T) Job {
	t.Helper()
	policy := f.policy(t)
	index, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 3, OID: 10003, Version: 1, State: model.StateAbsent, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "unique_values"}, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Index: &model.IndexMetadata{Table: 1, Unique: true, Keys: []model.KeyElement{{Column: 2, Type: f.layout[1].Type}}}}, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewAddIndexJob(1, policy.Table, index.ID())
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.lifecycle.SubmitIntent(t.Context(), IndexSubmissionOptions{Manager: f.manager, Policies: f.policies, Table: policy.Table, Index: index, Job: job, Transaction: f.options})
	if submission != nil {
		defer func() {
			if err := submission.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := submission.Resolve(t.Context())
	if err != nil || resolution.State != storage.CommitCommitted {
		t.Fatalf("intent outcome = %+v, %v", resolution, err)
	}
	return job
}

// Stop before the next step, after the real provider has observed a durable revision.
type revisionBoundary struct {
	IndexPolicies
	revision uint64
}

func (p revisionBoundary) Policy(ctx context.Context, job Job) (IndexWritePolicy, error) {
	policy, err := p.IndexPolicies.Policy(ctx, job)
	if err == nil && job.Revision > p.revision {
		return IndexWritePolicy{}, context.Canceled
	}
	return policy, err
}
func (f *indexRestartFixture) runner(t *testing.T, policies IndexPolicies) *IndexJobRunner {
	t.Helper()
	runner, err := NewIndexJobRunner(t.Context(), 1, IndexJobRunnerOptions{Manager: f.manager, Policies: policies, Jobs: f.jobs, Lifecycle: f.lifecycle, Protections: f.protections, Transaction: f.options, Scan: BackfillScanLimits{BatchRows: 2, MaxKeyBytes: 4096, WorkBytes: 4 << 20}, Backfill: BackfillLimits{MaxRows: 2, MaxKeyBytes: 8192, WorkBytes: 4 << 20}, Validation: IndexValidationLimits{BatchRows: 2, MaxRows: 100, MaxBytes: 8 << 20}, RetryDelay: time.Millisecond, MaxRetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return runner
}

// Index publication changes descriptor versions, not this fixture's row layout.
// The enclosing checked resolver validates the current descriptor under its lock.
type restartTablePolicy struct{ layout []storage.ColumnLayout }

func (policy restartTablePolicy) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if target.ObjectID != 1 || target.Generation != 1 || target.EncodingVersion != 1 || target.DescriptorVersion == 0 {
		return nil, storage.ErrInvalidTarget
	}
	return txn.NewRowNormalizer(ctx, target, []txn.ColumnRule{{Layout: policy.layout[0], KeyPosition: 1}, {Layout: policy.layout[1]}}, 4<<20)
}
