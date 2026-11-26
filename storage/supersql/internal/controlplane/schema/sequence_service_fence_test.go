package schema

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestSequenceServiceManagerFenceDoesNotInventPendingOwnership(t *testing.T) {
	_, service, backend := sequenceServiceFixture(t, 2)
	first := initializeServiceSequence(t, service, 1)
	second := initializeServiceSequence(t, service, 1)
	backend.failApply, backend.durable, backend.resolveError = true, true, errSequenceFault
	_, err := service.Reserve(t.Context(), first.Incarnation)
	var pending *PendingSequenceError
	if !errors.As(err, &pending) {
		t.Fatalf("missing original pending decision: %v", err)
	}
	id := pending.Transaction
	before := len(backend.applied)
	r, err := service.Reserve(t.Context(), second.Incarnation)
	if !errors.Is(err, storage.ErrCommitIndeterminate) || errors.As(err, &pending) || r != (SequenceReservation{}) {
		t.Fatalf("unrelated fenced request claimed ownership: %+v, %v", r, err)
	}
	if len(backend.applied) != before {
		t.Fatal("manager fence admitted another durable submission")
	}
	backend.resolveError = nil
	if _, err := service.Resolve(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	backend.failApply = false
	r, err = service.Reserve(t.Context(), second.Incarnation)
	if err != nil || r.First != 1 {
		t.Fatalf("fenced request consumed a value: %+v, %v", r, err)
	}
}
