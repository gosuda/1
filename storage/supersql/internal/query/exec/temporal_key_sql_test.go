package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLDateGroupingUsesTemporalKeys(t *testing.T) {
	graph := sqlGraph(t, `SELECT d::text,count(*) FROM (VALUES(make_date(2000,1,2)),(make_date(1999,12,31)),(make_date(2000,1,2))) v(d) GROUP BY d ORDER BY d`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("1999-12-31"), model.Int64Val(1)}, {model.StringVal("2000-01-02"), model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimeOrderingKeepsDayEndpointDistinct(t *testing.T) {
	graph := sqlGraph(t, `SELECT t::text FROM (VALUES(make_time(24,0,0)),(make_time(0,0,0)),(make_time(12,0,0)),(make_time(24,0,0))) v(t) GROUP BY t ORDER BY t DESC`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("24:00:00")}, {model.StringVal("12:00:00")}, {model.StringVal("00:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}
