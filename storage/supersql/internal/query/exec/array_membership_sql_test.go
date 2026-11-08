package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayMembershipIgnoresShapeButNotNulls(t *testing.T) {
	graph := sqlGraph(t, `SELECT a @> b,a <@ b,a && b FROM (VALUES(ARRAY[[1,2],[3,NULL]],ARRAY[1,1]),(ARRAY[NULL::integer],ARRAY[NULL::integer]),(ARRAY[]::integer[],ARRAY[]::integer[]),(NULL::integer[],ARRAY[]::integer[]))t(a,b)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes, no, n := model.BoolVal(true), model.BoolVal(false), model.NullVal(model.TypeBoolean)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16}, [][]model.Value{{yes, no, yes}, {no, no, no}, {yes, yes, no}, {n, n, n}})
	aggregateProjectionReleased(t, env.Query)
}
