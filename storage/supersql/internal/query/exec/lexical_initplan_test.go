package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
)

func lexicalInitplanEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env := recursiveEnvironment(t)
	env.Apply = apply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10}
	env.Spool.MemoryBytes = 0
	return env
}

func TestSQLLexicalCTEInitplans(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]int64
	}{
		{"outer-row CTE", `SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x*10 AS y) SELECT (SELECT y FROM c) AS y) b`, [][]int64{{1, 10}, {2, 20}, {3, 30}}},
		{"ancestor CTE across deeper rescans", `SELECT a.x,b.k,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x*10 AS y) SELECT q.k,d.y FROM (VALUES(1),(2)) q(k) CROSS JOIN LATERAL (SELECT (SELECT y FROM c) AS y WHERE q.k>0) d) b`, [][]int64{{1, 1, 10}, {1, 2, 10}, {2, 1, 20}, {2, 2, 20}}},
		{"empty first descendant", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x*10 AS y) SELECT d.y FROM (VALUES(0),(1)) q(k) CROSS JOIN LATERAL (SELECT (SELECT y FROM c) AS y WHERE q.k>0) d) b`, [][]int64{{1, 10}, {2, 20}}},
		{"root and local dependencies", `WITH r AS MATERIALIZED (SELECT 7 AS z) SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x*10 AS y) SELECT (SELECT c.y+r.z FROM c CROSS JOIN r) AS y) b`, [][]int64{{1, 17}, {2, 27}}},
		{"root dependency after empty parent", `WITH c AS MATERIALIZED (SELECT 7 AS y) SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (SELECT (SELECT y FROM c) AS y WHERE a.x>1) b`, [][]int64{{2, 7}, {3, 7}}},
		{"CTE local to scalar body", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (WITH c AS MATERIALIZED (SELECT 7 AS y) SELECT y FROM c) AS y WHERE a.x>0) b`, [][]int64{{1, 7}, {2, 7}}},
		{"external producer has local initplan", `WITH r AS MATERIALIZED (SELECT b.y FROM (VALUES(7)) a(z) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.z AS y) SELECT (SELECT y FROM c) AS y) b) SELECT a.x,d.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (SELECT y FROM r) AS y WHERE a.x>0) d`, [][]int64{{1, 7}, {2, 7}}},
		{"inner CTE shadows recursive name", `WITH RECURSIVE t(n) AS (WITH t(n) AS (VALUES(7)) SELECT n FROM t) SELECT n FROM t`, [][]int64{{7}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := lexicalInitplanEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			got := rows(t, portal)
			want := make([][]model.Value, len(scenario.want))
			for i, row := range scenario.want {
				want[i] = make([]model.Value, len(row))
				for j, value := range row {
					want[i][j] = model.Int64Val(value)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lexical rows=%v want=%v", got, want)
			}
			if env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("lexical CTE/cache fixture did not spill")
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLRecursiveStepInitplanTracksWorktable(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL (WITH c AS MATERIALIZED (SELECT n+1 AS n FROM t WHERE n<4) SELECT (SELECT n FROM c) FROM c)) SELECT n FROM t`, nil)
	env := lexicalInitplanEnvironment(t)
	env.Recursive.MaxIterations = 4
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}, {model.Int64Val(4)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("step initplan rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveInitplanOnlyReaderOfLocalCTE(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL (WITH c AS MATERIALIZED (SELECT n+1 AS n FROM t) SELECT (SELECT n FROM c))) SELECT n FROM t LIMIT 4`, nil)
	env := lexicalInitplanEnvironment(t)
	env.Recursive.MaxIterations = 3
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}, {model.Int64Val(4)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("scalar-only worktable rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestLexicalInitplanFailureClosesCTEReaders(t *testing.T) {
	for _, scenario := range []struct{ name, body, code string }{
		{"cardinality", `SELECT y FROM (VALUES(7),(8)) r(y)`, "21000"},
		{"late source error", `SELECT 10/y AS y FROM (VALUES(2),(0)) r(y)`, "22012"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, `WITH c AS MATERIALIZED (`+scenario.body+`) SELECT b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (SELECT y FROM c) AS y WHERE a.x>1) b`, nil)
			env := lexicalInitplanEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != scenario.code || batch.Rows() != 0 {
				t.Fatalf("lexical error=%v rows=%d want=%s", err, batch.Rows(), scenario.code)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestLexicalInitplanStopsBeforeNextOuterOwner(t *testing.T) {
	for _, cancelNext := range []bool{false, true} {
		name := "early close"
		if cancelNext {
			name = "canceled demand"
		}
		t.Run(name, func(t *testing.T) {
			graph := sqlGraph(t, `WITH r AS MATERIALIZED (SELECT 7 AS z) SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x*10 AS y) SELECT (SELECT c.y+r.z FROM c CROSS JOIN r) AS y) b`, nil)
			env := lexicalInitplanEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 2 {
				t.Fatalf("first lexical batch=%+v", batch)
			}
			for i, want := range []int64{1, 17} {
				value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[i], batch.Lane(0), 4096)
				if err != nil || value.IsNull || value.IntVal != want {
					t.Fatalf("first lexical column %d=%v err=%v want=%d", i, value, err, want)
				}
			}
			if cancelNext {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled lexical demand=%v", err)
				}
			}
			closePortal(t, portal)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
