package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLComputedQuantifierInputs(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      []model.Value
	}{
		{"uncorrelated IN preserves duplicates", `SELECT x FROM (VALUES(1),(1),(2),(4),(NULL)) a(x) WHERE x+1 IN (SELECT y FROM (VALUES(2),(3)) r(y))`, []model.Value{model.Int64Val(1), model.Int64Val(1), model.Int64Val(2)}},
		{"correlated IN", `SELECT x FROM (VALUES(1),(2),(3),(NULL)) a(x) WHERE x+1 IN (SELECT y FROM (VALUES(2),(3),(NULL)) r(y) WHERE y>=a.x)`, []model.Value{model.Int64Val(1), model.Int64Val(2)}},
		{"correlated ALL empty identity", `SELECT x FROM (VALUES(1),(2),(3),(6),(NULL)) a(x) WHERE x*2 < ALL (SELECT y FROM (VALUES(2),(5),(NULL)) r(y) WHERE y>a.x)`, []model.Value{model.Int64Val(2), model.Int64Val(6), model.NullVal(model.TypeInt64)}},
		{"NOT IN null witness", `SELECT x FROM (VALUES(1),(2),(NULL)) a(x) WHERE x+1 NOT IN (SELECT y FROM (VALUES(2),(NULL)) r(y))`, nil},
		{"NOT IN empty right", `SELECT x FROM (VALUES(1),(NULL)) a(x) WHERE x+1 NOT IN (SELECT y FROM (VALUES(2)) r(y) WHERE FALSE)`, []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64)}},
		{"CASE masks within input", `SELECT x FROM (VALUES(0),(2),(NULL)) a(x) WHERE CASE WHEN x=0 THEN 0 ELSE 10/x END IN (SELECT y FROM (VALUES(0),(5)) r(y))`, []model.Value{model.Int64Val(0), model.Int64Val(2)}},
		{"LIMIT bounds input evaluation", `SELECT x FROM (VALUES(1),(0)) a(x) WHERE 1/x IN (SELECT y FROM (VALUES(1)) r(y)) LIMIT 1`, []model.Value{model.Int64Val(1)}},
		{"correlated LIMIT bounds input evaluation", `SELECT x FROM (VALUES(1),(0)) a(x) WHERE 1/x IN (SELECT y FROM (VALUES(1)) r(y) WHERE y>=a.x) LIMIT 1`, []model.Value{model.Int64Val(1)}},
		{"empty ANY skips input error", `SELECT x FROM (VALUES(0)) a(x) WHERE 1/x = ANY (SELECT y FROM (VALUES(1)) r(y) WHERE y<0)`, nil},
		{"correlated empty ALL skips input error", `SELECT x FROM (VALUES(0)) a(x) WHERE 1/x < ALL (SELECT y FROM (VALUES(1)) r(y) WHERE y<a.x)`, []model.Value{model.Int64Val(0)}},
		{"empty NOT IN skips input error", `SELECT x FROM (VALUES(0)) a(x) WHERE 1/x NOT IN (SELECT y FROM (VALUES(1)) r(y) WHERE y<0)`, []model.Value{model.Int64Val(0)}},
		{"empty left skips RHS error", `SELECT x FROM (SELECT x FROM (VALUES(0)) a(x) WHERE x>1) b WHERE 1/x IN (SELECT 1/y FROM (VALUES(0)) r(y))`, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var got []model.Value
			for _, row := range rows(t, portal) {
				if len(row) != 1 {
					t.Fatalf("output shape=%v", row)
				}
				got = append(got, row[0])
			}
			if !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("quantified rows=%v want=%v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestComputedQuantifierErrorReleasesState(t *testing.T) {
	for _, scenario := range []struct{ name, sql string }{
		{"input", `SELECT x FROM (VALUES(0)) a(x) WHERE 1/x = ANY (SELECT y FROM (VALUES(1)) r(y))`},
		{"right output", `SELECT x FROM (VALUES(1)) a(x) WHERE x+1 IN (SELECT 1/y FROM (VALUES(0)) r(y))`},
		{"right predicate", `SELECT x FROM (VALUES(1)) a(x) WHERE x+1 IN (SELECT y FROM (VALUES(0)) r(y) WHERE 1/y>0)`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" || batch.Rows() != 0 {
				t.Fatalf("computed quantifier error=%v rows=%d", err, batch.Rows())
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestUncorrelatedExistsSkipsUnusedErroringTarget(t *testing.T) {
	graph := sqlGraph(t, `SELECT x FROM (VALUES(1)) a(x) WHERE EXISTS (SELECT 1/0)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("EXISTS result=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}
