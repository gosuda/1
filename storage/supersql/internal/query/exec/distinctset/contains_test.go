package distinctset_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestContainsDoesNotAdmitKeys(t *testing.T) {
	for _, storage := range []string{"memory", "spill"} {
		t.Run(storage, func(t *testing.T) {
			f := setup(t, []model.TypeLayout{intLayout})
			f.options.MaxKeys = 1
			if storage == "spill" {
				f.options.MemoryBytes = 0
			}
			s := create(t, f)
			insert(t, s, []model.Value{model.Int64Val(1)}, true)
			for _, key := range []int64{0, 1, 2, 1, 2} {
				found, err := s.Contains(t.Context(), []model.Value{model.Int64Val(key)})
				if err != nil || found != (key == 1) {
					t.Fatalf("Contains(%d)=%t,%v; want %t", key, found, err, key == 1)
				}
			}
			if inserted, err := s.Insert(t.Context(), []model.Value{model.Int64Val(2)}); inserted || !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("new key after read-only misses=%t,%v; want original key quota", inserted, err)
			}
			released(t, f)
		})
	}
}

func TestContainsCancellationReleasesSpilledState(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MemoryBytes = 0
	s := create(t, f)
	insert(t, s, []model.Value{model.Int64Val(1)}, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if found, err := s.Contains(ctx, []model.Value{model.Int64Val(1)}); found || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup=%t,%v", found, err)
	}
	if _, err := s.Contains(t.Context(), []model.Value{model.Int64Val(1)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup after cancellation=%v, want fenced error", err)
	}
	released(t, f)
}
