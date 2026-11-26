package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLProjectedRecordKeepsDatumComparisonSemantics(t *testing.T) {
	graph := sqlGraph(t, `SELECT r=r,r IN(r),r IS DISTINCT FROM r FROM (SELECT ROW(1,NULL::integer) AS r)s`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true), model.BoolVal(false)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowDistinctUsesPerFieldCoercionAndNulls(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(NULL) IS DISTINCT FROM ROW(NULL),ROW(1::smallint) IS NOT DISTINCT FROM ROW(1::bigint),ROW() IS DISTINCT FROM ROW(),ROW() IS NOT DISTINCT FROM ROW()`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16}, [][]model.Value{{model.BoolVal(false), model.BoolVal(true), model.BoolVal(false), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowDistinctSkipsLaterFieldErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(1,1/n) IS DISTINCT FROM ROW(2,0) FROM generate_series(0,0)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecordCastDoesNotRetainRowComparisonSyntax(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ROW(1,NULL::integer)::record)=ROW(1,NULL::integer)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNamedRecordCastUsesDatumEquality(t *testing.T) {
	graph := sqlGraphWithCatalog(t, `SELECT (ROW('a',NULL::integer[])::pair)=(ROW('a',NULL::integer[])::pair)`, recordSQLCatalog(t), nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowValueUsesDatumEquality(t *testing.T) {
	graph := sqlGraph(t, `SELECT v=v,v IN(v) FROM (VALUES(1,NULL::integer))v(a,b)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
