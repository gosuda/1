package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLMakeIntervalNamedAndDefaultArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT make_interval()::text,make_interval(1,days=>2)::text,make_interval(days:=2,years:=1,secs:=0.0000015)::text,make_interval(secs=>NULL)::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{model.StringVal("00:00:00"), model.StringVal("1 year 2 days"), model.StringVal("1 year 2 days 00:00:00.000002"), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNamedArgumentsEvaluateInDeclarationOrder(t *testing.T) {
	portal, _ := openLateralReplaySQL(t, `SELECT make_interval(mins=>public.tick()::int,hours=>public.tick()::int)::text`, statefulEnvironment(t))
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("01:02:00")}})
}
