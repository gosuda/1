package txn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// readDatabaseState returns the fixture's durable state bytes.
func readDatabaseState(t *testing.T, directory string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "database.state"))
	if err != nil {
		t.Fatalf("read database.state: %v", err)
	}
	return data
}

// TestClosedReadViewDoesNotAdvanceDurableFence proves the closed-frontier read
// path never certifies a new fence or republishes database.state.
func TestClosedReadViewDoesNotAdvanceDurableFence(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	view, err := fixture.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatalf("CurrentReadView: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close current view: %v", err)
	}
	before := readDatabaseState(t, fixture.config.Directory)
	beforeState, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for iteration := range 2 {
		view, err := fixture.manager.ClosedReadView(t.Context())
		if err != nil {
			t.Fatalf("ClosedReadView %d: %v", iteration, err)
		}
		fixture.balance(t, view, 100)
		if err := view.Close(); err != nil {
			t.Fatalf("close closed view %d: %v", iteration, err)
		}
		if after := readDatabaseState(t, fixture.config.Directory); !bytes.Equal(before, after) {
			t.Fatalf("ClosedReadView %d republished database.state", iteration)
		}
		if state, err := fixture.backend.State(t.Context()); err != nil || state.ClosedTS != beforeState.ClosedTS {
			t.Fatalf("ClosedReadView %d advanced frontier: %+v, err=%v", iteration, state, err)
		}
	}

	submission, err := fixture.manager.submit(t.Context(), fixture.batch(2, 90), false)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submission.State != storage.CommitCommitted {
		t.Fatalf("submission state = %v", submission.State)
	}
	for iteration := range 2 {
		view, err := fixture.manager.ClosedReadView(t.Context())
		if err != nil {
			t.Fatalf("ClosedReadView after commit %d: %v", iteration, err)
		}
		fixture.balance(t, view, 90)
		if err := view.Close(); err != nil {
			t.Fatalf("close closed view after commit %d: %v", iteration, err)
		}
		if after := readDatabaseState(t, fixture.config.Directory); !bytes.Equal(before, after) {
			t.Fatalf("ClosedReadView after commit %d republished database.state", iteration)
		}
		if state, err := fixture.backend.State(t.Context()); err != nil || state.ClosedTS != submission.Result.CommitTS {
			t.Fatalf("ClosedReadView after commit %d ClosedTS = %+v, err=%v, want %d", iteration, state, err, submission.Result.CommitTS)
		}
	}

	// Control: the certifying path advances the closed fence in memory without rewriting database.state.
	view, err = fixture.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatalf("CurrentReadView after commit: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close current view after commit: %v", err)
	}
	if state, err := fixture.backend.State(t.Context()); err != nil || state.ClosedTS <= submission.Result.CommitTS {
		t.Fatalf("CurrentReadView did not advance the closed fence: %+v, err=%v", state, err)
	}
	if after := readDatabaseState(t, fixture.config.Directory); !bytes.Equal(before, after) {
		t.Fatal("CurrentReadView republished database.state during runtime read")
	}
}

// TestClosedReadViewWithoutFrontier pins the sentinel returned before any closed
// timestamp exists: no durable state is readable without certifying one.
func TestClosedReadViewWithoutFrontier(t *testing.T) {
	manager := &Manager{}
	if _, err := manager.ClosedReadView(context.Background()); !errors.Is(err, ErrNoClosedFrontier) {
		t.Fatalf("ClosedReadView without frontier = %v, want ErrNoClosedFrontier", err)
	}
}
