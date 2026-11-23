package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayInputPreservesBoundsAndQuotedNulls(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{1,NULL,3}'::integer[], '[-2:-1][4:5]={{"a,b","NULL"},{"","x"}}'::text[]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	integers := arrayAggregateValue(model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3))
	texts := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.StringVal("a,b"), model.StringVal("NULL"), model.StringVal(""), model.StringVal("x")}}
	assertAnyValueRows(t, portal, []model.TypeID{1007, 1009}, [][]model.Value{{integers, texts}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayTextCastsApplyExplicitElementTypmods(t *testing.T) {
	graph := sqlGraph(t, `SELECT ('{"abcdef","xy"}'::varchar(3)[])::text, ('[0:1]={1.25,-2.35}'::numeric(4,1)[])::text,
 ('{NULL,"NULL","a,b",""}'::text[])::text, (ARRAY[1,2])::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{s("{abc,xy}"), s("[0:1]={1.3,-2.4}"), s(`{NULL,"NULL","a,b",""}`), s("{1,2}")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayTextRuntimeRoundTripPreservesNull(t *testing.T) {
	graph := sqlGraph(t, `SELECT s::integer[]::text FROM (VALUES(1,'{1,2}'::text),(2,'[0:1]={3,4}'::text),(3,NULL::text))t(k,s) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("{1,2}")}, {model.StringVal("[0:1]={3,4}")}, {model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}
