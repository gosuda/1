package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLStringToArrayPreservesEmptyAndNullFields(t *testing.T) {
	graph := sqlGraph(t, `SELECT string_to_array(s,','),string_to_array(s,',','') FROM (VALUES(1,'a,,b,'::text),(2,NULL::text))t(k,s) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{1009, 1009}, [][]model.Value{
		{arrayAggregateValue(s("a"), s(""), s("b"), s("")), arrayAggregateValue(s("a"), model.NullVal(model.TypeString), s("b"), model.NullVal(model.TypeString))},
		{model.NullVal(model.TypeArray), model.NullVal(model.TypeArray)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLStringArrayNullAndEmptyDelimiterSemantics(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_to_string(string_to_array('é界',NULL),':'),array_to_string(string_to_array('é界',''),':'),
 cardinality(string_to_array('',NULL)),array_to_string(string_to_array('a,,b',',',''),':','N'),array_to_string(ARRAY[1,NULL,2],NULL,'N')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 23, 25, 25}, [][]model.Value{{s("é:界"), s("é界"), model.Int64Val(0), s("a:N:b"), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayToStringFlattensWithoutLiteralQuoting(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_to_string(ARRAY[[1,NULL],[2,3]],'|'),array_to_string(ARRAY[[1,NULL],[2,3]],'|','N'),
 array_to_string(ARRAY[NULL,NULL]::integer[],'|',''),array_to_string(ARRAY['a,b','"q"','NULL'],';'),array_to_string(ARRAY[1,NULL,2],'|',NULL)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25}, [][]model.Value{{s("1|2|3"), s("1|N|2|3"), s("|"), s(`a,b;"q";NULL`), s("1|2")}})
	aggregateProjectionReleased(t, env.Query)
}
