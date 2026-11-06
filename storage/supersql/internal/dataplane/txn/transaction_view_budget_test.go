package txn

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// catalogDependencies builds a declared read set shaped like the one binding
// produces for a catalog-heavy statement: one entry per discovered object, each
// with the collection's key layout and an unbounded span.
func catalogDependencies(target storage.ObjectRef, count int) []ReadDependency {
	dependencies := make([]ReadDependency, count)
	for i := range dependencies {
		dependencies[i] = ReadDependency{
			Target:    target,
			KeyLayout: contractKeyLayout(),
			Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		}
	}
	return dependencies
}

// TestReadViewAllowanceFitsCatalogSizedDeclaration pins the transaction's view
// allowance. A single view retains the statement's whole declared read set, and
// that set is proportional to the catalog, so the allowance must serve one long
// view instead of handing each view a share of the allowance divided by the
// maximum view count. The declaration below is far larger than one such share
// (MemoryBytes/4/MaxViews) and must still be admitted.
func TestReadViewAllowanceFitsCatalogSizedDeclaration(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	options := contractOptions(t, policy)
	transaction := contractBegin(t, fixture.manager, options)
	dependencies := catalogDependencies(request.Target, 200)
	view, err := transaction.ReadView(t.Context(), dependencies)
	if err != nil {
		t.Fatalf("catalog-sized declaration of %d entries: %v", len(dependencies), err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadViewAllowanceIsReturnedOnClose checks that a view's charge returns to
// the shared allowance when the view closes: many sequential views must not
// exhaust it.
func TestReadViewAllowanceIsReturnedOnClose(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
	dependencies := catalogDependencies(request.Target, 200)
	for i := range 64 {
		view, err := transaction.ReadView(t.Context(), dependencies)
		if err != nil {
			t.Fatalf("sequential view %d: %v", i, err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReadViewAllowanceStillBoundsOpenViews checks the total retained by
// simultaneously open views stays bounded: the allowance is shared, not
// duplicated per view, and closing one view admits another.
func TestReadViewAllowanceStillBoundsOpenViews(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
	dependencies := catalogDependencies(request.Target, 200)
	var open []ReadView
	for {
		view, err := transaction.ReadView(t.Context(), dependencies)
		if err != nil {
			if !errors.Is(err, errTransactionBudget) {
				t.Fatalf("exhausting the view allowance: %v", err)
			}
			break
		}
		open = append(open, view)
		if len(open) > transaction.maxViews {
			t.Fatalf("admitted %d simultaneous views over a max of %d", len(open), transaction.maxViews)
		}
	}
	if len(open) == 0 {
		t.Fatal("no view was admitted at all")
	}
	if err := open[len(open)-1].Close(); err != nil {
		t.Fatal(err)
	}
	view, err := transaction.ReadView(t.Context(), dependencies)
	if err != nil {
		t.Fatalf("view after releasing one: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	for _, held := range open[:len(open)-1] {
		if err := held.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
