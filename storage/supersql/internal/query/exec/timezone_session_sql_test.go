package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLAtLocalAndCastsUseSessionZoneRules(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "CET"}}
	prepared := prepareSemanticSQL(t, `SELECT
 (make_timestamp(2024,7,1,12,0,0) AT LOCAL AT TIME ZONE 'UTC')::text,
 (make_timestamp(2024,7,1,12,0,0)::timestamptz AT TIME ZONE 'UTC')::text,
 ((make_timestamp(2024,7,1,12,0,0) AT TIME ZONE interval '0') AT LOCAL)::text,
 ((make_timestamp(2024,7,1,12,0,0) AT TIME ZONE interval '0')::timestamp)::text`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{model.StringVal("2024-07-01 10:00:00"), model.StringVal("2024-07-01 10:00:00"), model.StringVal("2024-07-01 14:00:00"), model.StringVal("2024-07-01 14:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}
