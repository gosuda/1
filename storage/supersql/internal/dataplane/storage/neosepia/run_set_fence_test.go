package neosepia

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCompactedBaseValidatesOlderFenceDecisionBoundary(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{fixture.batch(t, 1, 20, 100, 100), fixture.batch(t, 2, 100, 90, 110)} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		timestamp model.Timestamp
		position  uint64
		valid     bool
	}{
		{"omitted_first_commit", 50, 0, false},
		{"omitted_equal_commit", 100, 1, false},
		{"decision_after_fence", 99, 2, false},
		{"closed_between_commits", 99, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fence, err := fixture.authority.Issue(t.Context(), test.timestamp, test.position, fixture.epoch)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: test.timestamp, Fence: fence})
			if !test.valid {
				if reader != nil {
					if closeErr := reader.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}
				if !errors.Is(err, storage.ErrSnapshotNotClosed) {
					t.Fatalf("accepted inconsistent compacted fence: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			if got := backendBalance(t, reader, 0); got != 100 {
				t.Fatalf("historical fence selected balance %d, want 100", got)
			}
		})
	}
}
