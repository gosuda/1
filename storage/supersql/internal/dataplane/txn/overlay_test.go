package txn_test

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"math/rand/v2"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func overlayTarget() storage.ObjectRef {
	return storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
}
func pointMutation(key byte, ordinal uint64) storage.Mutation {
	m := storage.NewUpsertMutation(overlayTarget(), []byte{key}, []model.ColumnID{1}, []model.Value{model.Int64Val(int64(ordinal))})
	m.Ordinal = ordinal
	return m
}
func newOverlay(t *testing.T) *txn.Overlay {
	t.Helper()
	overlay, err := txn.NewOverlay(1 << 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := overlay.Close(); err != nil {
			t.Error(err)
		}
	})
	return overlay
}
func freezeOverlay(t *testing.T, overlay *txn.Overlay) storage.OverlayView {
	t.Helper()
	view, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	return view
}
func checkOverlay(t *testing.T, view storage.OverlayView, want map[byte]storage.Mutation) {
	t.Helper()
	for key := range 32 {
		got, found, err := view.Lookup(t.Context(), overlayTarget(), []byte{byte(key)})
		if err != nil {
			t.Fatal(err)
		}
		expected, present := want[byte(key)]
		if found != present {
			t.Fatalf("key %d presence: got %v, want %v", key, found, present)
		}
		if found && (got.Ordinal != expected.Ordinal || got.Op != expected.Op) {
			t.Fatalf("key %d: got op=%v ordinal=%d present=%v; want op=%v ordinal=%d present=%v", key, got.Op, got.Ordinal, found, expected.Op, expected.Ordinal, present)
		}
	}
}

func TestOverlayRangeSplitsPreserveOrdinalPrecedenceAndFrozenViews(t *testing.T) {
	overlay := newOverlay(t)
	expected := make(map[byte]storage.Mutation)
	random := rand.New(rand.NewPCG(7, 11))
	for i := range 300 {
		old := freezeOverlay(t, overlay)
		before := maps.Clone(expected)
		start := byte(random.IntN(32))
		mutation := pointMutation(start, uint64(i+1))
		switch random.IntN(3) {
		case 0:
			end := start + 1 + byte(random.IntN(32-int(start)))
			mutation = storage.NewDeleteRangeMutation(overlayTarget(), []byte{start}, []byte{end})
			mutation.Ordinal = uint64(i + 1)
			for key := start; key < end; key++ {
				expected[key] = mutation
			}
		case 1:
			mutation = storage.NewDeleteMutation(overlayTarget(), []byte{start})
			mutation.Ordinal = uint64(i + 1)
			expected[start] = mutation
		default:
			expected[start] = mutation
		}
		if err := overlay.StageOwned(t.Context(), []storage.Mutation{mutation}); err != nil {
			t.Fatal(err)
		}
		checkOverlay(t, old, before)
		current := freezeOverlay(t, overlay)
		checkOverlay(t, current, expected)
		if err := errors.Join(old.Close(), current.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverlayStatementFailureRestoresValuesAndOrdinal(t *testing.T) {
	overlay := newOverlay(t)
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{pointMutation(1, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{pointMutation(2, 2), pointMutation(3, 2)}); !errors.Is(err, storage.ErrUnnormalizedBatch) {
		t.Fatalf("duplicate statement ordinal: %v", err)
	}
	checkOverlay(t, freezeOverlay(t, overlay), map[byte]storage.Mutation{1: pointMutation(1, 1)})
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{pointMutation(2, 2)}); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayScansRetainCrossingRangesAfterOwnerClose(t *testing.T) {
	overlay := newOverlay(t)
	interval := storage.NewDeleteRangeMutation(overlayTarget(), []byte{1}, []byte{30})
	interval.Ordinal = 1
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{interval, pointMutation(15, 2)}); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		view := freezeOverlay(t, overlay)
		cursor, err := view.Scan(t.Context(), storage.OverlayScanRequest{Target: overlayTarget(), StartKey: []byte{10}, EndKey: []byte{20}, Direction: direction})
		if err != nil {
			t.Fatal(err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		var got []storage.Mutation
		for {
			m, err := cursor.Next(t.Context())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, m)
		}
		if err := cursor.Close(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("lost crossing range or point: %d mutations", len(got))
		}
		first, last := got[0], got[1]
		if first.Op != storage.MutationDeleteRange || !bytes.Equal(first.Key, []byte{1}) || last.Ordinal != 2 {
			t.Fatal("range/point stream ordering changed")
		}
	}
	view := freezeOverlay(t, overlay)
	retained, err := view.Retain()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := errors.Join(overlay.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	m, ok, err := retained.Lookup(t.Context(), overlayTarget(), []byte{15})
	if err != nil || !ok || m.Ordinal != 2 {
		t.Fatalf("retained overlay lost point precedence: %v", err)
	}
}
