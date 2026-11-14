package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLAtTimeZoneUsesExplicitIntervalOffsets(t *testing.T) {
	graph := sqlGraph(t, `SELECT (to_timestamp(0) AT TIME ZONE interval '5 hours 30 minutes')::text,(make_timestamp(2000,1,1,0,0,0) AT TIME ZONE interval '5 hours 30 minutes' AT TIME ZONE interval '-2 hours')::text,(to_timestamp('Infinity'::float8) AT TIME ZONE interval '1 day')::text,(to_timestamp(0) AT TIME ZONE interval '1.9 seconds')::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{model.StringVal("1970-01-01 05:30:00"), model.StringVal("1999-12-31 16:30:00"), model.StringVal("infinity"), model.StringVal("1970-01-01 00:00:01")}})
	aggregateProjectionReleased(t, env.Query)
}
