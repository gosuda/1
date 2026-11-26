package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayComparisonOperators(t *testing.T) {
	graph := sqlGraph(t, `SELECT a=b,a<>b,a<b,a>b,a<=b,a>=b,btarraycmp(a,b)
 FROM (VALUES(1,'{1,NULL}'::integer[],'{1,2}'::integer[]),(2,'{1,2}'::integer[],'{1,3}'::integer[]),(3,'{1,NULL}'::integer[],'{1,NULL}'::integer[]))t(k,a,b) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	b, i := model.BoolVal, model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16, 16, 23}, [][]model.Value{
		{b(false), b(true), b(false), b(true), b(false), b(true), i(1)},
		{b(false), b(true), b(true), b(false), b(true), b(false), i(-1)},
		{b(true), b(false), b(false), b(false), b(true), b(true), i(0)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayComparisonPreservesBoundsAndElementSemantics(t *testing.T) {
	graph := sqlGraph(t, `SELECT '[0:1]={1,2}'::integer[] < '[1:2]={1,2}'::integer[],
 ARRAY[[1,2],[3,4]] > ARRAY[1,2,3,4],
 ARRAY['a '::bpchar] = ARRAY['a'::bpchar], ARRAY['a '::varchar] = ARRAY['a'::varchar],
 ARRAY[1.0::numeric,'NaN'::numeric] = ARRAY[1.00::numeric,'NaN'::numeric],
 ARRAY['NaN'::float8] > ARRAY['Infinity'::float8]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	b := model.BoolVal
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16, 16}, [][]model.Value{{b(true), b(true), b(true), b(false), b(true), b(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayEqualityDefersMissingOperatorUntilNeeded(t *testing.T) {
	graph := sqlGraph(t, `SELECT ARRAY[]::json[] = ARRAY[NULL::json],
 CASE WHEN n=0 THEN false ELSE ARRAY[]::json[] = ARRAY[]::json[] END FROM (VALUES(0))t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(false), model.BoolVal(false)}})
	aggregateProjectionReleased(t, env.Query)
	for _, sql := range []string{`SELECT ARRAY[]::json[] = ARRAY[]::json[]`, `SELECT ARRAY[]::json[] < ARRAY[]::json[]`} {
		t.Run(sql, func(t *testing.T) {
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), sqlGraph(t, sql, nil), env)
			if err == nil {
				defer closePortal(t, portal)
				err = portal.Next(t.Context(), &columnar.Batch{})
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "42883" {
				t.Fatalf("missing element operator: %v", err)
			}
		})
	}
}

func TestSQLArrayComparisonInConditionalExpressions(t *testing.T) {
	graph := sqlGraph(t, `SELECT GREATEST(ARRAY[1,NULL],ARRAY[1,2]), NULLIF(ARRAY[1,NULL],ARRAY[1,NULL]), ARRAY[NULL::integer] IS NOT DISTINCT FROM ARRAY[NULL::integer]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := arrayAggregateValue(model.Int64Val(1), model.NullVal(model.TypeInt64))
	assertAnyValueRows(t, portal, []model.TypeID{1007, 1007, 16}, [][]model.Value{{want, model.NullVal(model.TypeArray), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
