package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLTimestamptzOutputUsesCapturedTimezone(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT to_timestamp(0)::text,ARRAY[to_timestamp(0),to_timestamp(1)]::text,quote_literal(to_timestamp(0)),format('%s',to_timestamp(0))`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{model.StringVal("1969-12-31 19:00:00-05"), model.StringVal(`{"1969-12-31 19:00:00-05","1969-12-31 19:00:01-05"}`), model.StringVal("'1969-12-31 19:00:00-05'"), model.StringVal("1969-12-31 19:00:00-05")}})
	aggregateProjectionReleased(t, env.Query)
}
