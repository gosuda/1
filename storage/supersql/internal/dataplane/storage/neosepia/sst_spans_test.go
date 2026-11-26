package neosepia

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPinnedSpanPruningRetainsEarlierCrossingTombstones(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 4, MaxPins: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	for i, item := range []struct {
		key, end string
		kind     storage.MutationOp
	}{{"m", "", storage.MutationUpsert}, {"a", "z", storage.MutationDeleteRange}, {"zz", "", storage.MutationUpsert}} {
		timestamp := model.Timestamp((i + 1) * 10)
		file := openRun(t, writeRun(t, runBlock(t, item.key, item.end, timestamp, item.kind, 10)))
		if err := set.publish(t.Context(), uint64(i+1), timestamp, []*sstFile{file}); err != nil {
			t.Fatal(err)
		}
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
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		for _, timestamp := range []model.Timestamp{15, 25} {
			t.Run(fmt.Sprintf("direction_%d_at_%d", direction, timestamp), func(t *testing.T) {
				spec := scanFixtureSpec()
				spec.Spans = []encodedSpan{{lower: keyCut{prefix: []byte("m")}, upper: keyCut{prefix: []byte("m"), after: true}}}
				cursor, err := pin.scan(t.Context(), timestamp, direction, spec, pinnedScanLimits{SourceBytes: 5 << 20, BlockBytes: 1 << 20, Merge: mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, Reverse: mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := cursor.Close(); err != nil {
						t.Error(err)
					}
				})
				var want []int64
				if timestamp == 15 {
					want = []int64{10}
				}
				if got := collectScanInts(t, cursor); !slices.Equal(got, want) {
					t.Fatalf("span visibility: got %v, want %v", got, want)
				}
			})
		}
	}
}
