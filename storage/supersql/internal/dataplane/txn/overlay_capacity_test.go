package txn_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

func TestOverlayTransferBoundsRetainedBuffers(t *testing.T) {
	key := pointMutation('b', 2)
	key.Key = make([]byte, 1, 1<<20)
	key.Key[0] = 'b'
	payload := pointMutation('b', 2)
	payload.Values[0] = model.BytesVal(make([]byte, 1, 1<<20))
	tail := pointMutation('b', 2)
	values := make([]model.Value, 2)
	values[0] = model.Int64Val(2)
	values[1] = model.BytesVal(make([]byte, 1<<20))
	tail.Values = values[:1]
	for _, test := range []struct {
		name     string
		mutation storage.Mutation
	}{{"key_capacity", key}, {"payload_capacity", payload}, {"unselected_value_tail", tail}} {
		t.Run(test.name, func(t *testing.T) {
			overlay, err := txn.NewOverlay(4096)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := overlay.Close(); err != nil {
					t.Error(err)
				}
			})
			err = overlay.StageOwned(t.Context(), []storage.Mutation{pointMutation('a', 1), test.mutation})
			if !errors.Is(err, ordered.ErrBudget) {
				t.Fatalf("oversized retained transfer: got %v, want budget error", err)
			}
			view := freezeOverlay(t, overlay)
			if _, found, err := view.Lookup(t.Context(), overlayTarget(), []byte("a")); err != nil || found {
				t.Fatalf("failed statement retained its prefix: found=%v error=%v", found, err)
			}
			if err := overlay.StageOwned(t.Context(), []storage.Mutation{pointMutation('a', 1)}); err != nil {
				t.Fatalf("failed transfer consumed ordinal or budget: %v", err)
			}
		})
	}
}
