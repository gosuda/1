package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayIndexUsesAllDimensionsAndBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT a[-2][4],a[-1][5],a[-3][4],a[-2],a[NULL::integer][4]
 FROM (VALUES('[-2:-1][4:5]={{1,2},{3,4}}'::integer[]))t(a)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 23, 23}, [][]model.Value{{model.Int64Val(1), model.Int64Val(4), n, n, n}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArraySlicesClampAndResetBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT a[-2:-1][4:4]::text,a[:][5:]::text,a[-2:-2]::text,a[-2:-1][4]::text,
 cardinality(a[99:100]),cardinality(a[:][:][:]),a[NULL::integer:1] IS NULL
 FROM (VALUES('[-2:-1][4:5]={{1,2},{3,4}}'::integer[]))t(a)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 23, 23, 16}, [][]model.Value{{s("{{1},{3}}"), s("{{2},{4}}"), s("{{1,2}}"), s("{{1},{3}}"), model.Int64Val(0), model.Int64Val(0), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArraySubscriptsUseAssignmentCastsAndNullSourceMask(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ARRAY[10,20,30])[1.6::numeric],(ARRAY[10,20,30])[2::bigint],
 (NULL::integer[])[1/n],(NULL::integer[])[1/n:1/n],(ARRAY[]::integer[])[1]
 FROM generate_series(0,0)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 1007, 23}, [][]model.Value{{model.Int64Val(20), model.Int64Val(20), model.NullVal(model.TypeInt64), model.NullVal(model.TypeArray), model.NullVal(model.TypeInt64)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArraySubscriptsEvaluateAllBoundsInPostgresOrder(t *testing.T) {
	for _, tc := range []struct{ name, sql, state string }{
		{"NULL index does not skip later expression", `SELECT (ARRAY[1])[NULL::integer][1/n] FROM generate_series(0,0)t(n)`, "22012"},
		{"upper before lower", `SELECT (ARRAY[1])[1/n:gcd((-2147483648)::integer,n)] FROM generate_series(0,0)t(n)`, "22003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), sqlGraph(t, tc.sql, nil), env)
			if err == nil {
				defer closePortal(t, portal)
				err = portal.Next(t.Context(), &columnar.Batch{})
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != tc.state {
				t.Fatalf("bound evaluation order: %v; want %s", err, tc.state)
			}
		})
	}
}
