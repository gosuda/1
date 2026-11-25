package neosepia

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestLeveledScanDoesNotPinOnePayloadPerFile(t *testing.T) {
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		t.Run(fmt.Sprint(direction), func(t *testing.T) {
			set, err := newRunSet(runSetLimits{MaxBytes: 1<<20, MaxFiles: 128, MaxPins: 8})
			if err != nil { t.Fatal(err) }
			defer func() { if err := set.Close(); err != nil { t.Error(err) } }()
			files := make([]*sstFile, 32)
			want := make([]int64, len(files))
			for i := range files {
				files[i] = openRun(t, writeRun(t, runBlock(t, fmt.Sprintf("%04d", i), "", 10, storage.MutationUpsert, int64(i))))
				files[i].level = 1
				want[i] = int64(i)
			}
			if err := set.publish(t.Context(), 1, 10, files); err != nil { t.Fatal(err) }
			pin, err := set.pin(t.Context())
			if err != nil { t.Fatal(err) }
			defer func() { if err := pin.Close(); err != nil { t.Error(err) } }()
			cursor, err := pin.scan(t.Context(), 10, direction, scanFixtureSpec(), pinnedScanLimits{SourceBytes: 8<<10, BlockBytes: 4<<10, Merge: mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}, Reverse: mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}})
			if err != nil { t.Fatal(err) }
			defer func() { if err := cursor.Close(); err != nil { t.Error(err) } }()
			if direction == storage.ScanReverse { slices.Reverse(want) }
			if got := collectScanInts(t, cursor); !slices.Equal(got, want) { t.Fatalf("leveled scan: got %v, want %v", got, want) }
		})
	}
}
