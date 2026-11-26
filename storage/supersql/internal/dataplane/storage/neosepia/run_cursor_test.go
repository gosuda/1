package neosepia

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestRunCursorRetainsSelectedColumnsAcrossBlockAdvance(t *testing.T) {
	path := writeRun(t, runBlock(t, "a", "", 2, storage.MutationUpsert, 20), runBlock(t, "a", "", 1, storage.MutationUpsert, 10), runBlock(t, "b", "", 3, storage.MutationUpsert, 30))
	run := openRun(t, path)
	cursor, err := newRunVersionCursor(run, 0, 3, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	first, err := cursor.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selected := first.lease.retain()
	t.Cleanup(func() {
		if err := selected.release(); err != nil {
			t.Error(err)
		}
	})
	older, err := cursor.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(older.key) != "a" || older.timestamp != 1 {
		t.Fatal("run cursor changed version order")
	}
	next, err := cursor.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(next.key) != "b" || next.timestamp != 3 {
		t.Fatal("run cursor lost next key")
	}
	if _, err := cursor.Next(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("end of run: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	column, err := selected.block.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if column.Int64[first.row] != 20 || string(first.key) != "a" {
		t.Fatal("advancing the source invalidated a retained selection")
	}
}

func TestRunCursorCancellationPreservesRetainedSelection(t *testing.T) {
	run := openRun(t, writeRun(t, runBlock(t, "a", "", 1, storage.MutationUpsert, 10)))
	cursor, err := newRunVersionCursor(run, 0, 1, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	row, err := cursor.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selected := row.lease.retain()
	t.Cleanup(func() {
		if err := selected.release(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cursor.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
	column, err := selected.block.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if column.Int64[row.row] != 10 {
		t.Fatal("cancellation changed a retained selection")
	}
}
