package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUnnestFlattensDimensionsAndPreservesNullElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT x,n FROM unnest('[-1:0][4:5]={{1,2},{3,NULL}}'::integer[]) WITH ORDINALITY AS u(x,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 20}, [][]model.Value{{i(1), i(1)}, {i(2), i(2)}, {i(3), i(3)}, {model.NullVal(model.TypeInt64), i(4)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestCapturesLateralArrays(t *testing.T) {
	graph := sqlGraph(t, `SELECT v.id,u.x,u.n FROM (VALUES(1,ARRAY['a','b']),(2,NULL::text[]),(3,ARRAY[]::text[]))v(id,a) LEFT JOIN LATERAL unnest(v.a) WITH ORDINALITY AS u(x,n) ON true ORDER BY v.id,u.n`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, s := model.Int64Val, model.StringVal
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25, 20}, [][]model.Value{{i(1), s("a"), i(1)}, {i(1), s("b"), i(2)}, {i(2), model.NullVal(model.TypeString), n}, {i(3), model.NullVal(model.TypeString), n}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestExpandsDefinedRecordsAndNullRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1,'x'::text),NULL::record]) AS(a integer,b text)) WITH ORDINALITY AS u(a,b,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25, 20}, [][]model.Value{{model.Int64Val(1), model.StringVal("x"), model.Int64Val(1)}, {model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestRecordSchemaMismatchIsDeferredToNonnullRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM unnest(ARRAY[ROW(1)]) AS u(a bigint)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "42804" {
		t.Fatalf("record schema error=%v, want 42804", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestNullRecordBypassesDescriptorMismatch(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM unnest(ARRAY[NULL::record]) AS u(a bigint)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{20}, [][]model.Value{{model.NullVal(model.TypeInt64)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestEmptyRecordArrayBypassesDescriptorMismatch(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM unnest(ARRAY[]::record[]) AS u(a bigint)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{20}, nil)
	aggregateProjectionReleased(t, env.Query)
}
