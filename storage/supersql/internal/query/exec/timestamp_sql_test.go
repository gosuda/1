package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimestampArithmeticAndPrecision(t *testing.T) {
	graph := sqlGraph(t, `SELECT (make_timestamp(2020,1,31,23,59,59.999999)+interval '1 month')::text,(make_date(2000,1,1)+make_time(24,0,0))::text,(make_timestamp(2000,1,2,2,0,0)-make_timestamp(2000,1,1,0,0,0))::text,make_timestamp(1999,12,31,23,59,59.999999)::time::text,make_timestamp(1999,12,31,23,59,59.999999)::date::text,make_timestamp(2000,1,1,0,0,0.5)::timestamp(0)::text,make_timestamp(1999,12,31,23,59,59.5)::timestamp(0)::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25, 25, 25}, [][]model.Value{{model.StringVal("2020-02-29 23:59:59.999999"), model.StringVal("2000-01-02 00:00:00"), model.StringVal("1 day 02:00:00"), model.StringVal("23:59:59.999999"), model.StringVal("1999-12-31"), model.StringVal("2000-01-01 00:00:01"), model.StringVal("1999-12-31 23:59:59")}})
	aggregateProjectionReleased(t, env.Query)
}
