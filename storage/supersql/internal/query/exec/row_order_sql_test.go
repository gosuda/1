package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRowOrderingUsesCrossTypeBtreeSupport(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(1::smallint,5::bigint)<ROW(2::bigint,1::smallint),ROW(1::smallint,2::integer)<=ROW(1::bigint,2::smallint),ROW(1::integer,3::bigint)>ROW(1::smallint,2::integer),ROW(1::bigint,2::smallint)>=ROW(1::integer,2::bigint),ROW('a'::name,2)<ROW('b'::text,1)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes := model.BoolVal(true)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16}, [][]model.Value{{yes, yes, yes, yes, yes}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowOrderingDistinguishesNullFieldFromNestedRecord(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(NULL::integer,1)<ROW(2,3),ROW(ROW(NULL::integer),0)>ROW(ROW(1),0),ROW(ARRAY[1,2],0)<ROW(ARRAY[2,1],0),ROW('NaN'::float4,0)>ROW('Infinity'::float8,0)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes := model.BoolVal(true)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16}, [][]model.Value{{model.NullVal(model.TypeBoolean), yes, yes, yes}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowOrderingMasksLaterFieldErrorsPerLane(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,ROW(n,1/(n-1))<ROW(2,0) FROM generate_series(1,3)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 16}, [][]model.Value{{model.Int64Val(1), model.BoolVal(true)}, {model.Int64Val(2), model.BoolVal(false)}, {model.Int64Val(3), model.BoolVal(false)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowOrderingNullStopsBeforeLaterPair(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(NULL::integer,1/n)<ROW(2,3) FROM generate_series(0,0)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.NullVal(model.TypeBoolean)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowOrderingEvaluatesBothCurrentPairOperands(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(NULL::integer,0)<ROW(1/n,0) FROM generate_series(0,0)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "22012" {
		t.Fatalf("current-pair error=%v, want 22012", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLSingleFieldRowUsesOrdinaryBooleanOperator(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(ARRAY[1]) @> ROW(ARRAY[1]),ROW(1)<ROW(2)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
