package txn

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type commitFixtureClock struct {
	*sequencerTestClock
	scale     clock.ScaleDescriptor
	automatic bool
}

func newCommitFixtureClock() *commitFixtureClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	provider := &commitFixtureClock{sequencerTestClock: newSequencerTestClock(), automatic: true, scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: sha256.Sum256(history), CanonicalLeapHistory: history}}
	provider.reading.Epoch.LeapHistory = provider.scale.LeapHistoryID
	return provider
}
func (provider *commitFixtureClock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	provider.mu.Lock()
	automatic, reading := provider.automatic, provider.reading
	provider.mu.Unlock()
	if !automatic {
		return provider.sequencerTestClock.WaitUntilAfter(ctx, target, epoch)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reading.Epoch != epoch {
		return clock.ErrClockEpochChanged
	}
	if reading.Status != clock.Synced && reading.Status != clock.Holdover {
		return clock.ErrClockUnavailable
	}
	if target < 0 || target == clock.Instant(math.MaxInt64) {
		return clock.ErrClockRange
	}
	provider.advance(max(reading.Earliest, target+1), max(reading.Latest, target+1))
	return nil
}
func (provider *commitFixtureClock) Scale() clock.ScaleDescriptor {
	scale := provider.scale
	scale.CanonicalLeapHistory = append([]byte(nil), scale.CanonicalLeapHistory...)
	return scale
}
func (*commitFixtureClock) FromUTC(value time.Time) (clock.Instant, error) {
	if value.Before(time.Unix(0, 0)) || value.After(time.Unix(0, math.MaxInt64)) {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(value.UnixNano()), nil
}
func (*commitFixtureClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	if value < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: time.Unix(0, int64(value)).UTC()}, nil
}

type commitFixtureEncoder struct{}

func (commitFixtureEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maximum uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(layout) != 1 || len(values) != 1 {
		return nil, storage.ErrInvalidScanPlan
	}
	if layout[0].Type != (model.Type{ID: 20, Storage: model.TypeInt64}) || values[0].Type != model.TypeInt64 || values[0].IsNull {
		return nil, storage.ErrInvalidScanPlan
	}
	if maximum < 8 {
		return nil, columnar.ErrBatchBudget
	}
	value := uint64(values[0].IntVal) ^ (uint64(1) << 63)
	if layout[0].Descending {
		value = ^value
	}
	return binary.BigEndian.AppendUint64(nil, value), nil
}

type commitFixture struct {
	manager   *Manager
	backend   *neosepia.Backend
	clock     *commitFixtureClock
	request   storage.ReadRequest
	config    neosepia.Config
	authority *FenceAuthority
}

func newCommitFixture(t *testing.T, wrap func(storage.Backend) storage.Backend) commitFixture {
	t.Helper()
	provider := newCommitFixtureClock()
	authority, err := NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := neosepia.Config{Directory: t.TempDir(), Scale: provider.Scale(), KeyEncoder: commitFixtureEncoder{}, FenceVerifier: authority}
	backend, err := neosepia.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	request := storage.ReadRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Key: binary.BigEndian.AppendUint64(nil, (uint64(1)<<63)|1), ColumnIDs: []model.ColumnID{1}, Layout: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}
	fixture := commitFixture{backend: backend, clock: provider, request: request, config: config, authority: authority}
	seed := fixture.batch(1, 100)
	seed.CommitTS, seed.Epoch = 20, provider.reading.Epoch
	seed.ChecksumCRC, err = seed.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	var implementation storage.Backend = backend
	if wrap != nil {
		implementation = wrap(backend)
	}
	fixture.manager = openCommitManager(t, &fixture, implementation)
	return fixture
}
func openCommitManager(t *testing.T, fixture *commitFixture, backend storage.Backend) *Manager {
	t.Helper()
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(t.Context(), ManagerConfig{Backend: backend, Clock: fixture.clock, Authority: fixture.authority, MaxOperations: 8, Recovered: RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager
}

func reopenCommitFixture(t *testing.T, fixture *commitFixture) {
	t.Helper()
	if err := fixture.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err := neosepia.Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = backend
	fixture.manager = openCommitManager(t, fixture, backend)
}

func (fixture commitFixture) batch(id byte, value int64) storage.MutationBatch {
	mutation := storage.NewUpsertMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(value)})
	mutation.Ordinal = 1
	return storage.MutationBatch{TxID: model.TxnID{id}, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: fixture.request.Target, Columns: fixture.request.Layout}}, Mutations: []storage.Mutation{mutation}}
}
func (fixture commitFixture) balance(t *testing.T, reader storage.SnapshotReader, want int64) {
	t.Helper()
	row, err := reader.Get(t.Context(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].IntVal != want {
		t.Fatalf("balance: got %+v, want %d", row.Values, want)
	}
}

func TestManagerCommitWaitsForClockBeforeWAL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		fixture.manager.sequencer.anchored = false
		fixture.clock.mu.Lock()
		fixture.clock.automatic = false
		fixture.clock.mu.Unlock()
		done := make(chan error, 1)
		var submission managerSubmission
		go func() {
			var err error
			submission, err = fixture.manager.submit(t.Context(), fixture.batch(2, 90), false)
			done <- err
		}()
		target := <-fixture.clock.entered
		fixture.clock.advance(target, target+10)
		synctest.Wait()
		resolution, err := fixture.backend.ResolveCommit(t.Context(), model.TxnID{2})
		if err != nil || resolution.State != storage.CommitNotCommitted {
			t.Fatalf("equality submitted a WAL decision: %+v %v", resolution, err)
		}
		fixture.clock.advance(target+1, target+10)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if submission.State != storage.CommitCommitted {
			t.Fatalf("successful submission state: %v", submission.State)
		}
		applied := submission.Result
		if applied.CommitTS != model.Timestamp(target) || applied.LogPosition != 2 {
			t.Fatalf("certified commit result: %+v, target %d", applied, target)
		}
		reader, err := fixture.manager.HistoricalSnapshot(t.Context(), applied.CommitTS)
		if err != nil {
			t.Fatal(err)
		}
		fixture.balance(t, reader, 90)
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		fixture.manager.sequencer.anchored = false
		ctx, cancel := context.WithCancel(t.Context())
		var canceled managerSubmission
		go func() {
			var err error
			canceled, err = fixture.manager.submit(ctx, fixture.batch(3, 80), false)
			done <- err
		}()
		<-fixture.clock.entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("pre-fence cancellation became uncertain: %v", err)
		}
		if canceled.State != storage.CommitNotCommitted {
			t.Fatalf("pre-fence submission outcome: %v", canceled.State)
		}
		resolution, err = fixture.backend.ResolveCommit(t.Context(), model.TxnID{3})
		if err != nil || resolution.State != storage.CommitNotCommitted {
			t.Fatalf("canceled fence submitted a decision: %+v %v", resolution, err)
		}
	})
}

func TestRecoveredCommitFrontierRequiresFreshStrictFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		fixture.clock.advance(190, 200)
		prior, err := fixture.manager.submit(t.Context(), fixture.batch(2, 90), false)
		if err != nil || prior.State != storage.CommitCommitted || prior.Result.CommitTS != 200 {
			t.Fatalf("prior commit=%+v error=%v", prior, err)
		}
		fixture.clock.advance(100, 110)
		reopenCommitFixture(t, &fixture)
		recovered, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		target := clock.Instant(recovered.ClosedTS + 1)
		fixture.clock.advance(100, 110)
		fixture.manager.sequencer.anchored = false
		fixture.clock.mu.Lock()
		fixture.clock.automatic = false
		fixture.clock.mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan struct{})
		var outcome managerSubmission
		var commitErr error
		go func() {
			defer close(finished)
			outcome, commitErr = fixture.manager.submit(ctx, fixture.batch(3, 80), false)
		}()
		defer func() { cancel(); <-finished }()
		select {
		case actual := <-fixture.clock.entered:
			if actual != target {
				t.Fatalf("recovered fence target=%d, want %d", actual, target)
			}
		case <-finished:
			t.Fatalf("commit bypassed recovered clock fence: %+v, %v", outcome, commitErr)
		}
		fixture.clock.advance(target, target+10)
		synctest.Wait()
		resolution, err := fixture.backend.ResolveCommit(t.Context(), model.TxnID{3})
		if err != nil || resolution.State != storage.CommitNotCommitted {
			t.Fatalf("equality published a recovered commit: %+v, %v", resolution, err)
		}
		state, err := fixture.backend.State(t.Context())
		if err != nil || state.LogPosition != prior.Result.LogPosition || state.CommitTS != prior.Result.CommitTS {
			t.Fatalf("clock wait changed recovered WAL frontier: %+v, %v", state, err)
		}
		fixture.clock.advance(target+1, target+11)
		<-finished
		if commitErr != nil || outcome.State != storage.CommitCommitted || outcome.Result.CommitTS != model.Timestamp(target) {
			t.Fatalf("strict recovered fence result=%+v error=%v", outcome, commitErr)
		}
		reader, err := fixture.manager.HistoricalSnapshot(t.Context(), outcome.Result.CommitTS)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		}()
		fixture.balance(t, reader, 80)
	})
}

type clockLostAfterFence struct {
	sequencingClock
	lost bool
}

func (provider *clockLostAfterFence) Read(ctx context.Context) (clock.Reading, error) {
	if provider.lost {
		return clock.Reading{}, clock.ErrClockUnavailable
	}
	return provider.sequencingClock.Read(ctx)
}

func (provider *clockLostAfterFence) WaitUntilAfter(ctx context.Context, timestamp clock.Instant, epoch clock.Epoch) error {
	if err := provider.sequencingClock.WaitUntilAfter(ctx, timestamp, epoch); err != nil {
		return err
	}
	provider.lost = true
	return nil
}

func TestFreshClockFailureAfterWaitCannotSubmitWAL(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	fixture.manager.sequencer.anchored = false
	fixture.manager.sequencer.provider = &clockLostAfterFence{sequencingClock: fixture.clock}
	outcome, err := fixture.manager.submit(t.Context(), fixture.batch(2, 90), false)
	if !errors.Is(err, clock.ErrClockUnavailable) || errors.Is(err, storage.ErrCommitIndeterminate) || outcome.State != storage.CommitNotCommitted {
		t.Fatalf("fresh clock failure outcome=%+v error=%v", outcome, err)
	}
	resolution, err := fixture.backend.ResolveCommit(t.Context(), model.TxnID{2})
	if err != nil || resolution.State != storage.CommitNotCommitted {
		t.Fatalf("stale clock evidence submitted WAL: %+v, %v", resolution, err)
	}
	reader, err := fixture.manager.HistoricalSnapshot(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	fixture.balance(t, reader, 100)
}

type cancelAfterCommitBackend struct {
	storage.Backend
	cancel context.CancelFunc
}

func (backend *cancelAfterCommitBackend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	result, err := backend.Backend.ApplyMutations(ctx, batch)
	if err != nil {
		return result, err
	}
	backend.cancel()
	return result, context.Canceled
}

func TestManagerResolvesDurabilityWithoutClockAndFencesNewWork(t *testing.T) {
	var injected *cancelAfterCommitBackend
	fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
		injected = &cancelAfterCommitBackend{Backend: backend}
		return injected
	})
	historical, err := fixture.manager.HistoricalSnapshot(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := historical.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	injected.cancel = cancel
	submission, err := fixture.manager.submit(ctx, fixture.batch(2, 90), false)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("post-durability cancellation: %v", err)
	}
	if submission.State != storage.CommitUnknown {
		t.Fatalf("unresolved submission state: %v", submission.State)
	}
	fixture.clock.mu.Lock()
	fixture.clock.reading.Status = 0
	fixture.clock.mu.Unlock()
	if blocked, err := fixture.manager.submit(t.Context(), fixture.batch(3, 80), false); !errors.Is(err, storage.ErrCommitIndeterminate) || blocked.State != storage.CommitNotCommitted {
		t.Fatalf("write bypassed unresolved decision: %v", err)
	}
	blocked, err := fixture.manager.StrongSnapshot(t.Context())
	if blocked != nil {
		if closeErr := blocked.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("strong barrier bypassed unresolved decision: %v", err)
	}
	fixture.balance(t, historical, 100)
	resolution, err := fixture.manager.ResolveCommit(t.Context(), model.TxnID{2})
	if err != nil || resolution.State != storage.CommitCommitted || resolution.Result.LogPosition != 2 {
		t.Fatalf("clock-free commit resolution: %+v %v", resolution, err)
	}
	if submission.State != storage.CommitUnknown {
		t.Fatalf("independent resolution rewrote the original submission outcome: %v", submission.State)
	}
	repeat, err := fixture.manager.ResolveCommit(t.Context(), model.TxnID{2})
	if err != nil || repeat != resolution {
		t.Fatalf("resolution changed its durable decision: %+v %v", repeat, err)
	}
	exact, err := fixture.manager.HistoricalSnapshot(t.Context(), resolution.Result.CommitTS)
	if err != nil {
		t.Fatal(err)
	}
	fixture.balance(t, exact, 90)
	if err := exact.Close(); err != nil {
		t.Fatal(err)
	}
	blocked, err = fixture.manager.StrongSnapshot(t.Context())
	if blocked != nil {
		if closeErr := blocked.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, clock.ErrClockUnavailable) {
		t.Fatalf("clock loss permitted a new strong barrier: %v", err)
	}
}

type epochOnSequenceOpen struct {
	storage.MutationSequence
	provider *commitFixtureClock
}

func (sequence epochOnSequenceOpen) Open(ctx context.Context) (storage.MutationCursor, error) {
	sequence.provider.mu.Lock()
	sequence.provider.reading.Epoch.Config[0]++
	sequence.provider.mu.Unlock()
	return sequence.MutationSequence.Open(ctx)
}

func TestManagerRevalidatesEpochAfterSpilledBatchHashing(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	limits := mutationLogTestLimits(t, true)
	log := mutationLogTestOpen(t, limits)
	batch := fixture.batch(2, 90)
	mutation := batch.Mutations[0]
	batch.Mutations = make([]storage.Mutation, 512)
	for i := range batch.Mutations {
		mutation.Ordinal = uint64(i + 1)
		batch.Mutations[i] = mutation
	}
	if err := log.append(t.Context(), batch.Mutations); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(limits.SpillDirectory)
	if err != nil || len(files) != 1 {
		t.Fatalf("expected an owned spill before hashing: %v, %v", files, err)
	}
	sequence := mutationLogTestSeal(t, log)
	batch.Mutations = nil
	batch.Sequence = epochOnSequenceOpen{MutationSequence: sequence, provider: fixture.clock}
	if _, err := fixture.manager.submit(t.Context(), batch, false); !errors.Is(err, clock.ErrClockEpochChanged) || errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("encoding crossed assurance epochs: %v", err)
	}
	resolution, err := fixture.backend.ResolveCommit(t.Context(), batch.TxID)
	if err != nil || resolution.State != storage.CommitNotCommitted {
		t.Fatalf("changed epoch reached WAL: %+v %v", resolution, err)
	}
}
