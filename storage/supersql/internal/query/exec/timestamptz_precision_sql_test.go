package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLTimestampTZPrecisionRoundingAndArrayCoercion(t *testing.T) {
	graph := sqlGraph(t, `SELECT to_timestamp(946684800.5)::timestamptz(0),to_timestamp(946684799.5)::timestamp(0) with time zone,(ARRAY[to_timestamp(946684800.125)]::timestamptz(2)[])[1]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	v := func(n int64) model.Value { return model.Value{Type: model.TypeTimestamp, IntVal: n} }
	assertAnyValueRows(t, portal, []model.TypeID{1184, 1184, 1184}, [][]model.Value{{v(1000000), v(-1000000), v(130000)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestPreparedTimestampTZPrecisionRetainsClamp(t *testing.T) {
	prepared := prepareSemanticSQL(t, `SELECT to_timestamp(946684800.123456)::timestamptz(9)`, nil, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	warnings := prepared.Warnings()
	if len(warnings) != 1 || warnings[0].Code != "22023" {
		t.Fatalf("timestamptz precision warning=%v", warnings)
	}
	env := statefulEnvironment(t)
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1184}, [][]model.Value{{{Type: model.TypeTimestamp, IntVal: 123456}}})
	aggregateProjectionReleased(t, env.Query)
}
