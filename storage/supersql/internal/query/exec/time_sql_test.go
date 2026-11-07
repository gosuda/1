package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimeConstructorAndIntervalArithmetic(t *testing.T) {
	graph := sqlGraph(t, `SELECT make_time(24,0,0)::text,(make_time(23,59,59)+interval '2 seconds')::text,(interval '2 seconds'+make_time(23,59,59))::text,(make_time(0,0,1)-interval '2 seconds')::text,make_time(24,0,0)>make_time(0,0,0),extract(epoch FROM make_time(1,0,0)-make_time(0,0,0))`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 16, 1700}, [][]model.Value{{model.StringVal("24:00:00"), model.StringVal("00:00:01"), model.StringVal("00:00:01"), model.StringVal("23:59:59"), model.BoolVal(true), {Type: model.TypeNumeric, StrVal: "3600.000000"}}})
	aggregateProjectionReleased(t, env.Query)
}
