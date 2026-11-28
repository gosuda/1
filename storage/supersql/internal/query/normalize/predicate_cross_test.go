package normalize_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func scalarTreeHasSubquery(t *testing.T, graph *ir.Graph, id ir.ScalarID) bool {
	t.Helper()
	seen := make(map[ir.ScalarID]bool)
	var walk func(ir.ScalarID, uint32) bool
	walk = func(cur ir.ScalarID, d uint32) bool {
		if cur == 0 || d > 64 || seen[cur] {
			return false
		}
		seen[cur] = true
		s, ok := graph.Scalar(cur)
		if !ok {
			return false
		}
		if s.Kind == ir.Subquery {
			return true
		}
		for i := range s.OperandCount() {
			child, _ := graph.Operand(cur, i)
			if walk(child, d+1) {
				return true
			}
		}
		return false
	}
	return walk(id, 0)
}

// A conjunct that reads a subquery capture from a later cross input must
// not move into an earlier join: the join cannot supply that capture, and
// verification rejects the plan. It stays in the residual filter.
func TestExistsCaptureStaysInResidualFilter(t *testing.T) {
	sql := `SELECT x FROM (VALUES (1)) a(x), (VALUES (1)) b(y), (VALUES (1)) c(z) WHERE a.x = b.y AND EXISTS (SELECT 1 FROM (VALUES (1)) r(w) WHERE r.w = c.z)`
	graph := normalized(t, bound(t, sql))
	foundResidual := false
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		rel, ok := graph.Rel(id)
		if !ok || rel.Predicate == 0 {
			continue
		}
		if scalarTreeHasSubquery(t, graph, rel.Predicate) {
			if rel.Kind != ir.Filter {
				t.Fatalf("subquery conjunct moved into rel %d kind %v, want residual Filter", id, rel.Kind)
			}
			foundResidual = true
		}
	}
	if !foundResidual {
		t.Fatal("EXISTS conjunct vanished: no predicate references a subquery")
	}
}

// A volatile conjunct pins the whole filter in place: moving any other
// conjunct into a join would evaluate the volatile expression fewer times
// than the nested loop over all pairs.
func TestVolatileConjunctKeepsFilterOverCrossJoin(t *testing.T) {
	sql := `SELECT x FROM (VALUES (1)) a(x), (VALUES (1)) b(y) WHERE random() > 2 AND a.x = b.y`
	graph := normalized(t, bound(t, sql))
	foundFilter := false
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		rel, ok := graph.Rel(id)
		if !ok || rel.Predicate == 0 {
			continue
		}
		if rel.Kind == ir.Join && rel.JoinKind == ir.InnerJoin {
			t.Fatalf("volatile filter rewritten into inner join rel %d", id)
		}
		if rel.Kind == ir.Filter {
			foundFilter = true
		}
	}
	if !foundFilter {
		t.Fatal("volatile filter vanished: no Filter rel left")
	}
}
