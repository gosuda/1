package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLPredicateSubqueriesPreserveUnknownAndMultiplicity(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"duplicate left rows", `SELECT x FROM (VALUES(1),(2),(2),(NULL)) t(x) WHERE x IN(SELECT x FROM (VALUES(2),(2)) r(x)) ORDER BY x`, [][]model.Value{{model.Int64Val(2)}, {model.Int64Val(2)}}},
		{"non equality ANY", `SELECT x FROM (VALUES(1),(2),(NULL)) t(x) WHERE x < ANY(SELECT y FROM (VALUES(2),(NULL)) r(y))`, [][]model.Value{{model.Int64Val(1)}}},
		{"NOT IN unknown", `SELECT x FROM (VALUES(1),(2),(NULL)) t(x) WHERE x NOT IN(SELECT y FROM (VALUES(2),(NULL)) r(y))`, nil},
		{"ALL empty includes NULL", `SELECT x FROM (VALUES(1),(NULL)) t(x) WHERE x < ALL(SELECT y FROM (VALUES(2)) r(y) WHERE FALSE)`, [][]model.Value{{model.Int64Val(1)}, {model.NullVal(model.TypeInt64)}}},
		{"NOT EXISTS empty", `SELECT x FROM (VALUES(1),(NULL)) t(x) WHERE NOT EXISTS(SELECT y FROM (VALUES(2)) r(y) WHERE FALSE)`, [][]model.Value{{model.Int64Val(1)}, {model.NullVal(model.TypeInt64)}}},
		{"EXISTS null row", `SELECT x FROM (VALUES(1),(NULL)) t(x) WHERE EXISTS(SELECT y FROM (VALUES(NULL::integer)) r(y))`, [][]model.Value{{model.Int64Val(1)}, {model.NullVal(model.TypeInt64)}}},
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

func TestSQLCorrelatedExistsUsesCapturedRows(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"matching rows", `SELECT a.x FROM (VALUES(1),(2),(3),(NULL)) a(x) WHERE EXISTS(SELECT y FROM (VALUES(2),(3)) r(y) WHERE y=a.x)`, [][]model.Value{{model.Int64Val(2)}, {model.Int64Val(3)}}},
		{"negated null comparison", `SELECT a.x FROM (VALUES(1),(2),(NULL)) a(x) WHERE NOT EXISTS(SELECT y FROM (VALUES(2)) r(y) WHERE y=a.x)`, [][]model.Value{{model.Int64Val(1)}, {model.NullVal(model.TypeInt64)}}},
		{"unused target error", `SELECT a.x FROM (VALUES(1),(2)) a(x) WHERE EXISTS(SELECT 1/0 FROM (VALUES(2)) r(y) WHERE y=a.x)`, [][]model.Value{{model.Int64Val(2)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EXISTS rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLCorrelatedQuantifiersPreserveUnknownAndEmptySets(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"IN", `SELECT a.x FROM (VALUES(1,1),(2,1),(NULL,1),(3,0)) a(x,g) WHERE a.x IN(SELECT y FROM (VALUES(2),(NULL)) r(y) WHERE a.g=1)`, [][]model.Value{{model.Int64Val(2)}}},
		{"NOT IN", `SELECT a.x FROM (VALUES(1,1),(2,1),(NULL,1),(3,0)) a(x,g) WHERE a.x NOT IN(SELECT y FROM (VALUES(2),(NULL)) r(y) WHERE a.g=1)`, [][]model.Value{{model.Int64Val(3)}}},
		{"ALL", `SELECT a.x FROM (VALUES(1,1),(2,1),(NULL,1),(3,0)) a(x,g) WHERE a.x <> ALL(SELECT y FROM (VALUES(2),(NULL)) r(y) WHERE a.g=1)`, [][]model.Value{{model.Int64Val(3)}}},
		{"ordered ANY", `SELECT a.x FROM (VALUES(1,3),(3,2),(4,99)) a(x,m) WHERE a.x < ANY(SELECT y FROM (VALUES(2),(3)) r(y) WHERE y>=a.m)`, [][]model.Value{{model.Int64Val(1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("quantified rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
