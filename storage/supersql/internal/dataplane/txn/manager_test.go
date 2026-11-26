package txn_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func TestManagerKeepsHistoricalReadsAvailableAfterClockLoss(t *testing.T) {
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
	fixture.clock.reading.Status = 0
	fixture.clock.mu.Unlock()
	reader, err := manager.StrongSnapshot(t.Context())
	if !errors.Is(err, clock.ErrClockUnavailable) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("strong snapshot during assurance loss: %v", err)
	}
	reader, err = manager.HistoricalSnapshot(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	fixture.requireBalance(t, reader)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := fixture.managerConfig(t)
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := neosepia.Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	recovered.Backend = reopened
	recovered.Clock = nil
	recovered.HistoricalReadOnly = true
	historical, err := txn.NewManager(t.Context(), recovered)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := historical.Close(); err != nil {
			t.Error(err)
		}
	})
	reader, err = historical.HistoricalSnapshot(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.requireBalance(t, reader)
	future, err := historical.HistoricalSnapshot(t.Context(), recovered.Recovered.ClosedTS+1)
	if !errors.Is(err, clock.ErrClockUnavailable) {
		if future != nil {
			future.Close()
		}
		t.Fatalf("uncertified future snapshot after clockless reopen: %v", err)
	}
}

type managerBlockedBackend struct {
	storage.Backend
	blocked    atomic.Bool
	blockAfter atomic.Int64
	entered    chan struct{}
}

func (backend *managerBlockedBackend) OpenSnapshot(ctx context.Context, spec storage.SnapshotSpec) (storage.SnapshotReader, error) {
	if backend.blocked.Load() && int64(spec.Fence.ClosedTS) > backend.blockAfter.Load() {
		select {
		case backend.entered <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return backend.Backend.OpenSnapshot(ctx, spec)
}

func TestManagerCloseCancelsBlockedSnapshotAndPreservesPins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := openManagerStore(t)
		backend := &managerBlockedBackend{Backend: fixture.backend, entered: make(chan struct{}, 1)}
		config := fixture.managerConfig(t)
		config.Backend = backend
		config.MaxOperations = 1
		manager, err := txn.NewManager(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := manager.Close(); err != nil {
				t.Error(err)
			}
		})
		pinned, err := manager.HistoricalSnapshot(t.Context(), 20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := pinned.Close(); err != nil {
				t.Error(err)
			}
		})
		backend.blocked.Store(true)
		finished := make(chan error, 1)
		go func() {
			reader, err := manager.StrongSnapshot(t.Context())
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			finished <- err
		}()
		<-backend.entered
		excess, err := manager.HistoricalSnapshot(t.Context(), 20)
		if !errors.Is(err, txn.ErrManagerCapacity) {
			if excess != nil {
				excess.Close()
			}
			t.Fatalf("operation admission while saturated: %v", err)
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked snapshot after close: %v", err)
		}
		fixture.requireBalance(t, pinned)
		rejected, err := manager.HistoricalSnapshot(t.Context(), 20)
		if !errors.Is(err, storage.ErrClosed) {
			if rejected != nil {
				rejected.Close()
			}
			t.Fatalf("snapshot admitted after manager close: %v", err)
		}
	})
}

func TestManagerHistoricalReadDoesNotWaitForPendingBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := openManagerStore(t)
		backend := &managerBlockedBackend{Backend: fixture.backend, entered: make(chan struct{}, 1)}
		config := fixture.managerConfig(t)
		config.Backend = backend
		manager, err := txn.NewManager(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := manager.Close(); err != nil {
				t.Error(err)
			}
		})
		state, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		backend.blockAfter.Store(int64(state.ClosedTS))
		backend.blocked.Store(true)
		finished := make(chan error, 1)
		go func() {
			reader, err := manager.StrongSnapshot(t.Context())
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			finished <- err
		}()
		<-backend.entered
		historical, err := manager.HistoricalSnapshot(t.Context(), 20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := historical.Close(); err != nil {
				t.Error(err)
			}
		})
		fixture.requireBalance(t, historical)
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending read barrier after close: %v", err)
		}
	})
}
