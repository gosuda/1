package schema

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errSequenceFault = errors.New("injected sequence durability fault")

type sequenceFaultBackend struct {
	storage.Backend
	mu                 sync.Mutex
	failApply, durable bool
	resolveError       error
	cancel             context.CancelFunc
	blocked            chan struct{}
	applied            []model.TxnID
}

func (b *sequenceFaultBackend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	b.mu.Lock()
	fail, durable, cancel, blocked := b.failApply, b.durable, b.cancel, b.blocked
	b.applied = append(b.applied, batch.TxID)
	b.mu.Unlock()
	if blocked != nil {
		close(blocked)
		<-ctx.Done()
		return storage.ApplyResult{}, ctx.Err()
	}
	if !fail {
		return b.Backend.ApplyMutations(ctx, batch)
	}
	if durable {
		if _, err := b.Backend.ApplyMutations(ctx, batch); err != nil {
			return storage.ApplyResult{}, err
		}
	}
	if cancel != nil {
		cancel()
	}
	return storage.ApplyResult{}, errSequenceFault
}
func (b *sequenceFaultBackend) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	if err := ctx.Err(); err != nil {
		return storage.CommitResolution{}, err
	}
	b.mu.Lock()
	err := b.resolveError
	b.mu.Unlock()
	if err != nil {
		return storage.CommitResolution{}, err
	}
	return b.Backend.ResolveCommit(ctx, id)
}

func sequenceServiceFixture(t *testing.T, maximum int) (*indexRestartFixture, *SequenceService, *sequenceFaultBackend) {
	t.Helper()
	f := newIndexRestartFixture(t)
	if err := f.protections.Close(); err != nil {
		t.Fatal(err)
	}
	f.protections = nil
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := f.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	backend := &sequenceFaultBackend{Backend: f.backend}
	f.manager, err = txn.NewManager(t.Context(), txn.ManagerConfig{Backend: backend, Clock: f.clock, Authority: f.authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSequenceStore(t.Context(), SequenceStoreOptions{Target: storage.ObjectRef{ObjectID: 90, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.keys, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 64, MaxCache: 8})
	if err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.Resolver = store
	transaction, err := f.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	err = transaction.Statement(t.Context(), func(ctx context.Context) error { return store.InitializeAllocator(ctx, transaction) })
	if err == nil {
		_, err = transaction.Commit(t.Context())
	}
	if err = errors.Join(err, transaction.Close(context.Background())); err != nil {
		t.Fatal(err)
	}
	service, err := NewSequenceService(t.Context(), SequenceServiceOptions{Manager: f.manager, Store: store, Transactions: f.options, MaxOperations: maximum})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		backend.mu.Lock()
		backend.resolveError = nil
		backend.mu.Unlock()
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return f, service, backend
}

func initializeServiceSequence(t *testing.T, service *SequenceService, cache int64) SequenceState {
	t.Helper()
	state, err := service.Initialize(t.Context(), SequenceConfig{Start: 1, Min: 1, Max: 1000, Increment: 1, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSequenceServiceConcurrentReservationsSurviveReopen(t *testing.T) {
	f, service, _ := sequenceServiceFixture(t, 4)
	state := initializeServiceSequence(t, service, 2)
	type result struct {
		reservation SequenceReservation
		err         error
	}
	results := make(chan result, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			reservation, err := service.Reserve(t.Context(), state.Incarnation)
			results <- result{reservation, err}
		}()
	}
	workers.Wait()
	close(results)
	seen := make(map[int64]bool)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		r := result.reservation
		if r.Count != 2 || r.Last != r.First+1 {
			t.Fatalf("invalid committed range: %+v", r)
		}
		for value := r.First; value <= r.Last; value++ {
			if seen[value] {
				t.Fatalf("value %d allocated twice", value)
			}
			seen[value] = true
		}
	}
	for value := int64(1); value <= 64; value++ {
		if !seen[value] {
			t.Fatalf("missing allocated value %d", value)
		}
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	reopened, err := NewSequenceService(t.Context(), SequenceServiceOptions{Manager: f.manager, Store: service.options.Store, Transactions: f.options, MaxOperations: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	r, err := reopened.Reserve(t.Context(), state.Incarnation)
	if err != nil || r.First != 65 {
		t.Fatalf("reopened reservation = %+v, %v; want first 65", r, err)
	}
}

func TestSequenceServiceRunsInsideParentStatementWithSchemaLock(t *testing.T) {
	f, service, _ := sequenceServiceFixture(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	options := f.options
	options.Resolver = service.options.Store
	parent, err := f.manager.Begin(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := parent.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := parent.LockSchema(ctx, storage.ObjectRef{ObjectID: 91, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}); err != nil {
		t.Fatal(err)
	}
	var id SequenceIncarnation
	err = parent.Statement(ctx, func(ctx context.Context) error {
		state, err := service.Initialize(ctx, SequenceConfig{Start: 7, Min: 1, Max: 100, Increment: 1, Cache: 1})
		if err != nil {
			return err
		}
		id = state.Incarnation
		r, err := service.Reserve(ctx, id)
		if err == nil && r.First != 7 {
			t.Errorf("first = %d, want 7", r.First)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := service.Reserve(ctx, id)
	if err != nil || r.First != 8 {
		t.Fatalf("parent rollback undid allocation: %+v, %v", r, err)
	}
}

func TestSequenceServiceKnownAbortDoesNotConsumeValue(t *testing.T) {
	_, service, backend := sequenceServiceFixture(t, 1)
	state := initializeServiceSequence(t, service, 1)
	backend.failApply = true
	r, err := service.Reserve(t.Context(), state.Incarnation)
	var pending *PendingSequenceError
	if !errors.Is(err, errSequenceFault) || errors.As(err, &pending) || r != (SequenceReservation{}) {
		t.Fatalf("known abort returned %+v, %v", r, err)
	}
	backend.failApply = false
	r, err = service.Reserve(t.Context(), state.Incarnation)
	if err != nil || r.First != 1 {
		t.Fatalf("aborted allocation consumed value: %+v, %v", r, err)
	}
}

func TestSequenceServiceResolvesOriginalDecisionWithoutResubmission(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "aborted", true: "committed"}[durable], func(t *testing.T) {
			_, service, backend := sequenceServiceFixture(t, 1)
			state := initializeServiceSequence(t, service, 1)
			backend.failApply, backend.durable, backend.resolveError = true, durable, errSequenceFault
			r, err := service.Reserve(t.Context(), state.Incarnation)
			var pending *PendingSequenceError
			if !errors.As(err, &pending) || r != (SequenceReservation{}) {
				t.Fatalf("unknown commit exposed %+v, %v", r, err)
			}
			id := pending.Transaction
			before := len(backend.applied)
			if _, err := service.Reserve(t.Context(), state.Incarnation); err == nil {
				t.Fatal("pending ownership exceeded capacity")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := service.Resolve(ctx, id); !errors.As(err, &pending) || pending.Transaction != id || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled resolution lost ownership: %v", err)
			}
			backend.resolveError = nil
			outcome, err := service.Resolve(t.Context(), id)
			if durable {
				if err != nil || outcome.Operation != SequenceReserve || outcome.Reservation.First != 1 {
					t.Fatalf("resolved result: %+v, %v", outcome, err)
				}
			} else if !errors.Is(err, errSequenceOperationAborted) || errors.As(err, &pending) || outcome != (SequenceOutcome{}) {
				t.Fatalf("resolved abort: %+v, %v", outcome, err)
			}
			if len(backend.applied) != before {
				t.Fatal("resolution resubmitted allocation")
			}
			if _, err := service.Resolve(t.Context(), id); !errors.Is(err, errSequenceOperationMissing) {
				t.Fatalf("terminal outcome retained: %v", err)
			}
			backend.failApply = false
			r, err = service.Reserve(t.Context(), state.Incarnation)
			want := int64(1)
			if durable {
				want = 2
			}
			if err != nil || r.First != want {
				t.Fatalf("next = %+v, %v; want %d", r, err, want)
			}
		})
	}
}

func TestSequenceServiceCancellationAfterDurabilityRetainsResult(t *testing.T) {
	_, service, backend := sequenceServiceFixture(t, 2)
	state := initializeServiceSequence(t, service, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.failApply, backend.durable, backend.cancel = true, true, cancel
	changed, err := service.Set(ctx, state.Incarnation, 50, false)
	var pending *PendingSequenceError
	if !errors.As(err, &pending) || changed != (SequenceState{}) {
		t.Fatalf("canceled durable set: %+v, %v", changed, err)
	}
	outcome, err := service.Resolve(t.Context(), pending.Transaction)
	if err != nil || outcome.Operation != SequenceSet || outcome.State.Last != 50 || outcome.State.Called {
		t.Fatalf("set resolution: %+v, %v", outcome, err)
	}
	backend.failApply = false
	r, err := service.Reserve(t.Context(), state.Incarnation)
	if err != nil || r.First != 50 {
		t.Fatalf("durable set not applied: %+v, %v", r, err)
	}
}

func TestSequenceServiceCloseRetainsFailedResolution(t *testing.T) {
	_, service, backend := sequenceServiceFixture(t, 1)
	backend.failApply, backend.durable, backend.resolveError = true, true, errSequenceFault
	state, err := service.Initialize(t.Context(), SequenceConfig{Start: 1, Min: 1, Max: 100, Increment: 1, Cache: 1})
	var pending *PendingSequenceError
	if !errors.As(err, &pending) || state != (SequenceState{}) {
		t.Fatalf("uncertain initialize: %+v, %v", state, err)
	}
	id := pending.Transaction
	if err := service.Close(t.Context()); !errors.As(err, &pending) || pending.Transaction != id {
		t.Fatalf("close discarded pending decision: %v", err)
	}
	backend.resolveError = nil
	outcome, err := service.Resolve(t.Context(), id)
	if err != nil || outcome.Operation != SequenceInitialize || outcome.State.Incarnation == 0 {
		t.Fatalf("resolve after failed close: %+v, %v", outcome, err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve(t.Context(), outcome.State.Incarnation); err == nil {
		t.Fatal("closed service admitted allocation")
	}
	transaction, err := service.options.Manager.Begin(t.Context(), service.options.Transactions)
	if err != nil {
		t.Fatalf("service closed borrowed manager: %v", err)
	}
	if err := transaction.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSequenceServiceCloseCancelsAndDrainsSubmission(t *testing.T) {
	_, service, backend := sequenceServiceFixture(t, 2)
	state := initializeServiceSequence(t, service, 1)
	backend.blocked = make(chan struct{})
	result := make(chan error, 1)
	go func() { _, err := service.Reserve(t.Context(), state.Incarnation); result <- err }()
	select {
	case <-backend.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("allocation never reached durable submission")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Reserve(ctx, state.Incarnation); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting admission ignored cancellation: %v", err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active submission was not canceled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not drain allocation")
	}
}
