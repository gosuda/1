package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type SequenceTransactions interface {
	Begin(context.Context, txn.TransactionOptions) (*txn.Transaction, error)
}

type SequenceServiceOptions struct {
	Manager       SequenceTransactions
	Store         *SequenceStore
	Transactions  txn.TransactionOptions
	MaxOperations int
}

type SequenceOperation uint8

const (
	SequenceInitialize SequenceOperation = iota
	SequenceReserve
	SequenceSet
	SequenceReconfigure
	SequenceDiscard
)

type SequenceOutcome struct {
	Operation   SequenceOperation
	State       SequenceState
	Reservation SequenceReservation
}

// PendingSequenceError transfers no candidate value. Resolve Transaction rather
// than resubmitting the operation, even when Cause reports cancellation.
type PendingSequenceError struct {
	Transaction model.TxnID
	Cause       error
}

func (e *PendingSequenceError) Error() string {
	return fmt.Sprintf("sequence transaction %v requires resolution: %v", e.Transaction, e.Cause)
}
func (e *PendingSequenceError) Unwrap() error { return e.Cause }

var (
	errSequenceServiceOptions   = &model.Error{Code: "22023", Message: "invalid sequence service options"}
	errSequenceServiceClosed    = &model.Error{Code: "55000", Message: "sequence service is closed"}
	errSequenceOperationMissing = &model.Error{Code: "55000", Message: "sequence transaction is not pending"}
	errSequenceOperationAborted = &model.Error{Code: "40001", Message: "sequence transaction did not commit"}
)

type pendingSequence struct {
	transaction *txn.Transaction
	outcome     SequenceOutcome
}

// SequenceService borrows its manager and store. Its cancellable gate serializes
// allocation, avoiding update-lock wounds among its own child transactions.
// Pending decisions consume capacity until Resolve or a successful Close.
type SequenceService struct {
	options  SequenceServiceOptions
	lifetime context.Context
	cancel   context.CancelFunc
	gate     chan struct{}
	mu       sync.Mutex
	pending  map[model.TxnID]pendingSequence
}

func NewSequenceService(ctx context.Context, options SequenceServiceOptions) (*SequenceService, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mutationPolicyNil(options.Manager) || options.Store == nil || options.MaxOperations <= 0 || options.Transactions.Mode != txn.ReadWrite || options.Transactions.Timestamp != 0 {
		return nil, errSequenceServiceOptions
	}
	// Child normalization cannot call parent catalog/default resolvers.
	options.Transactions.Resolver = options.Store
	options.Transactions.KeyEncoder = options.Store.options.Encoder
	lifetime, cancel := context.WithCancel(context.Background())
	return &SequenceService{options: options, lifetime: lifetime, cancel: cancel, gate: make(chan struct{}, 1), pending: make(map[model.TxnID]pendingSequence)}, nil
}

func (s *SequenceService) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *SequenceService) leave() { <-s.gate }

// Initialize returns committed state even when terminal cleanup reports an error;
// the caller then owns that incarnation. Uncertain outcomes return no state.
func (s *SequenceService) Initialize(ctx context.Context, config SequenceConfig) (SequenceState, error) {
	outcome, err := s.run(ctx, SequenceInitialize, 0, func(ctx context.Context, transaction *txn.Transaction) (SequenceOutcome, error) {
		state, err := s.options.Store.Initialize(ctx, transaction, config)
		return SequenceOutcome{Operation: SequenceInitialize, State: state}, err
	})
	return outcome.State, err
}

func (s *SequenceService) Reserve(ctx context.Context, id SequenceIncarnation) (SequenceReservation, error) {
	outcome, err := s.run(ctx, SequenceReserve, id, func(ctx context.Context, transaction *txn.Transaction) (SequenceOutcome, error) {
		reservation, err := s.options.Store.Reserve(ctx, transaction, id)
		return SequenceOutcome{Operation: SequenceReserve, Reservation: reservation}, err
	})
	return outcome.Reservation, err
}

func (s *SequenceService) Set(ctx context.Context, id SequenceIncarnation, value int64, called bool) (SequenceState, error) {
	outcome, err := s.run(ctx, SequenceSet, id, func(ctx context.Context, transaction *txn.Transaction) (SequenceOutcome, error) {
		state, err := s.options.Store.Set(ctx, transaction, id, value, called)
		return SequenceOutcome{Operation: SequenceSet, State: state}, err
	})
	return outcome.State, err
}

// Reconfigure creates a new allocation incarnation while the caller holds the
// logical sequence's DDL barrier. It never mutates the currently published state.
// A nil restart preserves the reserved high-water value and called flag.
func (s *SequenceService) Reconfigure(ctx context.Context, id SequenceIncarnation, config SequenceConfig, restart *int64) (SequenceState, error) {
	outcome, err := s.run(ctx, SequenceReconfigure, id, func(ctx context.Context, transaction *txn.Transaction) (SequenceOutcome, error) {
		previous, err := s.options.Store.Get(ctx, transaction, id)
		if err != nil {
			return SequenceOutcome{}, err
		}
		if previous.Retired {
			return SequenceOutcome{}, sequenceUnavailableError()
		}
		last, called := previous.Last, previous.Called
		if restart != nil {
			last, called = *restart, false
		}
		if last < config.Min || last > config.Max {
			return SequenceOutcome{}, &model.Error{Code: "22023", Message: "sequence current or restart value is outside altered bounds"}
		}
		created, err := s.options.Store.Initialize(ctx, transaction, config)
		if err != nil {
			return SequenceOutcome{}, err
		}
		if last != config.Start || called {
			created, err = s.options.Store.Set(ctx, transaction, created.Incarnation, last, called)
		}
		return SequenceOutcome{Operation: SequenceReconfigure, State: created}, err
	})
	return outcome.State, err
}

// Discard is idempotent only for caller-proven unreachable incarnations. It
// atomically retires and reclaims state, preserving the allocator high-water mark.
func (s *SequenceService) Discard(ctx context.Context, id SequenceIncarnation) error {
	_, err := s.run(ctx, SequenceDiscard, id, func(ctx context.Context, transaction *txn.Transaction) (SequenceOutcome, error) {
		outcome := SequenceOutcome{Operation: SequenceDiscard, State: SequenceState{Incarnation: id, Retired: true}}
		_, err := s.options.Store.Get(ctx, transaction, id)
		if errors.Is(err, storage.ErrRowNotFound) {
			return outcome, nil
		}
		if err != nil {
			return SequenceOutcome{}, err
		}
		if err := s.options.Store.Retire(ctx, transaction, id); err != nil {
			return SequenceOutcome{}, err
		}
		return outcome, s.options.Store.Reclaim(ctx, transaction, id)
	})
	return err
}

func (s *SequenceService) run(ctx context.Context, operation SequenceOperation, id SequenceIncarnation, work func(context.Context, *txn.Transaction) (SequenceOutcome, error)) (SequenceOutcome, error) {
	if err := ctx.Err(); err != nil {
		return SequenceOutcome{}, err
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return SequenceOutcome{}, ctx.Err()
	case <-s.lifetime.Done():
		return SequenceOutcome{}, errSequenceServiceClosed
	}
	defer s.leave()
	if s.lifetime.Err() != nil {
		return SequenceOutcome{}, errSequenceServiceClosed
	}
	s.mu.Lock()
	full := len(s.pending) >= s.options.MaxOperations
	s.mu.Unlock()
	if full {
		return SequenceOutcome{}, sequenceLimitError()
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	transaction, err := s.options.Manager.Begin(ctx, s.options.Transactions)
	if err != nil {
		return SequenceOutcome{}, err
	}
	var outcome SequenceOutcome
	err = transaction.Statement(ctx, func(ctx context.Context) error {
		kind, lockID := byte(1), uint64(id)
		allocator := operation == SequenceInitialize || operation == SequenceReconfigure || operation == SequenceDiscard
		if allocator {
			kind, lockID = 0, 0
		}
		if err := s.options.Store.lockRecord(ctx, transaction, kind, lockID); err != nil {
			return err
		}
		if allocator && operation != SequenceInitialize {
			if err := s.options.Store.lockRecord(ctx, transaction, 1, uint64(id)); err != nil {
				return err
			}
		}
		var err error
		outcome, err = work(ctx, transaction)
		return err
	})
	if err != nil {
		return SequenceOutcome{}, errors.Join(err, transaction.Close(ctx))
	}
	_, err = transaction.Commit(ctx)
	if transaction.State() == txn.Indeterminate {
		s.mu.Lock()
		s.pending[transaction.ID()] = pendingSequence{transaction: transaction, outcome: outcome}
		s.mu.Unlock()
		return SequenceOutcome{}, &PendingSequenceError{Transaction: transaction.ID(), Cause: err}
	}
	cleanupErr := transaction.Close(ctx)
	if transaction.State() == txn.Committed {
		return outcome, errors.Join(err, cleanupErr)
	}
	if err != nil || cleanupErr != nil {
		return SequenceOutcome{}, errors.Join(err, cleanupErr)
	}
	return outcome, nil
}

func (s *SequenceStore) lockRecord(ctx context.Context, transaction *txn.Transaction, kind byte, id uint64) error {
	key, err := s.options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(jobLogicalKey(kind, id))}, s.options.KeyBytes)
	if err != nil {
		return err
	}
	if uint64(cap(key)) > s.options.KeyBytes {
		return sequenceLimitError()
	}
	_, err = transaction.LockRow(ctx, s.options.Target, key, txn.UpdateLock, txn.WaitForRowLock)
	return err
}

// Resolve consumes a terminal outcome. Repeated resolution does not replay values.
// Failed resolution retains ownership and always returns PendingSequenceError.
func (s *SequenceService) Resolve(ctx context.Context, id model.TxnID) (SequenceOutcome, error) {
	if err := s.enter(ctx); err != nil {
		s.mu.Lock()
		_, pending := s.pending[id]
		s.mu.Unlock()
		if pending {
			return SequenceOutcome{}, &PendingSequenceError{Transaction: id, Cause: err}
		}
		return SequenceOutcome{}, errSequenceOperationMissing
	}
	defer s.leave()
	return s.resolve(ctx, id)
}

func (s *SequenceService) resolve(ctx context.Context, id model.TxnID) (SequenceOutcome, error) {
	s.mu.Lock()
	pending, ok := s.pending[id]
	s.mu.Unlock()
	if !ok {
		return SequenceOutcome{}, errSequenceOperationMissing
	}
	_, err := pending.transaction.Resolve(ctx)
	state := pending.transaction.State()
	if state != txn.Committed && state != txn.Aborted {
		return SequenceOutcome{}, &PendingSequenceError{Transaction: id, Cause: err}
	}
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
	if state == txn.Aborted {
		if err == nil {
			return SequenceOutcome{}, errSequenceOperationAborted
		}
		return SequenceOutcome{}, errors.Join(errSequenceOperationAborted, err)
	}
	return pending.outcome, err
}

// Close stops admission and cancels active work before resolving retained
// decisions. A failed Close may be retried; neither collaborator is closed.
func (s *SequenceService) Close(ctx context.Context) error {
	s.cancel()
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.leave()
	s.mu.Lock()
	ids := make([]model.TxnID, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var result error
	for _, id := range ids {
		_, err := s.resolve(ctx, id)
		if err == errSequenceOperationAborted {
			continue
		}
		result = errors.Join(result, err)
	}
	return result
}
