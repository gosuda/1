package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLMakeTimestamptzResolvesCivilGap(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT make_timestamptz(2024,3,10,2,30,0)::text,make_timestamptz(2024,3,10,2,30,0,'UTC')::text`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{model.StringVal("2024-03-10 03:30:00-04"), model.StringVal("2024-03-09 21:30:00-05")}})
	aggregateProjectionReleased(t, env.Query)
}
