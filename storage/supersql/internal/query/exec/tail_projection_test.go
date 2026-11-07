package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLLimitDoesNotEvaluateDiscardedTargets(t *testing.T) {
	graph := sqlGraph(t, `SELECT 10/x FROM (VALUES(0),(2)) t(x) ORDER BY x DESC LIMIT 1`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(5)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLHiddenDistinctKeysDoNotLeakIntoResult(t *testing.T) {
	graph := sqlGraph(t, `SELECT DISTINCT ON(x%2) x FROM (VALUES(1),(2),(3),(4)) t(x) ORDER BY x%2,x DESC`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(4)}, {model.Int64Val(3)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLOrderingReusesEquivalentVisibleTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"equal duplicate aliases", `SELECT x AS k,x AS k FROM (VALUES(2),(1)) t(x) ORDER BY k`, [][]model.Value{{model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(2), model.Int64Val(2)}}},
		{"qualified DISTINCT key", `SELECT DISTINCT x FROM (VALUES(2),(1),(2)) t(x) ORDER BY t.x`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}}},
		{"computed target after star", `SELECT DISTINCT t.*,x+1 AS y FROM (VALUES(2,9),(1,7),(2,9)) t(x,z) ORDER BY x+1`, [][]model.Value{{model.Int64Val(1), model.Int64Val(7), model.Int64Val(2)}, {model.Int64Val(2), model.Int64Val(9), model.Int64Val(3)}}},
		{"output alias precedes input", `SELECT -x AS x FROM (VALUES(1),(2)) t(x) ORDER BY x`, [][]model.Value{{model.Int64Val(-2)}, {model.Int64Val(-1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := setWindowEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
