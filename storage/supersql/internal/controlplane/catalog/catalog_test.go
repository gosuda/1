package catalog_test

import (
	"bytes"
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
)

func catalogLimits() catalog.Limits {
	return catalog.Limits{Descriptor: model.DescriptorLimits{MaxBytes: 4 << 20, MaxItems: 4096, MaxDepth: 64}, MaxEncodedBytes: 1 << 20, MaxKeyBytes: 4096, ScanBatchRows: 1}
}

type byteEncoder struct{}

func (byteEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, limit uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(layout) != 1 || len(values) != 1 {
		return nil, storage.ErrInvalidLayout
	}
	ascendingBytesKey := layout[0].Type.Storage == model.TypeBytes && !layout[0].Descending
	nonNullBytesValue := values[0].Type == model.TypeBytes && !values[0].IsNull
	if !ascendingBytesKey || !nonNullBytesValue {
		return nil, storage.ErrInvalidLayout
	}
	if uint64(len(values[0].BytesVal)) > limit {
		return nil, model.ErrValueBudget
	}
	return bytes.Clone(values[0].BytesVal), nil
}

type catalogClock struct {
	mu      sync.Mutex
	reading clock.Reading
	scale   clock.ScaleDescriptor
}

func newCatalogClock() *catalogClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: sha256.Sum256(history), CanonicalLeapHistory: history}
	return &catalogClock{scale: scale, reading: clock.Reading{Earliest: 100, Latest: 110, Status: clock.Synced, Epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}}}
}
func (c *catalogClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reading, nil
}
func (c *catalogClock) WaitUntilAfter(ctx context.Context, instant clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.reading.Epoch {
		return clock.ErrClockEpochChanged
	}
	if instant == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.reading.Earliest = max(c.reading.Earliest, instant+1)
	c.reading.Latest = max(c.reading.Latest, c.reading.Earliest)
	return nil
}
func (c *catalogClock) Scale() clock.ScaleDescriptor {
	s := c.scale
	s.CanonicalLeapHistory = bytes.Clone(s.CanonicalLeapHistory)
	return s
}
func (*catalogClock) FromUTC(t time.Time) (clock.Instant, error) {
	if t.Before(time.Unix(0, 0)) || t.After(time.Unix(0, math.MaxInt64)) {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(t.UnixNano()), nil
}
func (*catalogClock) ToUTC(t clock.Instant) (clock.UTCProjection, error) {
	if t < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: time.Unix(0, int64(t)).UTC()}, nil
}

type catalogResolver struct{ layouts []storage.ObjectLayout }

func (r catalogResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, layout := range r.layouts {
		if layout.Target == target {
			return txn.NewRowNormalizer(ctx, target, []txn.ColumnRule{{Layout: layout.Columns[0], KeyPosition: 1}, {Layout: layout.Columns[1]}}, 4<<20)
		}
	}
	return nil, storage.ErrInvalidTarget
}

type catalogFixture struct {
	store     *catalog.Store
	backend   *neosepia.Backend
	manager   *txn.Manager
	config    neosepia.Config
	authority *txn.FenceAuthority
	clock     *catalogClock
}

func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	f := &catalogFixture{clock: newCatalogClock()}
	var err error
	f.store, err = catalog.New(byteEncoder{}, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	f.authority, err = txn.NewFenceAuthority([32]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	f.config = neosepia.Config{Directory: t.TempDir(), Scale: f.clock.Scale(), KeyEncoder: byteEncoder{}, FenceVerifier: f.authority}
	f.open(t)
	t.Cleanup(func() {
		if err := f.manager.Close(); err != nil {
			t.Error(err)
		}
		if err := f.backend.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}
func (f *catalogFixture) open(t *testing.T) {
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
}
func (f *catalogFixture) snapshot(t *testing.T, ts model.Timestamp) *catalog.CatalogSnapshot {
	t.Helper()
	reader, err := f.manager.HistoricalSnapshot(t.Context(), ts)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.store.Snapshot(reader, ts, catalog.OwnReader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	})
	return snapshot
}
func (f *catalogFixture) begin(t *testing.T) *txn.Transaction {
	t.Helper()
	transaction, err := f.manager.Begin(t.Context(), txn.TransactionOptions{Mode: txn.ReadWrite, Resolver: catalogResolver{layouts: f.store.Layouts()}, KeyEncoder: byteEncoder{}, MemoryBytes: 16 << 20, SpillBytes: 16 << 20, SpillDirectory: t.TempDir(), MaxViews: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transaction.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	return transaction
}
func (f *catalogFixture) commit(t *testing.T, changes ...catalog.Change) model.Timestamp {
	t.Helper()
	transaction := f.begin(t)
	var mutations []storage.Mutation
	for _, change := range changes {
		mutations = append(mutations, change.Mutations...)
	}
	if err := transaction.ApplyMutations(t.Context(), mutations); err != nil {
		t.Fatal(err)
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return result.CommitTS
}
func tableDescriptor(t *testing.T, id model.ObjectID, name string, version uint64) *model.Descriptor {
	t.Helper()
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: id, OID: model.OID(id + 100), Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: name}, Version: version, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", Layout: model.TypeLayout{Type: typ}, State: model.StatePublic}}, PrimaryKey: []model.KeyElement{{Column: 1, Type: typ}}}}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func requireResolved(t *testing.T, snapshot *catalog.CatalogSnapshot, name model.NamespaceName, id model.ObjectID, version uint64, ts model.Timestamp) {
	t.Helper()
	got, err := snapshot.Resolve(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Descriptor.ID() != id || got.Descriptor.Version() != version || got.PublishedAt != ts {
		t.Fatalf("resolved %v: id=%d version=%d publication=%d", name, got.Descriptor.ID(), got.Descriptor.Version(), got.PublishedAt)
	}
}
func TestCatalogHistorySurvivesAtomicRenameDropAndReopen(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	first, second := tableDescriptor(t, 20, "MixedCase", 1), tableDescriptor(t, 21, "discarded", 1)
	a, err := f.store.Create(t.Context(), empty, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.store.Create(t.Context(), empty, second)
	if err != nil {
		t.Fatal(err)
	}
	created := f.commit(t, a, b)
	old := f.snapshot(t, created)
	renamed := tableDescriptor(t, 20, "Renamed", 2)
	rename, err := f.store.Update(t.Context(), old, 1, renamed)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := f.store.Drop(t.Context(), old, second.ID(), 1)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.commit(t, rename, drop)
	assertHistory := func() {
		before, after := f.snapshot(t, created), f.snapshot(t, changed)
		requireResolved(t, before, first.Name(), 20, 1, created)
		requireResolved(t, before, second.Name(), 21, 1, created)
		requireResolved(t, after, renamed.Name(), 20, 2, changed)
		for _, name := range []model.NamespaceName{first.Name(), second.Name()} {
			if _, err := after.Resolve(t.Context(), name); !errors.Is(err, catalog.ErrUnknownObject) {
				t.Fatalf("old name after change: %v", err)
			}
		}
		if _, err := before.Resolve(t.Context(), renamed.Name()); !errors.Is(err, catalog.ErrUnknownObject) {
			t.Fatalf("new name at old snapshot: %v", err)
		}
		byOID, err := before.ResolveOID(t.Context(), first.OID())
		if err != nil {
			t.Fatal(err)
		}
		if byOID.Descriptor.Name() != first.Name() {
			t.Fatal("OID resolved current instead of historical descriptor")
		}
		var names []string
		if err := after.ScanNamespace(t.Context(), 10, model.ObjectTable, func(d catalog.PublishedDescriptor) error { names = append(names, d.Descriptor.Name().Name); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "Renamed" {
			t.Fatalf("namespace scan = %v", names)
		}
		if err := before.Close(); err != nil {
			t.Fatal(err)
		}
		if err := after.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertHistory()
	requireResolved(t, old, first.Name(), 20, 1, created)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	assertHistory()
}
func TestFailedCatalogStatementPublishesNeitherNamespaceNorDescriptor(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	d := tableDescriptor(t, 20, "failed", 1)
	change, err := f.store.Create(t.Context(), empty, d)
	if err != nil {
		t.Fatal(err)
	}
	// A duplicate descriptor insert fails after the namespace and descriptor have
	// entered the same statement; txn must restore the entire statement overlay.
	change.Mutations = append(change.Mutations, change.Mutations[1])
	transaction := f.begin(t)
	err = transaction.ApplyMutations(t.Context(), change.Mutations)
	var state interface{ SQLState() string }
	if !errors.As(err, &state) || state.SQLState() != "23505" {
		t.Fatalf("duplicate descriptor batch: %v", err)
	}
	survivor := tableDescriptor(t, 21, "survivor", 1)
	valid, err := f.store.Create(t.Context(), empty, survivor)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(t.Context(), valid.Mutations); err != nil {
		t.Fatal(err)
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, result.CommitTS)
	if _, err := snapshot.Resolve(t.Context(), d.Name()); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("failed namespace visible: %v", err)
	}
	if _, err := snapshot.Descriptor(t.Context(), d.ID()); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("failed descriptor visible: %v", err)
	}
	requireResolved(t, snapshot, survivor.Name(), survivor.ID(), 1, result.CommitTS)
}
func TestCatalogDuplicateAndUnknownIdentities(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	d := tableDescriptor(t, 20, "existing", 1)
	change, err := f.store.Create(t.Context(), empty, d)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, f.commit(t, change))
	for _, duplicate := range []*model.Descriptor{tableDescriptor(t, 20, "different", 1), tableDescriptor(t, 21, "existing", 1)} {
		if _, err := f.store.Create(t.Context(), snapshot, duplicate); !errors.Is(err, catalog.ErrDuplicateObject) {
			t.Fatalf("duplicate identity accepted: %v", err)
		}
	}
	if _, err := f.store.Drop(t.Context(), snapshot, 999, 1); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("unknown drop: %v", err)
	}
	if _, err := f.store.Update(t.Context(), snapshot, 2, tableDescriptor(t, 20, "existing", 3)); !errors.Is(err, catalog.ErrDescriptorChanged) {
		t.Fatalf("stale update: %v", err)
	}
	folded := d.Name()
	folded.Name = "EXISTING"
	if _, err := snapshot.Resolve(t.Context(), folded); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("storage folded normalized identifier: %v", err)
	}
}

func (f *catalogFixture) transactionSnapshot(t *testing.T, transaction *txn.Transaction) *catalog.CatalogSnapshot {
	t.Helper()
	var dependencies []txn.ReadDependency
	for _, layout := range f.store.Layouts() {
		dependencies = append(dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: f.store.KeyLayout(), Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
	}
	view, err := transaction.ReadView(t.Context(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.store.TransactionSnapshot(view, catalog.OwnReader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	})
	return snapshot
}
func TestCreateRenameDropUsesOneAtomicTransaction(t *testing.T) {
	f := newCatalogFixture(t)
	transaction := f.begin(t)
	before := f.transactionSnapshot(t, transaction)
	original, discarded := tableDescriptor(t, 20, "original", 1), tableDescriptor(t, 21, "discarded", 1)
	a, err := f.store.Create(t.Context(), before, original)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.store.Create(t.Context(), before, discarded)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(t.Context(), append(a.Mutations, b.Mutations...)); err != nil {
		t.Fatal(err)
	}
	own := f.transactionSnapshot(t, transaction)
	requireResolved(t, own, original.Name(), 20, 1, 0)
	if _, err := before.Resolve(t.Context(), original.Name()); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("frozen pre-write view changed: %v", err)
	}
	renamed := tableDescriptor(t, 20, "renamed", 2)
	rename, err := f.store.Update(t.Context(), own, 1, renamed)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := f.store.Drop(t.Context(), own, discarded.ID(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(t.Context(), append(rename.Mutations, drop.Mutations...)); err != nil {
		t.Fatal(err)
	}
	requireResolved(t, own, original.Name(), 20, 1, 0)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	published := f.snapshot(t, result.CommitTS)
	requireResolved(t, published, renamed.Name(), 20, 2, result.CommitTS)
	for _, name := range []model.NamespaceName{original.Name(), discarded.Name()} {
		if _, err := published.Resolve(t.Context(), name); !errors.Is(err, catalog.ErrUnknownObject) {
			t.Fatalf("intermediate name was published: %v", err)
		}
	}
}

func TestAnalyzeSkipLockedUsesAtomicSchemaRelationAdmission(t *testing.T) {
	fixture := newCatalogFixture(t)
	contender := fixture.begin(t)
	holder := fixture.begin(t)
	const schema, relation, probeRelation model.ObjectID = 10, 11, 12
	if err := fixture.store.LockDescriptor(t.Context(), holder, relation); err != nil {
		t.Fatal(err)
	}
	acquired, err := fixture.store.LockDescriptorPairSkipLocked(t.Context(), contender, schema, relation)
	if err != nil || acquired {
		t.Fatalf("locked ANALYZE relation admission: acquired=%v err=%v", acquired, err)
	}
	if holder.State() != txn.Active {
		t.Fatalf("SKIP LOCKED wounded relation holder: %v", holder.State())
	}
	probe := fixture.begin(t)
	acquired, err = fixture.store.LockDescriptorPairSkipLocked(t.Context(), probe, probeRelation, schema)
	if err != nil || !acquired {
		t.Fatalf("skipped relation retained schema lock: acquired=%v err=%v", acquired, err)
	}
	result, err := contender.Commit(t.Context())
	if err != nil || result.AppliedCount != 0 {
		t.Fatalf("skipped ANALYZE changed persistent state: result=%+v err=%v", result, err)
	}
}
