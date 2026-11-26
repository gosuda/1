package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

type constraintModeClock struct {
	clock.Clock
	mu      sync.Mutex
	scale   clock.ScaleDescriptor
	reading clock.Reading
}

func (provider *constraintModeClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.reading, nil
}

func (provider *constraintModeClock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if epoch != provider.reading.Epoch {
		return clock.ErrClockEpochChanged
	}
	if target < 0 || target == clock.Instant(math.MaxInt64) {
		return clock.ErrClockRange
	}
	provider.reading.Earliest = max(provider.reading.Earliest, target+1)
	provider.reading.Latest = max(provider.reading.Latest, provider.reading.Earliest)
	return nil
}

func (provider *constraintModeClock) Scale() clock.ScaleDescriptor {
	scale := provider.scale
	scale.CanonicalLeapHistory = append([]byte(nil), scale.CanonicalLeapHistory...)
	return scale
}

type unusedConstraintModeEncoder struct {
	storage.KeyEncoder
}

var errDeliberateConstraintViolation = &model.Error{
	Code:       "23505",
	Message:    "deliberate deferred constraint violation",
	Constraint: "unrelated",
}

type violatingDeferredConstraint struct{}

func (violatingDeferredConstraint) Identity() txn.ConstraintIdentity {
	return txn.ConstraintIdentity{Object: 9, Constraint: 99, Schema: "public", Name: "unrelated", Deferrable: true, InitiallyDeferred: true}
}

func (violatingDeferredConstraint) RetainedBytes() uint64 { return 1 }

func (violatingDeferredConstraint) Validate(context.Context, txn.ConstraintTransaction) error {
	return errDeliberateConstraintViolation
}

func activeConstraintModeTransaction(t *testing.T) *txn.Transaction {
	t.Helper()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(history),
		CanonicalLeapHistory: history,
	}
	epoch := clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}
	provider := constraintModeClock{
		scale:   scale,
		reading: clock.Reading{Earliest: 100, Latest: 110, Epoch: epoch, Status: clock.Synced},
	}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := neosepia.Open(t.Context(), neosepia.Config{
		Directory:     t.TempDir(),
		Scale:         scale,
		KeyEncoder:    unusedConstraintModeEncoder{},
		FenceVerifier: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	state, err := backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := txn.NewManager(t.Context(), txn.ManagerConfig{
		Backend:       backend,
		Clock:         &provider,
		Authority:     authority,
		MaxOperations: 4,
		Recovered: txn.RecoveredState{
			LogPosition:  state.LogPosition,
			CommitTS:     state.CommitTS,
			ClosedTS:     state.ClosedTS,
			HistoryFloor: state.HistoryFloor,
			Epoch:        state.Epoch,
			LeapHistory:  state.Scale.LeapHistoryID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	transaction, err := manager.Begin(t.Context(), txn.TransactionOptions{
		Mode:        txn.ReadOnly,
		MemoryBytes: 1 << 20,
		MaxViews:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transaction.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return transaction
}

func TestNamedNondeferrableImmediateDoesNotFlushUnrelatedDeferredViolation(t *testing.T) {
	pending := violatingDeferredConstraint{}
	transactions := &Transactions{
		active:                  true,
		transaction:             activeConstraintModeTransaction(t),
		deferredConstraints:     []deferredRelationalConstraint{{event: pending, bytes: 1}},
		deferredConstraintBytes: 1,
	}
	named := bind.ConstraintModeCommand{
		Names: []bind.ResolvedConstraintMode{{Object: 1, Constraint: 2, Schema: "public", Name: "fixed_key"}},
	}
	if _, err := transactions.executeConstraintMode(t.Context(), named); err != nil {
		t.Fatal(err)
	}
	if _, err := transactions.executeConstraintMode(t.Context(), bind.ConstraintModeCommand{All: true}); !errors.Is(err, errDeliberateConstraintViolation) {
		t.Fatalf("ALL IMMEDIATE error = %v; want deliberate deferred violation", err)
	}
}
