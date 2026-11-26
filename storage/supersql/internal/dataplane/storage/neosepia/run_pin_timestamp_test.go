package neosepia

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestHistoricalCursorPinsReadTimeRatherThanLatestGeneration(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 100, 90, 110)); err != nil {
		t.Fatal(err)
	}
	reader := fixture.snapshot(t, 50)
	plan, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.runs.checkCollectionBoundary(t.Context(), 75); !errors.Is(err, storage.ErrSnapshotPinned) {
		t.Fatalf("historical cursor lost its read-time pin: %v", err)
	}
	if err := fixture.backend.runs.checkCollectionBoundary(t.Context(), 50); err != nil {
		t.Fatalf("boundary at pinned timestamp: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.runs.checkCollectionBoundary(t.Context(), 75); err != nil {
		t.Fatalf("closed cursor retained history: %v", err)
	}
}
