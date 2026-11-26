package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLCatalogVectorOrderingUsesLengthBeforeElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT v::text FROM (VALUES ('1 2'::oidvector),('9'::oidvector),(''::oidvector),('1 2 3'::oidvector)) t(v) ORDER BY t.v`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("")}, {model.StringVal("9")}, {model.StringVal("1 2")}, {model.StringVal("1 2 3")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLEmptyCatalogVectorRetainsArrayDimension(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_ndims(v::smallint[]),array_lower(v::smallint[],1),array_upper(v::smallint[],1),v::text FROM (VALUES (''::int2vector)) t(v)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 25}, [][]model.Value{{model.Int64Val(1), model.Int64Val(0), model.Int64Val(-1), model.StringVal("")}})
	aggregateProjectionReleased(t, env.Query)
}
