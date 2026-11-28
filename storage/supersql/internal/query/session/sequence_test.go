package session

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sessionSequenceAllocator struct {
	reservations    map[schema.SequenceIncarnation]schema.SequenceReservation
	reserveCalls    int
	setCalls        int
	failure         error
	resolution      schema.SequenceOutcome
	resolutionError error
	reserveHook     func(context.Context) error
}

func (a *sessionSequenceAllocator) Reserve(ctx context.Context, id schema.SequenceIncarnation) (schema.SequenceReservation, error) {
	a.reserveCalls++
	if a.reserveHook != nil {
		return schema.SequenceReservation{}, a.reserveHook(ctx)
	}
	if a.failure != nil {
		return schema.SequenceReservation{}, a.failure
	}
	reservation := a.reservations[id]
	if reservation.Count == 0 {
		return schema.SequenceReservation{}, &model.Error{Code: "2200H", Message: "sequence exhausted"}
	}
	delete(a.reservations, id)
	return reservation, nil
}

func (a *sessionSequenceAllocator) Set(_ context.Context, id schema.SequenceIncarnation, value int64, called bool) (schema.SequenceState, error) {
	a.setCalls++
	return schema.SequenceState{Incarnation: id, Last: value, Called: called}, a.failure
}

func (a *sessionSequenceAllocator) Resolve(context.Context, model.TxnID) (schema.SequenceOutcome, error) {
	return a.resolution, a.resolutionError
}

func newSequenceSessionTest(t *testing.T, allocations SequenceAllocations, limit int) *SequenceSession {
	t.Helper()
	s, err := NewSequenceSession(SequenceSessionOptions{Allocations: allocations, MaxSequences: limit})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireSequenceValue(t *testing.T, value int64, err error, want int64) {
	t.Helper()
	if err != nil || value != want {
		t.Fatalf("sequence value = %d, %v; want %d", value, err, want)
	}
}

func TestSequenceSetDoesNotEstablishOrReorderLast(t *testing.T) {
	a := &sessionSequenceAllocator{reservations: map[schema.SequenceIncarnation]schema.SequenceReservation{
		1: {Incarnation: 1, First: 10, Last: 12, Increment: 1, Count: 3},
	}}
	s := newSequenceSessionTest(t, a, 2)
	first := SequenceReference{Object: 10, Identity: 1, Incarnation: 1}
	second := SequenceReference{Object: 20, Identity: 2, Incarnation: 2}
	_, err := s.Current(t.Context(), first)
	requireCode(t, err, "55000")
	_, err = s.Last(t.Context())
	requireCode(t, err, "55000")
	value, err := s.Set(t.Context(), second, 30, true)
	requireSequenceValue(t, value, err, 30)
	_, err = s.Last(t.Context())
	requireCode(t, err, "55000")
	value, err = s.Next(t.Context(), first)
	requireSequenceValue(t, value, err, 10)
	value, err = s.Set(t.Context(), second, 40, true)
	requireSequenceValue(t, value, err, 40)
	value, err = s.Last(t.Context())
	requireSequenceValue(t, value, err, 10)
	value, err = s.Set(t.Context(), first, 50, true)
	requireSequenceValue(t, value, err, 50)
	value, err = s.Last(t.Context())
	requireSequenceValue(t, value, err, 50)
	value, err = s.Set(t.Context(), first, 60, false)
	requireSequenceValue(t, value, err, 60)
	value, err = s.Current(t.Context(), first)
	requireSequenceValue(t, value, err, 50)
	_, err = s.Next(t.Context(), first)
	requireCode(t, err, "2200H") // The pre-set lease must not supply 11.
}

func TestSequenceRestartPreservesCurrentButDiscardsLease(t *testing.T) {
	a := &sessionSequenceAllocator{reservations: map[schema.SequenceIncarnation]schema.SequenceReservation{
		1: {Incarnation: 1, First: 1, Last: 3, Increment: 1, Count: 3},
		2: {Incarnation: 2, First: 100, Last: 101, Increment: 1, Count: 2},
	}}
	s := newSequenceSessionTest(t, a, 2)
	ref := SequenceReference{Object: 10, Identity: 1, Incarnation: 1}
	value, err := s.Next(t.Context(), ref)
	requireSequenceValue(t, value, err, 1)
	ref.Incarnation = 2
	value, err = s.Current(t.Context(), ref)
	requireSequenceValue(t, value, err, 1)
	value, err = s.Next(t.Context(), ref)
	requireSequenceValue(t, value, err, 100)
	ref.Identity, ref.Incarnation = 3, 3
	_, err = s.Current(t.Context(), ref)
	requireCode(t, err, "55000")
	last, err := s.LastReference(t.Context())
	if err != nil || last.Identity != 1 || last.Incarnation != 2 || last.Object != 10 {
		t.Fatalf("last reference = %+v, %v", last, err)
	}
}

func TestSequenceCacheCoversSignedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		first, increment, last int64
	}{
		{"minimum_increment", math.MaxInt64, math.MinInt64, -1},
		{"ascending_cross_zero", -1, math.MaxInt64, math.MaxInt64 - 1},
		{"descending_to_minimum", 0, math.MinInt64, math.MinInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &sessionSequenceAllocator{reservations: map[schema.SequenceIncarnation]schema.SequenceReservation{
				1: {Incarnation: 1, First: tc.first, Last: tc.last, Increment: tc.increment, Count: 2},
			}}
			s := newSequenceSessionTest(t, a, 1)
			ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
			value, err := s.Next(t.Context(), ref)
			requireSequenceValue(t, value, err, tc.first)
			value, err = s.Next(t.Context(), ref)
			requireSequenceValue(t, value, err, tc.last)
			_, err = s.Next(t.Context(), ref)
			requireCode(t, err, "2200H")
		})
	}
}

func TestSequenceSessionsOwnIndependentCurrentsAndLimits(t *testing.T) {
	a := &sessionSequenceAllocator{reservations: map[schema.SequenceIncarnation]schema.SequenceReservation{
		1: {Incarnation: 1, First: 1, Last: 2, Increment: 1, Count: 2},
	}}
	first, second := newSequenceSessionTest(t, a, 1), newSequenceSessionTest(t, a, 1)
	ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
	value, err := first.Next(t.Context(), ref)
	requireSequenceValue(t, value, err, 1)
	_, err = second.Current(t.Context(), ref)
	requireCode(t, err, "55000")
	value, err = second.Set(t.Context(), ref, 90, true)
	requireSequenceValue(t, value, err, 90)
	value, err = first.Next(t.Context(), ref)
	requireSequenceValue(t, value, err, 2)
	_, err = first.Set(t.Context(), SequenceReference{Object: 2, Identity: 2, Incarnation: 2}, 80, true)
	requireCode(t, err, "54000")
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	value, err = second.Current(t.Context(), ref)
	requireSequenceValue(t, value, err, 90)
	_, err = first.Last(t.Context())
	requireCode(t, err, "55000")
}

func TestSequenceUncertainReservePublishesOnlyOnResolution(t *testing.T) {
	pending := &schema.PendingSequenceError{Transaction: model.TxnID{7}, Cause: errors.New("commit response lost")}
	a := &sessionSequenceAllocator{failure: pending, resolution: schema.SequenceOutcome{
		Operation:   schema.SequenceReserve,
		Reservation: schema.SequenceReservation{Incarnation: 1, First: 10, Last: 11, Increment: 1, Count: 2},
	}}
	s := newSequenceSessionTest(t, a, 1)
	ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
	_, err := s.Next(t.Context(), ref)
	if !errors.Is(err, pending) {
		t.Fatalf("uncertain reserve = %v", err)
	}
	_, err = s.Current(t.Context(), ref)
	requireCode(t, err, "55000")
	_, err = s.Next(t.Context(), ref)
	var retry *schema.PendingSequenceError
	if !errors.As(err, &retry) || retry.Transaction != (model.TxnID{7}) || a.reserveCalls != 1 {
		t.Fatalf("retry duplicated uncertain mutation: %v, reserve calls %d", err, a.reserveCalls)
	}
	_, err = s.Resolve(t.Context(), model.TxnID{8})
	requireCode(t, err, "55000")
	value, err := s.Resolve(t.Context(), model.TxnID{7})
	requireSequenceValue(t, value, err, 10)
	value, err = s.Last(t.Context())
	requireSequenceValue(t, value, err, 10)
	value, err = s.Next(t.Context(), ref)
	requireSequenceValue(t, value, err, 11)
	_, err = s.Resolve(t.Context(), model.TxnID{7})
	requireCode(t, err, "55000")
}

func TestSequenceUncertainSetPreservesLeaseUntilCommitted(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "aborted", true: "committed"}[committed], func(t *testing.T) {
			a := &sessionSequenceAllocator{reservations: map[schema.SequenceIncarnation]schema.SequenceReservation{
				1: {Incarnation: 1, First: 10, Last: 12, Increment: 1, Count: 3},
			}}
			s := newSequenceSessionTest(t, a, 1)
			ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
			value, err := s.Next(t.Context(), ref)
			requireSequenceValue(t, value, err, 10)
			a.failure = &schema.PendingSequenceError{Transaction: model.TxnID{7}, Cause: errors.New("unknown decision")}
			_, err = s.Set(t.Context(), ref, 99, true)
			var pending *schema.PendingSequenceError
			if !errors.As(err, &pending) {
				t.Fatalf("set = %v", err)
			}
			value, err = s.Last(t.Context())
			requireSequenceValue(t, value, err, 10)
			_, err = s.Set(t.Context(), ref, 99, true)
			if !errors.As(err, &pending) || a.setCalls != 1 {
				t.Fatalf("set retry = %v, durable mutations %d", err, a.setCalls)
			}
			a.failure = nil
			if !committed {
				a.resolutionError = &model.Error{Code: "40001", Message: "transaction aborted"}
				_, err = s.Resolve(t.Context(), model.TxnID{7})
				requireCode(t, err, "40001")
				value, err = s.Next(t.Context(), ref)
				requireSequenceValue(t, value, err, 11)
				return
			}
			a.resolution = schema.SequenceOutcome{Operation: schema.SequenceSet, State: schema.SequenceState{Incarnation: 1, Last: 99, Called: true}}
			value, err = s.Resolve(t.Context(), model.TxnID{7})
			requireSequenceValue(t, value, err, 99)
			value, err = s.Last(t.Context())
			requireSequenceValue(t, value, err, 99)
			_, err = s.Next(t.Context(), ref)
			requireCode(t, err, "2200H")
		})
	}
}

func TestSequenceCloseRetainsUnknownDecisionForRetry(t *testing.T) {
	pending := &schema.PendingSequenceError{Transaction: model.TxnID{7}, Cause: errors.New("decision unavailable")}
	a := &sessionSequenceAllocator{failure: pending, resolutionError: pending}
	s := newSequenceSessionTest(t, a, 1)
	ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
	_, err := s.Next(t.Context(), ref)
	if !errors.Is(err, pending) {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); !errors.Is(err, pending) {
		t.Fatalf("close lost unresolved decision: %v", err)
	}
	_, err = s.Current(t.Context(), ref)
	requireCode(t, err, "55000")
	a.resolutionError = nil
	a.resolution = schema.SequenceOutcome{Operation: schema.SequenceReserve, Reservation: schema.SequenceReservation{Incarnation: 1, First: 10, Count: 1}}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSequenceGateWaitHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	a := &sessionSequenceAllocator{reserveHook: func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	s := newSequenceSessionTest(t, a, 1)
	ref := SequenceReference{Object: 1, Identity: 1, Incarnation: 1}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx, ref)
		finished <- err
	}()
	t.Cleanup(func() { cancel(); <-finished })
	<-entered
	waiting, stopWaiting := context.WithCancel(t.Context())
	stopWaiting()
	_, err := s.Current(waiting, ref)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled gate waiter = %v", err)
	}
}
