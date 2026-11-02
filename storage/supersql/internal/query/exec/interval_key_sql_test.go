package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntervalGroupingUsesThirtyDayMonths(t *testing.T) {
	graph := sqlGraph(t, `SELECT extract(epoch FROM t),count(*) FROM (VALUES(interval '1 month'),(interval '30 days'),(interval '720 hours'),(interval '-1 microsecond'),(interval '0')) v(t) GROUP BY t ORDER BY t`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1700, 20}, [][]model.Value{{{Type: model.TypeNumeric, StrVal: "-0.000001"}, model.Int64Val(1)}, {{Type: model.TypeNumeric, StrVal: "0.000000"}, model.Int64Val(1)}, {{Type: model.TypeNumeric, StrVal: "2592000.000000"}, model.Int64Val(3)}})
	aggregateProjectionReleased(t, env.Query)
}
