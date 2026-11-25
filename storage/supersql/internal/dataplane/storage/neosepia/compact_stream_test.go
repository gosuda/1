package neosepia

import (
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestRetainedRunWriteFailurePreservesInput(t *testing.T) {
	run := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 100)))
	input, err := newRunVersionCursor(run, 0, run.count, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newRetentionCursor(t.Context(), []*runVersionCursor{input}, 10, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	output, err := os.OpenFile(t.TempDir()+"/readonly.sst", os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := output.Close(); err != nil {
			t.Error(err)
		}
	})
	writer, err := newSSTWriter(output, encTestTarget(), runLimits())
	if err != nil {
		t.Fatal(err)
	}
	count, err := appendRetainedRun(t.Context(), writer, source, encTestLimits())
	if err == nil || count != 0 {
		t.Fatalf("failed compaction returned rows=%d error=%v", count, err)
	}
	got := visibleRunVersions(t, []*sstFile{run}, 10)
	if len(got) != 1 || got[0].key != "a" || got[0].value != 100 {
		t.Fatalf("failed compaction damaged its input: %v", got)
	}
}

func TestRetainedRunEliminatesDeletedTarget(t *testing.T) {
	run := openRun(t, writeRun(t,
		runBlock(t, "a", "", 20, storage.MutationDelete, 0),
		runBlock(t, "a", "", 10, storage.MutationUpsert, 100),
	))
	input, err := newRunVersionCursor(run, 0, run.count, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newRetentionCursor(t.Context(), []*runVersionCursor{input}, 30, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	output, err := os.CreateTemp(t.TempDir(), "empty-*.sst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := output.Close(); err != nil {
			t.Error(err)
		}
	})
	writer, err := newSSTWriter(output, encTestTarget(), runLimits())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := appendRetainedRun(t.Context(), writer, source, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("deleted target retained %d physical versions", rows)
	}
	info, err := output.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("empty compaction emitted %d bytes of publishable data", info.Size())
	}
}
