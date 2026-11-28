package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRowsFromPadsIndependentGenerators(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM ROWS FROM(generate_series(1,2),generate_series(9,8),generate_subscripts(ARRAY[4,5,6],1,true)) WITH ORDINALITY AS t(a,b,c,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 20}, [][]model.Value{{i(1), n, i(3), i(1)}, {i(2), n, i(2), i(2)}, {n, n, i(1), i(3)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowsFromPadsExpandedRecordColumns(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1,'x'::text),NULL::record]) AS(a integer,b text),generate_series(8,10)) WITH ORDINALITY AS u`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	n, s := model.NullVal(model.TypeInt64), model.NullVal(model.TypeString)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25, 23, 20}, [][]model.Value{{i(1), model.StringVal("x"), i(8), i(1)}, {n, s, i(9), i(2)}, {n, s, i(10), i(3)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLMultiArrayUnnestRebindsLateralInputs(t *testing.T) {
	graph := sqlGraph(t, `SELECT v.id,u.* FROM (VALUES(1,ARRAY[10,11]),(2,NULL::integer[]))v(id,a) CROSS JOIN LATERAL unnest(v.a,ARRAY['x']) WITH ORDINALITY AS u(a,b,n) ORDER BY v.id,u.n`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25, 20}, [][]model.Value{{i(1), i(10), model.StringVal("x"), i(1)}, {i(1), i(11), model.NullVal(model.TypeString), i(2)}, {i(2), model.NullVal(model.TypeInt64), model.StringVal("x"), i(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowsFromEmptySourcesDoNotSuppressLaterArgumentErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM ROWS FROM(generate_series(1,0),generate_series(1,2,0))`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "22023" {
		t.Fatalf("ROWS FROM argument error=%v, want 22023", err)
	}
	aggregateProjectionReleased(t, env.Query)
}
