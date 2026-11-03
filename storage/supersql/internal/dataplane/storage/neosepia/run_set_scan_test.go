package neosepia

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestPinnedScanOutlivesSnapshotAndReleasesFiles(t *testing.T) {
	for _, test := range []struct {
		name      string
		direction storage.ScanDirection
		early     bool
		want      []int64
	}{
		{name: "forward", direction: storage.ScanForward, want: []int64{10, 20, 30, 40}},
		{name: "reverse", direction: storage.ScanReverse, want: []int64{40, 30, 20, 10}},
		{name: "early_close", direction: storage.ScanForward, early: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 4, MaxPins: 4})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := set.Close(); err != nil {
					t.Error(err)
				}
			})
			file := multiRowRun(t)
			if err := set.publish(t.Context(), 1, 10, []*sstFile{file}); err != nil {
				t.Fatal(err)
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
			cursor, err := pin.scan(t.Context(), 10, test.direction, scanFixtureSpec(), pinnedScanLimits{SourceBytes: 4 << 20, BlockBytes: 1 << 20, Merge: mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, Reverse: mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := pin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := set.Close(); err != nil {
				t.Fatal(err)
			}
			if test.early {
				out := columnar.Batch{MaxRows: 1}
				if err := cursor.Next(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				if err := cursor.Close(); err != nil {
					t.Fatal(err)
				}
			} else if got := collectScanInts(t, cursor); !slices.Equal(got, test.want) {
				t.Fatalf("pinned scan: got %v, want %v", got, test.want)
			}
			if _, err := file.ReadBlock(t.Context(), 0, 1<<20, nil); !errors.Is(err, storage.ErrClosed) {
				t.Fatalf("scan retained file after completion: %v", err)
			}
		})
	}
}
