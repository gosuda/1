package neosepia

import (
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestReverseMVCCPreservesCrossBlockHistoryAndRangeBoundaries(t *testing.T) {
	points := func(keys []string, times []model.Timestamp, values []int64) []byte {
		t.Helper()
		ordinals := make([]uint64, len(keys))
		kinds := make([]storage.MutationOp, len(keys))
		for i := range keys {
			ordinals[i] = 1
			kinds[i] = storage.MutationUpsert
		}
		batch := columnar.Batch{Length: len(keys), Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: len(keys), Int64: values}}}
		versions := blockVersions{Keys: encTestKeyVector(keys...), CommitTS: times, Ordinals: ordinals, Kinds: kinds}
		data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, []model.ColumnID{1}, encTestLimits())
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	pointRun := openRun(t, writeRun(t,
		points([]string{"a", "b", "c"}, []model.Timestamp{10, 10, 32}, []int64{1, 2, 32}),
		points([]string{"c", "d", "m"}, []model.Timestamp{10, 10, 30}, []int64{3, 4, 30}),
		points([]string{"m", "z"}, []model.Timestamp{10, 10}, []int64{5, 6}),
	))
	rangeRun := openRun(t, writeRun(t,
		runBlock(t, "a", "b", 40, storage.MutationDeleteRange, 0),
		runBlock(t, "b", "z", 20, storage.MutationDeleteRange, 0),
		runBlock(t, "c", "d", 30, storage.MutationDeleteRange, 0),
	))
	for _, test := range []struct {
		timestamp model.Timestamp
		want      []string
	}{
		{15, []string{"z=6", "m=5", "d=4", "c=3", "b=2", "a=1"}},
		{25, []string{"z=6", "a=1"}},
		{35, []string{"z=6", "m=30", "c=32", "a=1"}},
		{45, []string{"z=6", "m=30", "c=32"}},
	} {
		t.Run(fmt.Sprint(test.timestamp), func(t *testing.T) {
			sources := make([]*runVersionCursor, 0, 2)
			for _, run := range []*sstFile{pointRun, rangeRun} {
				source, err := newReverseRunVersionCursor(run, 0, run.count, 1<<20, mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				})
				sources = append(sources, source)
			}
			cursor, err := newMVCCCursor(t.Context(), sources, test.timestamp, mergeLimits{MaxBytes: 16384, MaxKeyBytes: 64}, storage.ScanReverse)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []string
			for {
				row, err := cursor.Next(t.Context())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				vector, err := row.lease.block.Column(t.Context(), 1)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, fmt.Sprintf("%s=%d", row.key, vector.Int64[row.row]))
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("snapshot %d: got %v, want %v", test.timestamp, got, test.want)
			}
		})
	}
}
