package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayPositionsPreserveSubscriptsAndNullSearch(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_position(a,x),array_position(a,x,s),array_positions(a,x)::text FROM (VALUES('[-3:0]={1,NULL,1,2}'::integer[],1,-2),('[-3:0]={1,NULL,1,2}'::integer[],NULL::integer,-2),(ARRAY[]::integer[],1,NULL::integer),(NULL::integer[],1,NULL::integer))t(a,x,s)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25}, [][]model.Value{{model.Int64Val(-3), model.Int64Val(-1), model.StringVal("{-3,-1}")}, {model.Int64Val(-2), model.Int64Val(-2), model.StringVal("{-2}")}, {n, n, model.StringVal("{}")}, {n, n, model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayReplacementPreservesBoundsAndNullableElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_remove(a,x)::text,array_replace(a,x,y)::text FROM (VALUES('[-3:0]={1,NULL,1,2}'::integer[],1,9),('[-3:0]={1,NULL,1,2}'::integer[],NULL::integer,3),(ARRAY[]::integer[],1,NULL::integer),(NULL::integer[],1,NULL::integer))t(a,x,y)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, n := model.StringVal, model.NullVal(model.TypeString)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{s("[-3:-2]={NULL,2}"), s("[-3:0]={9,NULL,9,2}")}, {s("[-3:-1]={1,1,2}"), s("[-3:0]={1,3,1,2}")}, {s("{}"), s("{}")}, {n, n}})
	aggregateProjectionReleased(t, env.Query)
}
