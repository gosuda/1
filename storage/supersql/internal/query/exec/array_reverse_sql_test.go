package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayReversePreservesInnerOrderAndBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_reverse(a)::text FROM (VALUES('[-2:0][4:5]={{1,2},{3,NULL},{5,6}}'::integer[]),(ARRAY[]::integer[]),(NULL::integer[]))t(a)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("[-2:0][4:5]={{5,6},{3,NULL},{1,2}}")}, {model.StringVal("{}")}, {model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}
