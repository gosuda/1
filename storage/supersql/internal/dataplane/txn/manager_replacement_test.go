package txn_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func TestManagerReplacementClockFencesRecoveredFrontier(t *testing.T) {
	fixture := openManagerStore(t)
	manager, err := txn.NewManager(t.Context(), fixture.managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	initial := manager
	t.Cleanup(func() {
		if err := initial.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.clock.mu.Lock()
	fixture.clock.reading.Earliest, fixture.clock.reading.Latest = 10000, 10010
	fixture.clock.mu.Unlock()
	reader, err := manager.StrongSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.requireBalance(t, reader)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
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
	fixture.backend = reopened
	replacement := newManagerFixtureClock()
	replacement.reading.Epoch.Assurance[0] = 9
	replacement.reading.Epoch.Config[0] = 8
	fixture.clock = replacement
	manager, err = txn.NewManager(t.Context(), fixture.managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	after, err := reopened.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.ClosedTS <= before.ClosedTS || after.CommitTS != before.CommitTS || after.Epoch != replacement.reading.Epoch {
		t.Fatalf("replacement readiness lost recovered frontier: before=%+v after=%+v", before, after)
	}
	reading, err := replacement.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Earliest <= clock.Instant(after.ClosedTS) {
		t.Fatalf("replacement did not certify closed frontier: %v <= %v", reading.Earliest, after.ClosedTS)
	}
	historical, err := manager.HistoricalSnapshot(t.Context(), before.ClosedTS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := historical.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.requireBalance(t, historical)
	if err := historical.Close(); err != nil {
		t.Fatal(err)
	}
}
