package neosepia

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func runPinValues(t *testing.T, pin *runPin) []int64 {
	t.Helper()
	return runPinTargetValues(t, pin, encTestTarget())
}

func runPinTargetValues(t *testing.T, pin *runPin, target storage.ObjectRef) []int64 {
	t.Helper()
	var sources []*runVersionCursor
	err := pin.visit(t.Context(), target, func(file *sstFile) error {
		source, err := newRunVersionCursor(file, 0, file.count, 1<<20, nil)
		if err != nil {
			return err
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := newMVCCCursor(t.Context(), sources, 20, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, storage.ScanForward)
	if err != nil {
		t.Fatal(err)
	}
	spec := scanFixtureSpec()
	spec.Target = target
	cursor, err := newPAXScanCursor(t.Context(), visible, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return collectScanInts(t, cursor)
}

func TestRunGenerationsKeepPinnedFilesAfterOwnerClose(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 8, MaxPins: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	first := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10)))
	if err := set.publish(t.Context(), 1, 10, []*sstFile{first}); err != nil {
		t.Fatal(err)
	}
	old, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	})
	next := []*sstFile{
		openRun(t, writeRun(t, runBlock(t, "b", "", 20, storage.MutationUpsert, 20))),
		openRun(t, writeRun(t, runBlock(t, "c", "", 20, storage.MutationUpsert, 30))),
	}
	if err := set.publish(t.Context(), 2, 20, next); err != nil {
		t.Fatal(err)
	}
	current, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := current.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := current.retain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runPinValues(t, old); !slices.Equal(got, []int64{10}) {
		t.Fatalf("old generation: got %v, want [10]", got)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runPinValues(t, retained); !slices.Equal(got, []int64{10, 20, 30}) {
		t.Fatalf("published generation: got %v, want [10 20 30]", got)
	}
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReadBlock(t.Context(), 0, 1<<20, nil); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("last pin did not close files: %v", err)
	}
}

func TestRunPublicationBudgetFailureDoesNotExposePrefix(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 2, MaxPins: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	first := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10)))
	if err := set.publish(t.Context(), 1, 10, []*sstFile{first}); err != nil {
		t.Fatal(err)
	}
	incoming := []*sstFile{
		openRun(t, writeRun(t, runBlock(t, "b", "", 20, storage.MutationUpsert, 20))),
		openRun(t, writeRun(t, runBlock(t, "c", "", 20, storage.MutationUpsert, 30))),
	}
	if err := set.publish(t.Context(), 2, 20, incoming); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("over-budget publication: %v", err)
	}
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := runPinValues(t, pin); !slices.Equal(got, []int64{10}) {
		t.Fatalf("failed publication exposed prefix: %v", got)
	}
	for _, file := range incoming {
		if _, err := file.ReadBlock(t.Context(), 0, 1<<20, nil); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("rejected file remained open: %v", err)
		}
	}
}
