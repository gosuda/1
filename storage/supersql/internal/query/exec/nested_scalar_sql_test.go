package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArraySortFirstAxisAndNullOrder(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_sort(ARRAY[3,1,2])::text,array_sort(ARRAY[[NULL,1],[1,NULL],[0,5]],false,true)::text,array_sort(ARRAY[NULL,2,1],true,false)::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("{1,2,3}"), model.StringVal("{{0,5},{1,NULL},{NULL,1}}"), model.StringVal("{2,1,NULL}")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArraySortSkipsUnneededComparatorLookup(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_sort('{}'::json[])::text,array_sort(ARRAY['{}'::json])::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{model.StringVal("{}"), model.StringVal("{\"{}\"}")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNestedHashesPreserveNumericAndIntervalEquality(t *testing.T) {
	graph := sqlGraph(t, `SELECT hash_array(ARRAY[1.0,NULL]::numeric[])=hash_array('[0:1]={1.00,NULL}'::numeric[]), hash_record_extended(ROW(1.0,INTERVAL '1 month'),42)=hash_record_extended(ROW(1.00,INTERVAL '30 days'),42)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
