package ordered_test

import (
	"cmp"
	"errors"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type key int

func (k key) Compare(other key) int { return cmp.Compare(k, other) }

func checkTree(t *testing.T, tree ordered.Tree[key, int], want map[key]int) {
	t.Helper()
	if tree.Len() != uint64(len(want)) {
		t.Fatal("tree lost or duplicated entries")
	}
	keys := slices.Sorted(maps.Keys(want))
	for _, reverse := range []bool{false, true} {
		cursor := tree.First(reverse)
		for i := range keys {
			expected := keys[i]
			if reverse {
				expected = keys[len(keys)-1-i]
			}
			got, value, ok := cursor.Next()
			if !ok || got != expected || value != want[expected] {
				t.Fatalf("ordered view: key=%d value=%d present=%v, want %d/%d", got, value, ok, expected, want[expected])
			}
		}
		if _, _, ok := cursor.Next(); ok {
			t.Fatal("cursor repeated entries")
		}
		cursor.Close()
	}
}

func TestPersistentTreeUpdatesPreserveFrozenViews(t *testing.T) {
	var tree ordered.Tree[key, int]
	current := make(map[key]int)
	random := rand.New(rand.NewPCG(1, 2))
	for i := range 2000 {
		old, expected := tree, maps.Clone(current)
		k := key(random.IntN(128))
		var err error
		if random.IntN(3) == 0 {
			tree, _, err = tree.Delete(k, 65536)
			delete(current, k)
		} else {
			tree, _, err = tree.Set(k, i, 65536)
			current[k] = i
		}
		if err != nil {
			t.Fatal(err)
		}
		checkTree(t, tree, current)
		checkTree(t, old, expected)
	}
}

func TestPersistentTreeSeeksAndBudgetFailures(t *testing.T) {
	var tree ordered.Tree[key, int]
	var err error
	for _, k := range []key{2, 4, 6, 8} {
		tree, _, err = tree.Set(k, int(k), 65536)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, reverse := range []bool{false, true} {
		cursor := tree.Seek(5, reverse)
		k, _, ok := cursor.Next()
		want := key(6)
		if reverse {
			want = 4
		}
		if !ok || k != want {
			t.Fatalf("seek: key=%d present=%v want=%d", k, ok, want)
		}
		cursor.Close()
	}
	failed, _, err := tree.Set(5, 5, 1)
	if !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("node budget: %v", err)
	}
	checkTree(t, failed, map[key]int{2: 2, 4: 4, 6: 6, 8: 8})
	if _, ok := tree.Get(5); ok {
		t.Fatal("failed update leaked into a frozen view")
	}
}
