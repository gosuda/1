package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLOrderedSetDirectArgumentsUseFinalGroupingRow(t *testing.T) {
	graph := sqlGraph(t, `SELECT g, percentile_disc(g::float8 / 10) WITHIN GROUP (ORDER BY x) FROM (VALUES (2, 1), (2, 3), (8, 10), (8, 20)) v(g, x) GROUP BY g ORDER BY g`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{{model.Int64Val(2), model.Int64Val(1)}, {model.Int64Val(8), model.Int64Val(20)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLOrderedSetDirectArgumentSeesOmittedGroupingKeyAsNull(t *testing.T) {
	graph := sqlGraph(t, `SELECT g, percentile_disc(g::float8 / 10) WITHIN GROUP (ORDER BY x) FROM (VALUES (2, 1), (2, 3), (8, 10), (8, 20)) v(g, x) GROUP BY GROUPING SETS ((g), ()) ORDER BY g NULLS LAST`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{
		{model.Int64Val(2), model.Int64Val(1)},
		{model.Int64Val(8), model.Int64Val(20)},
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)},
	})
	aggregateProjectionReleased(t, env.Query)
}
