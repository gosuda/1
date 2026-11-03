package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLWindowPartitionsUseDeclaredArrayOrdering(t *testing.T) {
	graph := sqlGraph(t, `SELECT a::text,row_number() OVER (PARTITION BY a ORDER BY x) FROM (VALUES (ARRAY[1],2),(ARRAY[1],1),(ARRAY[2],3)) AS t(a,x) ORDER BY t.a,x`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{
		{model.StringVal("{1}"), model.Int64Val(1)},
		{model.StringVal("{1}"), model.Int64Val(2)},
		{model.StringVal("{2}"), model.Int64Val(1)},
	})
	aggregateProjectionReleased(t, env.Query)
}
