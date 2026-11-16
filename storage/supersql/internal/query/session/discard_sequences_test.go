package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type undiscardableSequences struct{ model.SequenceRuntime }

// discardSequenceAllocator serves queued leases so a test can observe that a
// discard actually forces the session to reserve again.
type discardSequenceAllocator struct {
	leases  map[schema.SequenceIncarnation][]schema.SequenceReservation
	failure error
}

func (a *discardSequenceAllocator) Reserve(_ context.Context, id schema.SequenceIncarnation) (schema.SequenceReservation, error) {
	if a.failure != nil {
		return schema.SequenceReservation{}, a.failure
	}
	queued := a.leases[id]
	if len(queued) == 0 {
		return schema.SequenceReservation{}, &model.Error{Code: "2200H", Message: "sequence exhausted"}
	}
	a.leases[id] = queued[1:]
	return queued[0], nil
}

func (a *discardSequenceAllocator) Set(_ context.Context, id schema.SequenceIncarnation, value int64, called bool) (schema.SequenceState, error) {
	return schema.SequenceState{Incarnation: id, Last: value, Called: called}, a.failure
}

func (a *discardSequenceAllocator) Resolve(context.Context, model.TxnID) (schema.SequenceOutcome, error) {
	return schema.SequenceOutcome{}, a.failure
}

type sequenceRuntimeCatalog struct{ descriptor *model.Descriptor }

func (c sequenceRuntimeCatalog) ResolveOID(context.Context, model.OID) (catalog.PublishedDescriptor, error) {
	return catalog.PublishedDescriptor{Descriptor: c.descriptor}, nil
}

func (c sequenceRuntimeCatalog) Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error) {
	return catalog.PublishedDescriptor{Descriptor: c.descriptor}, nil
}

func sequenceTestDescriptor(t *testing.T, oid model.OID) *model.Descriptor {
	t.Helper()
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 8, OID: oid, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectSequence, Name: "numbers"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Sequence: &model.SequenceMetadata{
			Identity: 1, Incarnation: 1, Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64},
			Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 3,
		},
	}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 64, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func newSequenceRuntimeForDiscard(t *testing.T, allocations SequenceAllocations) model.SequenceRuntime {
	t.Helper()
	session := newSequenceSessionTest(t, allocations, 2)
	runtime, err := NewSequenceRuntime(sequenceRuntimeCatalog{descriptor: sequenceTestDescriptor(t, 1001)}, session)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func discardSequences(t *testing.T, runtime model.SequenceRuntime) error {
	t.Helper()
	transactions := &Transactions{}
	environment := PreparedExecution{Environment: preparedEnvironmentWithSequences(runtime)}
	_, err := transactions.discard(t.Context(), bind.DiscardCommand{Target: zpgparse.DiscardMode_DISCARD_SEQUENCES}, environment)
	return err
}

// TestDiscardSequencesResetsRealSessionState pins the required DISCARD
// SEQUENCES semantics on the observable session surface: currval, lastval, and
// the cached lease are dropped, so the next nextval must reserve from the
// allocator again instead of serving a stale cached value.
func TestDiscardSequencesResetsRealSessionState(t *testing.T) {
	allocations := &discardSequenceAllocator{leases: map[schema.SequenceIncarnation][]schema.SequenceReservation{
		1: {
			{Incarnation: 1, First: 10, Last: 12, Increment: 1, Count: 3},
			{Incarnation: 1, First: 90, Last: 92, Increment: 1, Count: 3},
		},
	}}
	runtime := newSequenceRuntimeForDiscard(t, allocations)

	value, err := runtime.NextSequence(t.Context(), 1001)
	requireSequenceValue(t, value, err, 10)
	value, err = runtime.CurrentSequence(t.Context(), 1001)
	requireSequenceValue(t, value, err, 10)
	value, err = runtime.LastSequence(t.Context())
	requireSequenceValue(t, value, err, 10)

	if err := discardSequences(t, runtime); err != nil {
		t.Fatalf("discard with session runtime: %v", err)
	}

	_, err = runtime.CurrentSequence(t.Context(), 1001)
	requireCode(t, err, "55000")
	_, err = runtime.LastSequence(t.Context())
	requireCode(t, err, "55000")
	// The cached lease is gone, so this must consult the allocator again.
	value, err = runtime.NextSequence(t.Context(), 1001)
	requireSequenceValue(t, value, err, 90)
}

// TestDiscardSequencesRequiresRealSessionCapability pins that an absent or
// non-discarding provider reports the missing capability instead of silently
// succeeding, while a session that cannot discard pending work surfaces that
// error rather than dropping allocator ownership.
func TestDiscardSequencesRequiresRealSessionCapability(t *testing.T) {
	for name, provider := range map[string]model.SequenceRuntime{
		"absent":          nil,
		"lacking discard": undiscardableSequences{},
	} {
		t.Run(name, func(t *testing.T) {
			err := discardSequences(t, provider)
			var diagnostic *model.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != "0A000" {
				t.Fatalf("error = %v, want SQLSTATE 0A000", err)
			}
		})
	}

	t.Run("pending work is retained", func(t *testing.T) {
		allocations := &discardSequenceAllocator{failure: &schema.PendingSequenceError{
			Transaction: model.TxnID{7},
			Cause:       &model.Error{Code: "2200H", Message: "sequence outcome is unknown"},
		}}
		runtime := newSequenceRuntimeForDiscard(t, allocations)
		if _, err := runtime.NextSequence(t.Context(), 1001); err == nil {
			t.Fatal("expected the reserved sequence operation to stay pending")
		}
		var pending *schema.PendingSequenceError
		if err := discardSequences(t, runtime); !errors.As(err, &pending) {
			t.Fatalf("discard error = %v, want a pending sequence error", err)
		}
	})
}

func preparedEnvironmentWithSequences(provider model.SequenceRuntime) exec.Environment {
	return exec.Environment{Sequences: provider}
}
