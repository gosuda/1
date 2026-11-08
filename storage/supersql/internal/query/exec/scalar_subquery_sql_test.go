package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLScalarSubqueriesFollowExpressionMasks(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"case branch", `SELECT x,CASE WHEN x>0 THEN (SELECT 10/x) ELSE 7 END FROM (VALUES(0),(2),(NULL)) a(x)`, [][]model.Value{{model.Int64Val(0), model.Int64Val(7)}, {model.Int64Val(2), model.Int64Val(5)}, {model.NullVal(model.TypeInt64), model.Int64Val(7)}}},
		{"later case predicate", `SELECT CASE WHEN x=0 THEN 10 WHEN (SELECT 12/x)>1 THEN 20 ELSE 30 END FROM (VALUES(0),(2),(20),(NULL)) a(x)`, [][]model.Value{{model.Int64Val(10)}, {model.Int64Val(20)}, {model.Int64Val(30)}, {model.Int64Val(30)}}},
		{"coalesce", `SELECT COALESCE(x,(SELECT 8/y),99) FROM (VALUES(1,0),(NULL,2),(NULL,NULL)) a(x,y)`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(4)}, {model.Int64Val(99)}}},
		{"boolean null masks", `SELECT x,(x<>0 AND (SELECT 10/x)>1),(x=0 OR (SELECT 10/x)>1) FROM (VALUES(0),(2),(NULL)) a(x)`, [][]model.Value{{model.Int64Val(0), model.BoolVal(false), model.BoolVal(true)}, {model.Int64Val(2), model.BoolVal(true), model.BoolVal(true)}, {model.NullVal(model.TypeInt64), model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)}}},
		{"masked cardinality", `SELECT CASE WHEN x=0 THEN (SELECT y FROM (VALUES(7),(8)) b(y)) ELSE 9 END FROM (VALUES(1),(2)) a(x)`, [][]model.Value{{model.Int64Val(9)}, {model.Int64Val(9)}}},
		{"empty scalar coalesce", `SELECT COALESCE((SELECT y FROM (VALUES(7)) b(y) WHERE y=x),99) FROM (VALUES(7),(8)) a(x)`, [][]model.Value{{model.Int64Val(7)}, {model.Int64Val(99)}}},
		{"text capture and result", `SELECT COALESCE((SELECT CASE WHEN x='a' THEN x||x ELSE NULL END),'z') FROM (VALUES('a'),('b')) a(x)`, [][]model.Value{{model.StringVal("aa")}, {model.StringVal("z")}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLScalarSubqueriesUseTheirLexicalInputs(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"table function arguments", `SELECT v FROM generate_series((SELECT 1),(SELECT 3)) AS t(v)`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}}},
		{"lateral function argument", `SELECT a.x,t.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL generate_series(1,(SELECT a.x)) AS t(y)`, [][]model.Value{{model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(2), model.Int64Val(1)}, {model.Int64Val(2), model.Int64Val(2)}}},
		{"unnest argument", `SELECT v FROM unnest((SELECT ARRAY[1,2])) AS t(v)`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}}},
		{"aggregate argument", `SELECT sum((SELECT x+1)) FROM (VALUES(1),(2)) a(x)`, [][]model.Value{{model.Int64Val(5)}}},
		{"joined inputs", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN (VALUES(1),(2)) b(y) WHERE (SELECT a.x+b.y)=3 ORDER BY a.x`, [][]model.Value{{model.Int64Val(1), model.Int64Val(2)}, {model.Int64Val(2), model.Int64Val(1)}}},
		{"nested-only cte", `WITH q AS MATERIALIZED (SELECT 7 AS x) SELECT 1+(SELECT x FROM q)`, [][]model.Value{{model.Int64Val(8)}}},
		{"ancestor capture", `SELECT x+(SELECT y+(SELECT a.x) FROM (VALUES(2)) b(y)) FROM (VALUES(3)) a(x)`, [][]model.Value{{model.Int64Val(8)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLScalarSubqueryFailuresPublishNoRows(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"active cardinality", `SELECT CASE WHEN x=1 THEN (SELECT y FROM (VALUES(1),(2)) b(y) WHERE y>=x) ELSE 0 END FROM (VALUES(1)) a(x)`, "21000"},
		{"bounded cardinality demand", `SELECT 1+(SELECT n FROM generate_series(1,1000000) AS t(n))`, "21000"},
		{"earlier subquery", `SELECT (SELECT j FROM generate_series(1,2) AS t(j)),1/(i-1) FROM generate_series(1,1) AS s(i)`, "21000"},
		{"strict argument evaluation", `SELECT (CASE WHEN z=0 THEN NULL::int ELSE z END)+(SELECT 1/z) FROM generate_series(0,0) AS t(z)`, "22012"},
		{"late body error", `SELECT 1+(SELECT 1/(3-n) FROM generate_series(2,3) AS t(n))`, "22012"},
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
			var state *model.Error
			if !errors.As(err, &state) || state.Code != scenario.code {
				t.Fatalf("error=%v want SQLSTATE %s", err, scenario.code)
			}
			if batch.Rows() != 0 {
				t.Fatal("failed scalar subquery exposed output")
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLQuantifiedSubqueriesFollowExpressionMasks(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"exists multiple columns", `SELECT EXISTS(SELECT 1,2),EXISTS(SELECT 1 WHERE false)`, [][]model.Value{{model.BoolVal(true), model.BoolVal(false)}}},
		{"exists skips unused target error", `SELECT EXISTS(SELECT 1/x FROM (VALUES(0)) a(x)),EXISTS(SELECT 1/0)`, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}}},
		{"in and not in nulls", `SELECT x IN (SELECT y FROM (VALUES(1),(NULL)) b(y)),x NOT IN (SELECT y FROM (VALUES(1),(NULL)) b(y)) FROM (VALUES(1),(2),(NULL)) a(x)`, [][]model.Value{{model.BoolVal(true), model.BoolVal(false)}, {model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)}, {model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)}}},
		{"correlated empty sets", `SELECT x=ANY(SELECT y FROM (VALUES(1),(NULL)) b(y) WHERE y<x),x<>ALL(SELECT y FROM (VALUES(1),(NULL)) b(y) WHERE y<x) FROM (VALUES(0),(2),(NULL)) a(x)`, [][]model.Value{{model.BoolVal(false), model.BoolVal(true)}, {model.BoolVal(false), model.BoolVal(true)}, {model.BoolVal(false), model.BoolVal(true)}}},
		{"empty quantified skips left errors", `SELECT 1/x=ANY(SELECT y FROM (VALUES(1)) b(y) WHERE y<x),1/x<ALL(SELECT y FROM (VALUES(1)) b(y) WHERE y<x) FROM (VALUES(0)) a(x)`, [][]model.Value{{model.BoolVal(false), model.BoolVal(true)}}},
		{"all null and false dominance", `SELECT x<ALL(SELECT y FROM (VALUES(2),(NULL)) b(y)) FROM (VALUES(1),(3),(NULL)) a(x)`, [][]model.Value{{model.NullVal(model.TypeBoolean)}, {model.BoolVal(false)}, {model.NullVal(model.TypeBoolean)}}},
		{"case skips body errors", `SELECT CASE WHEN x=0 THEN false ELSE x=ANY(SELECT 4/x) END FROM (VALUES(0),(2),(NULL)) a(x)`, [][]model.Value{{model.BoolVal(false)}, {model.BoolVal(true)}, {model.NullVal(model.TypeBoolean)}}},
		{"coalesce skips body errors", `SELECT COALESCE(x>0,2=ANY(SELECT 4/y),false) FROM (VALUES(1,0),(NULL,2),(NULL,NULL)) a(x,y)`, [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(true)}, {model.BoolVal(false)}}},
		{"nested scalar left operand", `SELECT (SELECT x)=ANY(SELECT y FROM (VALUES(1),(2)) b(y)) FROM (VALUES(1),(3)) a(x)`, [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(false)}}},
		{"ancestor captures", `SELECT EXISTS(SELECT 1 FROM (VALUES(2)) b(y) WHERE a.x=ANY(SELECT b.y)) FROM (VALUES(2),(3),(NULL)) a(x)`, [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(false)}, {model.BoolVal(false)}}},
		{"any short circuits", `SELECT 1=ANY(SELECT 1/(2-n) FROM generate_series(1,2) t(n))`, [][]model.Value{{model.BoolVal(true)}}},
		{"all short circuits", `SELECT 0=ALL(SELECT 1/(2-n) FROM generate_series(1,2) t(n))`, [][]model.Value{{model.BoolVal(false)}}},
		{"bounded existential demand", `SELECT EXISTS(SELECT n FROM generate_series(1,1000000) t(n))`, [][]model.Value{{model.BoolVal(true)}}},
		{"composite single column", `SELECT (SELECT ROW(1,2))=ANY(SELECT ROW(1,2)),(SELECT ROW(1,3))<ALL(SELECT ROW(2,0))`, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLQuantifiedSubqueryFailureReleasesChild(t *testing.T) {
	graph := sqlGraph(t, `SELECT 0=ANY(SELECT 1/(2-n) FROM generate_series(1,2) t(n))`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "22012" {
		t.Fatalf("error=%v want SQLSTATE 22012", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("failed quantified subquery exposed output")
	}
	aggregateProjectionReleased(t, env.Query)
}
