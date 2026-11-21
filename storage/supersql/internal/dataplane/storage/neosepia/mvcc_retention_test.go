package neosepia

import (
	"os"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRetentionMergeKeepsFutureVersionsAndVisibleFloorAnchors(t *testing.T) {
	events := []struct {
		key, end  string
		timestamp model.Timestamp
		kind      storage.MutationOp
		value     int64
	}{
		{key: "a", timestamp: 10, kind: storage.MutationUpsert, value: 10},
		{key: "a", timestamp: 20, kind: storage.MutationUpsert, value: 20},
		{key: "a", timestamp: 30, kind: storage.MutationDelete},
		{key: "a", timestamp: 40, kind: storage.MutationUpsert, value: 40},
		{key: "a", end: "c", timestamp: 15, kind: storage.MutationDeleteRange},
		{key: "a", end: "d", timestamp: 32, kind: storage.MutationDeleteRange},
		{key: "b", timestamp: 10, kind: storage.MutationUpsert, value: 10},
		{key: "b", timestamp: 35, kind: storage.MutationUpsert, value: 35},
		{key: "c", timestamp: 20, kind: storage.MutationUpsert, value: 20},
		{key: "d", timestamp: 20, kind: storage.MutationUpsert, value: 20},
		{key: "e", timestamp: 10, kind: storage.MutationUpsert, value: 10},
		{key: "e", timestamp: 20, kind: storage.MutationDelete},
	}
	for _, test := range []struct {
		name     string
		floor    model.Timestamp
		retained int
	}{
		{name: "no_collection", retained: 12},
		{name: "collected_history", floor: 25, retained: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := make([]*runVersionCursor, len(events))
			for i, event := range events {
				file := openRun(t, writeRun(t, runBlock(t, event.key, event.end, event.timestamp, event.kind, event.value)))
				source, err := newRunVersionCursor(file, 0, file.count, 1<<20, nil)
				if err != nil {
					t.Fatal(err)
				}
				sources[i] = source
			}
			retained, err := newRetentionCursor(t.Context(), sources, test.floor, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := retained.Close(); err != nil {
					t.Error(err)
				}
			})
			output, err := os.CreateTemp(t.TempDir(), "compacted-*.sst")
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
			count, err := appendRetainedRun(t.Context(), writer, retained, encTestLimits())
			if err != nil {
				t.Fatal(err)
			}
			if count != uint64(test.retained) {
				t.Fatalf("retained physical versions: got %d, want %d", count, test.retained)
			}
			if err := writer.Finish(t.Context()); err != nil {
				t.Fatal(err)
			}
			compacted := openRun(t, output.Name())
			for _, snapshot := range []struct {
				timestamp model.Timestamp
				want      []visibleVersion
			}{
				{timestamp: 25, want: []visibleVersion{{key: "a", value: 20}, {key: "c", value: 20}, {key: "d", value: 20}}},
				{timestamp: 30, want: []visibleVersion{{key: "c", value: 20}, {key: "d", value: 20}}},
				{timestamp: 32, want: []visibleVersion{{key: "d", value: 20}}},
				{timestamp: 35, want: []visibleVersion{{key: "b", value: 35}, {key: "d", value: 20}}},
				{timestamp: 40, want: []visibleVersion{{key: "a", value: 40}, {key: "b", value: 35}, {key: "d", value: 20}}},
			} {
				if got := visibleRunVersions(t, []*sstFile{compacted}, snapshot.timestamp); !slices.Equal(got, snapshot.want) {
					t.Fatalf("retained snapshot %d: got %v, want %v", snapshot.timestamp, got, snapshot.want)
				}
			}
		})
	}
}
