package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTypedEmptyArrayRetainsZeroDimensions(t *testing.T) {
	graph := sqlGraph(t, `SELECT cardinality(ARRAY[]::integer[]),array_ndims(ARRAY[]::integer[]),
 array_dims(CAST(ARRAY[] AS text[])),array_length(ARRAY[]::bigint[],1)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25, 23}, [][]model.Value{{model.Int64Val(0), model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.NullVal(model.TypeInt64)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayCastCoercesElementsToDestination(t *testing.T) {
	graph := sqlGraph(t, `SELECT ARRAY[n,NULL]::text[],ARRAY['7','8']::integer[],ARRAY[1.6::numeric]::integer[]
 FROM (VALUES(1),(2)) t(n) ORDER BY n`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, s := model.Int64Val, model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{1009, 1007, 1007}, [][]model.Value{
		{arrayAggregateValue(s("1"), model.NullVal(model.TypeString)), arrayAggregateValue(i(7), i(8)), arrayAggregateValue(i(2))},
		{arrayAggregateValue(s("2"), model.NullVal(model.TypeString)), arrayAggregateValue(i(7), i(8)), arrayAggregateValue(i(2))},
	})
	aggregateProjectionReleased(t, env.Query)
}
