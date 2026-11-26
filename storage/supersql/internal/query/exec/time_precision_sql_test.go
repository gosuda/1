package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimePrecisionCastsAndArrayElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT make_time(0,0,0.5)::time(0)::text,make_time(23,59,59.999999)::time(0)::text,(ARRAY[make_time(0,0,0.125),make_time(0,0,0.135)]::time(2)[])::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("00:00:01"), model.StringVal("24:00:00"), model.StringVal("{00:00:00.13,00:00:00.14}")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestPreparedTimePrecisionRetainsClampWarning(t *testing.T) {
	prepared := prepareSemanticSQL(t, `SELECT make_time(0,0,0.123456)::time(9)::text`, nil, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	warnings := prepared.Warnings()
	if len(warnings) != 1 || warnings[0].Code != "22023" {
		t.Fatalf("time precision warning=%v", warnings)
	}
	env := statefulEnvironment(t)
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("00:00:00.123456")}})
	aggregateProjectionReleased(t, env.Query)
}
