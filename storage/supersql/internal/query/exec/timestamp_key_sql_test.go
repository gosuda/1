package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimestampGroupingPreservesMicrosecondOrder(t *testing.T) {
	graph := sqlGraph(t, `SELECT t::text,count(*) FROM (VALUES(make_timestamp(2000,1,1,0,0,0)),(make_timestamp(1999,12,31,23,59,59.999999)),(make_timestamp(2000,1,1,0,0,0))) v(t) GROUP BY t ORDER BY t`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("1999-12-31 23:59:59.999999"), model.Int64Val(1)}, {model.StringVal("2000-01-01 00:00:00"), model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}
