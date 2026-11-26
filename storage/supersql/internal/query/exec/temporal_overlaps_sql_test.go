package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTemporalOverlaps(t *testing.T) {
	graph := sqlGraph(t, `SELECT
 (make_time(1,0,0),make_time(2,0,0)) OVERLAPS (make_time(2,0,0),make_time(3,0,0)),
 (make_time(1,0,0),make_time(3,0,0)) OVERLAPS (make_time(2,0,0),NULL::time),
 (make_timestamp(2024,1,31,0,0,0),interval '1 month') OVERLAPS (make_timestamp(2024,2,28,0,0,0),interval '1 day'),
 (NULL::timestamp,NULL::timestamp) OVERLAPS (make_timestamp(2024,1,1,0,0,0),interval '1 day')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16}, [][]model.Value{{model.BoolVal(false), model.BoolVal(true), model.BoolVal(true), model.NullVal(model.TypeBoolean)}})
	aggregateProjectionReleased(t, env.Query)
}
