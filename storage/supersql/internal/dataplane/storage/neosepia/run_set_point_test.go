package neosepia

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPinnedPointPrunesUnrelatedRunsWithinSourceBudget(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 4, MaxPins: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	for i, key := range []string{"a", "z"} {
		timestamp := model.Timestamp((i + 1) * 10)
		file := openRun(t, writeRun(t, runBlock(t, key, "", timestamp, storage.MutationUpsert, 10)))
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
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	spec := scanFixtureSpec()
	request := storage.ReadRequest{Target: spec.Target, Key: []byte("a"), Layout: spec.Layout, ColumnIDs: spec.Projection}
	limits := pinnedPointLimits{SourceBytes: 3 << 20, BlockBytes: 1 << 20, Merge: mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, Point: pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096}}
	row, err := pin.get(t.Context(), 20, nil, request, limits)
	if err != nil {
		t.Fatal(err)
	}
	if row.Values[0].IntVal != 10 {
		t.Fatalf("point result: %+v", row)
	}
	request.Key = []byte("m")
	if _, err := pin.get(t.Context(), 20, nil, request, limits); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("absent point: %v", err)
	}
}

func TestPinnedOwnPointDoesNotReserveCommittedBlocks(t *testing.T) {
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
	_, view := scanOwnView(t, []storage.Mutation{scanOwnMutation(storage.MutationUpsert, "a", "", 99)})
	spec := scanFixtureSpec()
	row, err := pin.get(t.Context(), 20, view, storage.ReadRequest{Target: spec.Target, Key: []byte("a"), Layout: spec.Layout, ColumnIDs: spec.Projection}, pinnedPointLimits{Point: pointReadLimits{RowBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if row.Values[0].IntVal != 99 || row.CommitTS != 0 {
		t.Fatalf("own point result: %+v", row)
	}
}
