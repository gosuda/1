package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimeIntervalCastsPreserveFractionalDayRules(t *testing.T) {
	graph := sqlGraph(t, `SELECT (interval '-2 hours')::time::text,make_time(24,0,0)::interval::text,make_time(24,0,0)::interval::time::text,(interval '1 month -2 days 26:00')::time::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{model.StringVal("22:00:00"), model.StringVal("24:00:00"), model.StringVal("00:00:00"), model.StringVal("02:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}
