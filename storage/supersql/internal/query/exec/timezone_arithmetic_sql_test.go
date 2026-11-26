package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLTimezoneArithmeticDistinguishesDaysFromHours(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT
 ((t+interval '1 day') AT TIME ZONE 'UTC')::text,
 ((t+interval '24 hours') AT TIME ZONE 'UTC')::text,
 ((interval '1 day'+t) AT TIME ZONE 'UTC')::text,
 (date_add(t,interval '1 day','UTC') AT TIME ZONE 'UTC')::text,
 (date_add(t,interval '1 day','America/New_York') AT TIME ZONE 'UTC')::text,
 (date_subtract(date_add(t,interval '1 day'),interval '1 day') AT TIME ZONE 'UTC')::text
 FROM (VALUES(make_timestamp(2024,3,9,12,0,0) AT TIME ZONE 'America/New_York')) v(t)`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25, 25}, [][]model.Value{{model.StringVal("2024-03-10 16:00:00"), model.StringVal("2024-03-10 17:00:00"), model.StringVal("2024-03-10 16:00:00"), model.StringVal("2024-03-10 17:00:00"), model.StringVal("2024-03-10 16:00:00"), model.StringVal("2024-03-09 17:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimezoneArithmeticClampsMonthBeforeDays(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT (make_timestamp(2024,1,31,12,0,0)::timestamptz+interval '1 month 10 days')::text`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("2024-03-10 12:00:00-04")}})
	aggregateProjectionReleased(t, env.Query)
}
