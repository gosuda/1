package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRowNormalizerRejectsCanceledConstruction(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	normalizer, err := NewRowNormalizer(ctx, normalizerTarget(), []ColumnRule{normalizerColumn(1, model.TypeInt64, true)}, 8192)
	if normalizer != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled construction returned %v, %v", normalizer, err)
	}
}

func TestRowNormalizerConstructionContextIsNotLifetime(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	normalizer, err := NewRowNormalizer(ctx, fixture.request.Target, []ColumnRule{{Layout: fixture.request.Layout[0]}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	transaction := contractBegin(t, fixture.manager, contractOptions(t, normalizer))
	if err := transaction.ApplyMutations(t.Context(), fixture.batch(2, 73).Mutations); err != nil {
		t.Fatal(err)
	}
	contractValues(t, transaction, fixture.request, 73)
}
