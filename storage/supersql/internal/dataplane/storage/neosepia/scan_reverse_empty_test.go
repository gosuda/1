package neosepia

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestReverseOwnOnlyScanKeepsRequestedDirection(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 4, MaxPins: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	_, view := scanOwnView(t, []storage.Mutation{scanOwnMutation(storage.MutationUpsert, "a", "", 1), scanOwnMutation(storage.MutationUpsert, "z", "", 2)})
	spec := scanFixtureSpec()
	spec.Overlay = view
	spec.OverlayLimits = mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}
	spec.OverlayBlockBytes = 1 << 20
	cursor, err := pin.scan(t.Context(), 10, storage.ScanReverse, spec, pinnedScanLimits{SourceBytes: 4 << 20, BlockBytes: 1 << 20, Merge: mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, Reverse: mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("reverse own-only scan: got %v, want [2 1]", got)
	}
}
