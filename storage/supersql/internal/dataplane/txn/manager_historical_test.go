package txn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestClockLossPreservesHistoricalReads(t *testing.T) {
	fixture := openManagerStore(t)
	manager, err := txn.NewManager(t.Context(), fixture.managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	// The database has been idle since commit 20. Closing 150 must not create a write.
	fixture.clock.mu.Lock()
	fixture.clock.reading.Earliest, fixture.clock.reading.Latest = 140, 160
	fixture.clock.mu.Unlock()
	pinned, err := manager.HistoricalSnapshot(t.Context(), 150)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pinned.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.requireBalance(t, pinned)
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.CommitTS != 20 || state.ClosedTS != 150 {
		t.Fatalf("idle barrier committed data or changed the requested instant: commit=%d closed=%d", state.CommitTS, state.ClosedTS)
	}
	fixture.clock.mu.Lock()
	fixture.clock.reading.Status = 0
	fixture.clock.mu.Unlock()
	fixture.requireBalance(t, pinned)
	exact, err := manager.HistoricalSnapshot(t.Context(), 150)
	if err != nil {
		t.Fatal(err)
	}
	fixture.requireBalance(t, exact)
	if err := exact.Close(); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(context.Context) (storage.SnapshotReader, error){
		manager.StrongSnapshot,
		func(ctx context.Context) (storage.SnapshotReader, error) { return manager.HistoricalSnapshot(ctx, 151) },
	} {
		reader, err := open(t.Context())
		if reader != nil {
			if closeErr := reader.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if !errors.Is(err, clock.ErrClockUnavailable) {
			t.Fatalf("clock loss allowed a new barrier: %v", err)
		}
	}
	after, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.ClosedTS != state.ClosedTS || after.HistoryFloor != state.HistoryFloor || after.LogPosition != state.LogPosition {
		t.Fatalf("clock failure advanced durable metadata: before=%+v after=%+v", state, after)
	}
	if err := pinned.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
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
	config := fixture.managerConfig(t)
	config.Clock, config.HistoricalReadOnly = nil, true
	reopened, err := txn.NewManager(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	exact, err = reopened.HistoricalSnapshot(t.Context(), 150)
	if err != nil {
		t.Fatal(err)
	}
	fixture.requireBalance(t, exact)
	if err := exact.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalFutureRequiresFreshCertifiedBounds(t *testing.T) {
	fixture := openManagerStore(t)
	manager, err := txn.NewManager(t.Context(), fixture.managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.clock.mu.Lock()
	fixture.clock.reading.Earliest, fixture.clock.reading.Latest = 120, 130
	fixture.clock.mu.Unlock()
	reader, err := manager.HistoricalSnapshot(t.Context(), 131)
	if reader != nil {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, model.ErrTimestampRange) {
		t.Fatalf("future timestamp: %v", err)
	}
	fixture.clock.mu.Lock()
	fixture.clock.reading.Status = 0
	fixture.clock.mu.Unlock()
	reader, err = manager.HistoricalSnapshot(t.Context(), 131)
	if reader != nil {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, clock.ErrClockUnavailable) {
		t.Fatalf("unassured future verdict: %v", err)
	}
}

type incompatibleScaleClock struct{ clock.Clock }

func (provider incompatibleScaleClock) Scale() clock.ScaleDescriptor {
	scale := provider.Clock.Scale()
	scale.Name = "unix_ns"
	return scale
}

func TestManagerRejectsIncompatibleClockBeforeReadiness(t *testing.T) {
	fixture := openManagerStore(t)
	config := fixture.managerConfig(t)
	config.Clock = incompatibleScaleClock{Clock: config.Clock}
	manager, err := txn.NewManager(t.Context(), config)
	if manager != nil {
		if closeErr := manager.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, clock.ErrClockScale) {
		t.Fatalf("incompatible provider: %v", err)
	}
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.ClosedTS != config.Recovered.ClosedTS {
		t.Fatalf("rejected provider advanced fence to %d", state.ClosedTS)
	}
}
