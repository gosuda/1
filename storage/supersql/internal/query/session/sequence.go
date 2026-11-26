package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// SequenceAllocations is borrowed; closing a session never closes its allocator.
// Successful results must be committed. Unresolved failures carry PendingSequenceError.
type SequenceAllocations interface {
	Reserve(context.Context, schema.SequenceIncarnation) (schema.SequenceReservation, error)
	Set(context.Context, schema.SequenceIncarnation, int64, bool) (schema.SequenceState, error)
	Resolve(context.Context, model.TxnID) (schema.SequenceOutcome, error)
}

type SequenceSessionOptions struct {
	Allocations  SequenceAllocations
	MaxSequences int
}

type SequenceReference struct {
	Object      model.ObjectID
	Identity    schema.SequenceIncarnation
	Incarnation schema.SequenceIncarnation
}

type sequenceSessionEntry struct {
	reference SequenceReference
	current   int64
	called    bool
	lease     schema.SequenceReservation
}

type pendingSessionSequence struct {
	transaction model.TxnID
	reference   SequenceReference
	operation   schema.SequenceOperation
	value       int64
	called      bool
	cause       error
}

// SequenceSession owns rollback-independent currval and cache leases. The caller
// must pin and recheck catalog existence, logical identity, and privileges on
// every invocation (including cache hits and lastval), and enforce read-only mode
// before Next or Set. This facade never consults a catalog or user transaction.
type SequenceSession struct {
	allocations  SequenceAllocations
	maxSequences int
	gate         chan struct{}
	entries      map[schema.SequenceIncarnation]*sequenceSessionEntry
	last         schema.SequenceIncarnation
	pending      *pendingSessionSequence
	closing      bool
}

func NewSequenceSession(options SequenceSessionOptions) (*SequenceSession, error) {
	if options.Allocations == nil || options.MaxSequences <= 0 {
		return nil, sequenceSessionError("22023", "sequence session requires an allocator and positive sequence limit")
	}
	return &SequenceSession{
		allocations:  options.Allocations,
		maxSequences: options.MaxSequences,
		gate:         make(chan struct{}, 1),
		entries:      make(map[schema.SequenceIncarnation]*sequenceSessionEntry),
	}, nil
}

func sequenceSessionError(code, message string) error {
	return &model.Error{Code: code, Message: message}
}

func (s *SequenceSession) acquire(ctx context.Context) error {
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

func (s *SequenceSession) release() { <-s.gate }

func (s *SequenceSession) entry(ref SequenceReference) (*sequenceSessionEntry, error) {
	if s.closing {
		return nil, sequenceSessionError("55000", "sequence session is closed")
	}
	if ref.Object == 0 || ref.Identity == 0 || ref.Incarnation == 0 {
		return nil, sequenceSessionError("22023", "invalid sequence reference")
	}
	entry := s.entries[ref.Identity]
	if entry != nil && entry.reference.Object != ref.Object {
		return nil, sequenceSessionError("55000", "sequence logical identity does not match object")
	}
	return entry, nil
}

func (s *SequenceSession) mutationEntry(ref SequenceReference) (*sequenceSessionEntry, error) {
	entry, err := s.entry(ref)
	if err != nil {
		return nil, err
	}
	if s.pending != nil {
		return nil, &schema.PendingSequenceError{Transaction: s.pending.transaction, Cause: s.pending.cause}
	}
	if entry == nil {
		if len(s.entries) >= s.maxSequences {
			return nil, sequenceSessionError("54000", "sequence session limit exceeded")
		}
		entry = &sequenceSessionEntry{reference: ref}
	}
	return entry, nil
}

func (s *SequenceSession) retain(err error, request pendingSessionSequence, entry *sequenceSessionEntry) {
	var pending *schema.PendingSequenceError
	if errors.As(err, &pending) {
		request.transaction, request.cause = pending.Transaction, pending.Cause
		s.pending = &request
		s.entries[request.reference.Identity] = entry
	}
}

func (s *SequenceSession) consume(entry *sequenceSessionEntry, ref SequenceReference) int64 {
	value := entry.lease.First
	entry.lease.Count--
	if entry.lease.Count != 0 {
		// Modular unsigned addition covers negative increments, including MinInt64.
		entry.lease.First = int64(uint64(value) + uint64(entry.lease.Increment))
	}
	entry.reference, entry.current, entry.called = ref, value, true
	s.entries[ref.Identity], s.last = entry, ref.Identity
	return value
}

func (s *SequenceSession) Next(ctx context.Context, ref SequenceReference) (int64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	entry, err := s.mutationEntry(ref)
	if err != nil {
		return 0, err
	}
	if entry.lease.Incarnation == ref.Incarnation && entry.lease.Count != 0 {
		return s.consume(entry, ref), nil
	}
	reservation, err := s.allocations.Reserve(ctx, ref.Incarnation)
	if err != nil {
		s.retain(err, pendingSessionSequence{reference: ref, operation: schema.SequenceReserve}, entry)
		return 0, err
	}
	entry.lease = reservation
	return s.consume(entry, ref), nil
}

// InvalidateLease applies ALTER SEQUENCE's session-local cache discard without
// changing currval/lastval. Like sequence observations, it is not rolled back.
func (s *SequenceSession) InvalidateLease(ctx context.Context, ref SequenceReference) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	entry, err := s.entry(ref)
	if err != nil {
		return err
	}
	if s.pending != nil {
		return &schema.PendingSequenceError{Transaction: s.pending.transaction, Cause: s.pending.cause}
	}
	if entry != nil {
		entry.lease = schema.SequenceReservation{}
	}
	return nil
}

func (s *SequenceSession) Current(ctx context.Context, ref SequenceReference) (int64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	entry, err := s.entry(ref)
	if err != nil {
		return 0, err
	}
	if entry == nil || !entry.called {
		return 0, sequenceSessionError("55000", "currval is not yet defined in this session")
	}
	return entry.current, nil
}

func (s *SequenceSession) lastEntry() (*sequenceSessionEntry, error) {
	if s.closing {
		return nil, sequenceSessionError("55000", "sequence session is closed")
	}
	if s.last == 0 {
		return nil, sequenceSessionError("55000", "lastval is not yet defined in this session")
	}
	return s.entries[s.last], nil
}

func (s *SequenceSession) Last(ctx context.Context) (int64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	entry, err := s.lastEntry()
	if err != nil {
		return 0, err
	}
	return entry.current, nil
}

func (s *SequenceSession) LastReference(ctx context.Context) (SequenceReference, error) {
	if err := s.acquire(ctx); err != nil {
		return SequenceReference{}, err
	}
	defer s.release()
	entry, err := s.lastEntry()
	if err != nil {
		return SequenceReference{}, err
	}
	return entry.reference, nil
}

func (s *SequenceSession) applySet(entry *sequenceSessionEntry, ref SequenceReference, value int64, called bool) {
	entry.reference, entry.lease = ref, schema.SequenceReservation{}
	if called {
		entry.current, entry.called = value, true
	}
	s.entries[ref.Identity] = entry
}

func (s *SequenceSession) Set(ctx context.Context, ref SequenceReference, value int64, called bool) (int64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	entry, err := s.mutationEntry(ref)
	if err != nil {
		return 0, err
	}
	_, err = s.allocations.Set(ctx, ref.Incarnation, value, called)
	if err != nil {
		s.retain(err, pendingSessionSequence{reference: ref, operation: schema.SequenceSet, value: value, called: called}, entry)
		return 0, err
	}
	s.applySet(entry, ref, value, called)
	return value, nil
}

// Discard clears session-local sequence values and leases. A durable operation
// with an unknown outcome must be resolved before its ownership can be dropped.
func (s *SequenceSession) Discard(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.closing {
		return sequenceSessionError("55000", "sequence session is closed")
	}
	if s.pending != nil {
		return &schema.PendingSequenceError{Transaction: s.pending.transaction, Cause: s.pending.cause}
	}
	clear(s.entries)
	s.last = 0
	return nil
}

// Resolve consumes one original durable decision; it never resubmits a mutation.
func (s *SequenceSession) Resolve(ctx context.Context, transaction model.TxnID) (int64, error) {
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	return s.resolve(ctx, transaction)
}

func (s *SequenceSession) resolve(ctx context.Context, transaction model.TxnID) (int64, error) {
	request := s.pending
	if request == nil || request.transaction != transaction {
		return 0, sequenceSessionError("55000", "sequence operation is not pending in this session")
	}
	outcome, err := s.allocations.Resolve(ctx, transaction)
	if err != nil {
		var pending *schema.PendingSequenceError
		if errors.As(err, &pending) {
			request.cause = pending.Cause
			return 0, err
		}
		s.pending = nil
		if entry := s.entries[request.reference.Identity]; entry != nil && !entry.called && entry.lease.Count == 0 {
			delete(s.entries, request.reference.Identity)
		}
		return 0, err
	}
	s.pending = nil
	if s.closing {
		if request.operation == schema.SequenceReserve {
			return outcome.Reservation.First, nil
		}
		return request.value, nil
	}
	entry := s.entries[request.reference.Identity]
	if request.operation == schema.SequenceReserve {
		entry.lease = outcome.Reservation
		return s.consume(entry, request.reference), nil
	}
	s.applySet(entry, request.reference, request.value, request.called)
	return request.value, nil
}

// Close discards local values immediately after draining admitted work. A failed
// pending resolution retains allocator ownership and must be retried by the owner.
func (s *SequenceSession) Close(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	s.closing = true
	s.entries, s.last = nil, 0
	if s.pending != nil {
		if _, err := s.resolve(ctx, s.pending.transaction); err != nil {
			if s.pending != nil {
				return fmt.Errorf("sequence session close retains unresolved operation: %w", err)
			}
			s.allocations = nil
			return err
		}
	}
	s.allocations = nil
	return nil
}
