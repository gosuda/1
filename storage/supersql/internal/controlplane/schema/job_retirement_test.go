package schema

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type retirementTestBackend struct {
	state    storage.RetentionState
	err      error
	requests []storage.GCRequest
	inspect  func() error
}

func (backend *retirementTestBackend) RetentionState(ctx context.Context) (storage.RetentionState, error) {
	return backend.state, ctx.Err()
}

func (backend *retirementTestBackend) Collect(ctx context.Context, request storage.GCRequest) error {
	backend.requests = append(backend.requests, request)
	if backend.inspect != nil {
		if err := backend.inspect(); err != nil {
			return err
		}
	}
	if backend.err != nil {
		return backend.err
	}
	return ctx.Err()
}

var errAckIndeterminate = errors.New("retirement acknowledgement indeterminate")

type indeterminateAckBackend struct {
	storage.Backend
	fail bool
}

func (backend *indeterminateAckBackend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	if backend.fail {
		return storage.ApplyResult{}, errAckIndeterminate
	}
	return backend.Backend.ApplyMutations(ctx, batch)
}

func (backend *indeterminateAckBackend) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	if backend.fail {
		return storage.CommitResolution{State: storage.CommitUnknown}, errAckIndeterminate
	}
	return backend.Backend.ResolveCommit(ctx, id)
}

func installIndeterminateAckManager(t *testing.T, fixture *indexRestartFixture) *indeterminateAckBackend {
	t.Helper()
	if err := fixture.protections.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.protections = nil
	if err := fixture.manager.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.manager = nil
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	backend := &indeterminateAckBackend{Backend: fixture.backend}
	fixture.manager, err = txn.NewManager(t.Context(), txn.ManagerConfig{Backend: backend, Clock: fixture.clock, Authority: fixture.authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.protections, err = NewJobProtections(JobProtectionOptions{Jobs: fixture.jobs, Snapshots: fixture.manager, MaxJobs: 16, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	view, err := fixture.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(fixture.protections.Restore(t.Context(), view), view.Close()); err != nil {
		t.Fatal(err)
	}
	return backend
}

func retirementTransaction(t *testing.T, fixture *indexRestartFixture) *txn.Transaction {
	t.Helper()
	options := fixture.options
	options.Resolver = fixture.jobs
	transaction, err := fixture.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func persistTerminalRetirement(t *testing.T, fixture *indexRestartFixture, id JobID, target storage.ObjectGeneration) model.Timestamp {
	t.Helper()
	transaction := retirementTransaction(t, fixture)
	defer func() {
		if err := transaction.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	job := Job{ID: id, TableID: 1, MutationID: model.ObjectID(1000 + id), Kind: AddIndexJob, State: JobPending, SourceVersion: 1, TargetVersion: 2, Revision: 1}
	err := transaction.Statement(t.Context(), func(ctx context.Context) error {
		if err := fixture.jobs.Create(ctx, transaction, job); err != nil {
			return err
		}
		if err := fixture.jobs.recordRetirements(ctx, transaction, id, []storage.ObjectGeneration{target}); err != nil {
			return err
		}
		for _, state := range []JobState{JobDeleteOnly, JobWriteOnly, JobBackfilling, JobValidating, JobPublic, JobSucceeded} {
			next := job
			next.State = state
			next.Revision++
			if state == JobBackfilling {
				next.SnapshotTS, next.ProtectedTimestamp = 1, 1
			}
			if state == JobSucceeded {
				next.ProtectedTimestamp = 0
			}
			if err := fixture.jobs.Update(ctx, transaction, job.Revision, next); err != nil {
				return err
			}
			job = next
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return result.CommitTS
}

func readRetirement(t *testing.T, fixture *indexRestartFixture, id JobID) (model.Timestamp, []storage.ObjectGeneration, error) {
	t.Helper()
	view, err := fixture.manager.CurrentReadView(t.Context())
	if err != nil {
		return 0, nil, err
	}
	timestamp, retired, readErr := fixture.jobs.retirements(t.Context(), view, id)
	return timestamp, retired, errors.Join(readErr, view.Close())
}

func TestRetirementJournalReplaysUntilCommittedAck(t *testing.T) {
	fixture := newIndexRestartFixture(t)
	target := storage.ObjectGeneration{ObjectID: 1, Generation: 1}
	committed := persistTerminalRetirement(t, fixture, 11, target)
	fixture.reopen(t)
	timestamp, retired, err := readRetirement(t, fixture, 11)
	if err != nil || timestamp != committed || !slices.Equal(retired, []storage.ObjectGeneration{target}) {
		t.Fatalf("replayed retirement = %d, %v, %v", timestamp, retired, err)
	}
	failedAck := retirementTransaction(t, fixture)
	if err := failedAck.Statement(t.Context(), func(ctx context.Context) error {
		return fixture.jobs.deleteRetirements(ctx, failedAck, 11)
	}); err != nil {
		t.Fatal(err)
	}
	if err := failedAck.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, retired, err := readRetirement(t, fixture, 11); err != nil || !slices.Equal(retired, []storage.ObjectGeneration{target}) {
		t.Fatalf("aborted acknowledgement lost journal: %v, %v", retired, err)
	}
}

func TestRetentionAckRequiresSuccessfulRemovalPublication(t *testing.T) {
	fixture := newIndexRestartFixture(t)
	target := storage.ObjectGeneration{ObjectID: 1, Generation: 1}
	retiredAt := persistTerminalRetirement(t, fixture, 12, target)
	fixture.clock.mu.Lock()
	fixture.clock.now = max(fixture.clock.now, clock.Instant(retiredAt+10))
	now := fixture.clock.now
	fixture.clock.mu.Unlock()
	backend := &retirementTestBackend{state: storage.RetentionState{ClosedTS: model.Timestamp(now), LeapHistoryID: fixture.clock.scale.LeapHistoryID}}
	coordinator := &RetentionCoordinator{options: RetentionOptions{Clock: fixture.clock, Backend: backend, Protections: fixture.protections, Jobs: fixture.jobs, MaxRetiredTargets: 16}}
	for _, failure := range []error{errors.New("collection failed"), storage.ErrCommitIndeterminate, context.Canceled} {
		backend.err = failure
		transaction := retirementTransaction(t, fixture)
		err := coordinator.advance(t.Context(), transaction, 0)
		if !errors.Is(err, failure) {
			t.Fatalf("collection failure %v returned %v", failure, err)
		}
		if err := transaction.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRetirement(t, fixture, 12); err != nil {
			t.Fatalf("failed or indeterminate removal acknowledged journal: %v", err)
		}
	}
	backend.err = nil
	fault := installIndeterminateAckManager(t, fixture)
	coordinator.options.Protections = fixture.protections
	transaction := retirementTransaction(t, fixture)
	backend.inspect = func() error {
		_, _, err := fixture.jobs.retirements(t.Context(), transaction, 12)
		return err
	}
	if err := coordinator.advance(t.Context(), transaction, 0); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) == 0 || len(backend.requests[len(backend.requests)-1].Retired) != 1 {
		t.Fatal("retirement acknowledgement was staged before removal publication")
	}
	if _, _, err := fixture.jobs.retirements(t.Context(), transaction, 12); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("successful removal did not stage acknowledgement: %v", err)
	}
	fault.fail = true
	if _, err := transaction.Commit(t.Context()); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("acknowledgement commit was not indeterminate: %v", err)
	}
	fault.fail = false
	resolution, err := transaction.Resolve(t.Context())
	if err != nil || resolution.State != storage.CommitNotCommitted {
		t.Fatalf("indeterminate acknowledgement resolution: %+v, %v", resolution, err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRetirement(t, fixture, 12); err != nil {
		t.Fatalf("indeterminate acknowledgement lost journal: %v", err)
	}
	transaction = retirementTransaction(t, fixture)
	backend.inspect = func() error {
		_, _, err := fixture.jobs.retirements(t.Context(), transaction, 12)
		return err
	}
	if err := coordinator.advance(t.Context(), transaction, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRetirement(t, fixture, 12); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("committed acknowledgement retained journal: %v", err)
	}
}

func TestActiveBackfillProtectionDefersRetirement(t *testing.T) {
	fixture := newIndexRestartFixture(t)
	target := storage.ObjectGeneration{ObjectID: 1, Generation: 1}
	retiredAt := persistTerminalRetirement(t, fixture, 13, target)
	protectedAt := retiredAt - 1
	reader, err := fixture.manager.HistoricalSnapshot(t.Context(), protectedAt)
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: 99, TableID: 1, MutationID: 99, Kind: RewriteGenerationJob, State: JobBackfilling, SourceVersion: 1, TargetVersion: 4, Revision: 4, SnapshotTS: protectedAt, ProtectedTimestamp: protectedAt}
	if err := fixture.protections.Retain(t.Context(), job, reader); err != nil {
		t.Fatal(err)
	}
	fixture.clock.mu.Lock()
	fixture.clock.now = max(fixture.clock.now, clock.Instant(retiredAt+10))
	now := fixture.clock.now
	fixture.clock.mu.Unlock()
	backend := &retirementTestBackend{state: storage.RetentionState{ClosedTS: model.Timestamp(now), LeapHistoryID: fixture.clock.scale.LeapHistoryID}}
	coordinator := &RetentionCoordinator{options: RetentionOptions{Clock: fixture.clock, Backend: backend, Protections: fixture.protections, Jobs: fixture.jobs, MaxRetiredTargets: 16}}
	transaction := retirementTransaction(t, fixture)
	defer func() {
		if err := transaction.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := coordinator.advance(t.Context(), transaction, 0); err != nil {
		t.Fatal(err)
	}
	request := backend.requests[len(backend.requests)-1]
	if request.Boundary != protectedAt || len(request.Retired) != 0 {
		t.Fatalf("active backfill protection admitted retirement: %+v", request)
	}
}

func TestOrdinaryDDLRetirementCollectsWithoutSyntheticJob(t *testing.T) {
	fixture := newIndexRestartFixture(t)
	id := RetirementID(21)
	target := storage.ObjectGeneration{ObjectID: 1, Generation: 1}
	dependency, err := fixture.jobs.RetirementDependency(id)
	if err != nil {
		t.Fatal(err)
	}
	transaction := retirementTransaction(t, fixture)
	if err := transaction.Statement(t.Context(), func(ctx context.Context) error {
		view, err := transaction.ReadView(ctx, []txn.ReadDependency{dependency})
		if err != nil {
			return err
		}
		if err := view.Close(); err != nil {
			return err
		}
		return fixture.jobs.StageRetirements(ctx, transaction, id, []storage.ObjectGeneration{target})
	}); err != nil {
		t.Fatal(err)
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.clock.mu.Lock()
	fixture.clock.now = max(fixture.clock.now, clock.Instant(result.CommitTS+10))
	now := fixture.clock.now
	fixture.clock.mu.Unlock()
	backend := &retirementTestBackend{state: storage.RetentionState{ClosedTS: model.Timestamp(now), LeapHistoryID: fixture.clock.scale.LeapHistoryID}}
	coordinator := &RetentionCoordinator{options: RetentionOptions{Clock: fixture.clock, Backend: backend, Protections: fixture.protections, Jobs: fixture.jobs, MaxRetiredTargets: 16}}
	transaction = retirementTransaction(t, fixture)
	backend.inspect = func() error {
		_, retired, err := fixture.jobs.ordinaryRetirements(t.Context(), transaction, id)
		if err != nil {
			return err
		}
		if !slices.Equal(retired, []storage.ObjectGeneration{target}) {
			return ErrJobRecord
		}
		return nil
	}
	if err := coordinator.advance(t.Context(), transaction, 0); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 || len(backend.requests[0].Retired) != 1 || backend.requests[0].Retired[0].Target() != target {
		t.Fatalf("ordinary retirement request = %+v", backend.requests)
	}
	if _, err := transaction.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	view, err := fixture.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, _, readErr := fixture.jobs.ordinaryRetirements(t.Context(), view, id)
	if err := errors.Join(readErr, view.Close()); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("ordinary retirement acknowledgement retained journal: %v", err)
	}
}

// A clock younger than the retention window has no collectible boundary. The
// advance must return nil without asking the backend to collect; the prior
// behavior failed the whole Close with a timestamp range error.
func TestRetentionSkipsCollectionWhileClockIsYoungerThanWindow(t *testing.T) {
	fixture := newIndexRestartFixture(t)
	backend := &retirementTestBackend{state: storage.RetentionState{ClosedTS: 1, LeapHistoryID: fixture.clock.scale.LeapHistoryID}}
	coordinator := &RetentionCoordinator{options: RetentionOptions{Clock: fixture.clock, Backend: backend, Protections: fixture.protections, Jobs: fixture.jobs, MaxRetiredTargets: 16}}
	transaction := retirementTransaction(t, fixture)
	if err := coordinator.advance(t.Context(), transaction, time.Hour); err != nil {
		t.Fatalf("advance on young clock: %v", err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 0 {
		t.Fatalf("collected on young clock: %+v", backend.requests)
	}
}
