package txn

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestOverlayCheckpointRestoresWritesWithoutChangingFrozenViews(t *testing.T) {
	overlay, err := NewOverlay(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := overlay.Close(); err != nil {
			t.Error(err)
		}
	})
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	point := func(ordinal uint64, value int64) storage.Mutation {
		mutation := storage.NewUpsertMutation(target, []byte{2}, []model.ColumnID{1}, []model.Value{model.Int64Val(value)})
		mutation.Ordinal = ordinal
		return mutation
	}
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{point(1, 10)}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := overlay.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	removed := storage.NewDeleteRangeMutation(target, []byte{1}, []byte{3})
	removed.Ordinal = 2
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{removed, point(3, 20)}); err != nil {
		t.Fatal(err)
	}
	frozen, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := frozen.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := overlay.restore(checkpoint); err != nil {
		t.Fatal(err)
	}
	restored, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		view storage.OverlayView
		want int64
	}{{frozen, 20}, {restored, 10}} {
		mutation, found, err := test.view.Lookup(t.Context(), target, []byte{2})
		if err != nil || !found || mutation.Op != storage.MutationUpsert || len(mutation.Values) != 1 || mutation.Values[0].IntVal != test.want {
			t.Fatalf("snapshot after rollback: found=%v mutation=%+v err=%v, want %d", found, mutation, err, test.want)
		}
	}
	if mutation, found, err := restored.Lookup(t.Context(), target, []byte{1}); err != nil || found {
		t.Fatalf("rollback retained range tombstone: %+v found=%v err=%v", mutation, found, err)
	}
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{point(4, 30)}); err != nil {
		t.Fatal(err)
	}
	if err := overlay.restore(checkpoint); err != nil {
		t.Fatal(err)
	}
	again, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := again.Close(); err != nil {
			t.Error(err)
		}
	}()
	mutation, found, err := again.Lookup(t.Context(), target, []byte{2})
	if err != nil || !found || mutation.Values[0].IntVal != 10 {
		t.Fatalf("reused valid savepoint: %+v found=%v err=%v", mutation, found, err)
	}
}

func TestOverlayCheckpointRejectsAnotherOwnerAndClosedOverlay(t *testing.T) {
	first, err := NewOverlay(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := NewOverlay(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	checkpoint, err := first.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.restore(checkpoint); !errors.Is(err, errOverlayCheckpoint) {
		t.Fatalf("foreign savepoint: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.restore(checkpoint); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("restore closed overlay: %v", err)
	}
}
