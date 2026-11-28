package exec_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// hashJoinFragment runs the SQL pipeline, requires the executed plan to hold
// exactly one hash join over wantKeys keys of the resolved equality, and
// returns the fragment for execution. Keyed execution encodes the join keys,
// so running the plan exercises key encoding instead of only selecting it.
func hashJoinFragment(t *testing.T, sql string, wantEquality model.OperatorID, wantKeys int) *fragment.FragmentGraph {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := normalize.Normalize(t.Context(), bound.Graph(), normalize.Options{Limits: irLimits(), MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}})
	if err != nil {
		t.Fatal(err)
	}
	optimized, err := opt.Optimize(t.Context(), logical, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	hashes := 0
	for id := physical.NodeID(1); id <= physical.NodeID(optimized.Plan.NodeCount()); id++ {
		node, err := optimized.Plan.CloneNode(t.Context(), id, 16<<20)
		if err != nil {
			t.Fatal(err)
		}
		if node.Algorithm != physical.HashJoin {
			continue
		}
		hashes++
		if len(node.Join.Keys) != wantKeys {
			t.Fatalf("hash join holds %d keys, want %d: %+v", len(node.Join.Keys), wantKeys, node.Join.Keys)
		}
		for _, key := range node.Join.Keys {
			if key.Equality != wantEquality {
				t.Fatalf("hash join key equality = %d, want %d", key.Equality, wantEquality)
			}
		}
	}
	if hashes != 1 {
		t.Fatalf("plan holds %d hash joins, want exactly 1", hashes)
	}
	graph, err := fragment.Build(t.Context(), optimized.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func valuesInts(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		if i > from {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d)", i)
	}
	return b.String()
}

// A hash join over integer keys executes the encoded keys and returns the
// matching rows: the overlap of 0-29 and 15-44 is 15-29 with the right payload.
func TestSQLHashJoinExecutesIntegerKeys(t *testing.T) {
	var right strings.Builder
	for k := 15; k <= 44; k++ {
		if k > 15 {
			right.WriteByte(',')
		}
		fmt.Fprintf(&right, "(%d,'v%d')", k, k)
	}
	sql := fmt.Sprintf(`SELECT a.k, b.w FROM (VALUES %s) a(k) JOIN (VALUES %s) b(k,w) ON a.k = b.k ORDER BY a.k`, valuesInts(0, 29), right.String())
	portal, err := exec.Open(t.Context(), hashJoinFragment(t, sql, 96, 1), statefulEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var want [][]model.Value
	for k := 15; k <= 29; k++ {
		want = append(want, []model.Value{model.Int64Val(int64(k)), model.StringVal(fmt.Sprintf("v%d", k))})
	}
	assertAnyValueRows(t, portal, []model.TypeID{23, 25}, want)
}

func valuesTimestamps(fromDay, toDay int) string {
	var b strings.Builder
	for d := fromDay; d <= toDay; d++ {
		if d > fromDay {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "('%s'::timestamp)", time.Date(2024, time.January, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05"))
	}
	return b.String()
}

// A hash join over timestamp keys executes the encoded keys and returns the
// matching rows: the overlap of Jan 1-30 and Jan 16-Feb 14 is Jan 16-30.
func TestSQLHashJoinExecutesTimestampKeys(t *testing.T) {
	sql := fmt.Sprintf(`SELECT a.t::text FROM (VALUES %s) a(t) JOIN (VALUES %s) b(t) ON a.t = b.t ORDER BY 1`, valuesTimestamps(1, 30), valuesTimestamps(16, 45))
	portal, err := exec.Open(t.Context(), hashJoinFragment(t, sql, 2060, 1), statefulEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var want [][]model.Value
	for d := 16; d <= 30; d++ {
		want = append(want, []model.Value{model.StringVal(time.Date(2024, time.January, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05"))})
	}
	assertAnyValueRows(t, portal, []model.TypeID{25}, want)
}
