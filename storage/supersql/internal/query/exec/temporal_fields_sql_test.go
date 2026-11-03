package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTemporalFieldsAndSymbolicAge(t *testing.T) {
	graph := sqlGraph(t, `SELECT extract(second FROM make_time(12,34,56.123456)),extract(epoch FROM make_timestamp(1970,1,1,0,0,0.000001)),date_trunc('week',make_timestamp(2021,1,1,12,0,0))::text,age(make_timestamp(2004,6,1,0,0,0),make_timestamp(2004,4,30,0,0,0))::text,date_trunc('milliseconds',make_timestamp(2000,1,1,0,0,0.999999))::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1700, 1700, 25, 25, 25}, [][]model.Value{{{Type: model.TypeNumeric, StrVal: "56.123456"}, {Type: model.TypeNumeric, StrVal: "0.000001"}, model.StringVal("2020-12-28 00:00:00"), model.StringVal("1 mon 1 day"), model.StringVal("2000-01-01 00:00:00.999")}})
	aggregateProjectionReleased(t, env.Query)
}
