package txn

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestMutationLogCursorLimitReleasesAdmissionOnClose(t *testing.T) {
	limits := mutationLogTestLimits(t, true)
	limits.MaxCursors = 2
	log := mutationLogTestOpen(t, limits)
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	sequence := mutationLogTestSeal(t, log)
	first, second := mutationLogTestCursor(t, sequence), mutationLogTestCursor(t, sequence)
	excess, err := sequence.Open(t.Context())
	if excess != nil {
		if closeErr := excess.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, errMutationLogBudget) {
		t.Fatalf("unbounded live cursors: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := mutationLogTestCursor(t, sequence)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []storage.MutationCursor{second, replacement} {
		mutation, err := cursor.Next(t.Context())
		if err != nil || mutation.Ordinal != 1 || len(mutation.Values) != 1 || len(mutation.Values[0].BytesVal) != (32<<10) {
			t.Fatalf("live cursor lost sealed data after owner close: %+v %v", mutation, err)
		}
	}
}
