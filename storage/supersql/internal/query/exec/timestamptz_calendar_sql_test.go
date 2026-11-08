package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLTimestamptzTruncationPreservesFoldOffset(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT date_trunc('hour',t)::text,date_trunc('day',t)::text,date_trunc('day',t,'UTC')::text FROM (VALUES(make_timestamp(2024,11,3,5,30,0) AT TIME ZONE interval '0')) v(t)`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("2024-11-03 01:00:00-04"), model.StringVal("2024-11-03 00:00:00-04"), model.StringVal("2024-11-02 20:00:00-04")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestamptzAgeUsesCivilFieldsNotElapsedDuration(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT age(make_timestamp(2024,11,3,6,15,0) AT TIME ZONE interval '0',make_timestamp(2024,11,3,5,45,0) AT TIME ZONE interval '0'),age(make_timestamp(2024,3,10,16,0,0) AT TIME ZONE interval '0',make_timestamp(2024,3,9,17,0,0) AT TIME ZONE interval '0')`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1186, 1186}, [][]model.Value{{{Type: model.TypeInterval, IntervalVal: model.Interval{Months: -1, Days: 29, Microseconds: 84600000000}}, {Type: model.TypeInterval, IntervalVal: model.Interval{Days: 1}}}})
	aggregateProjectionReleased(t, env.Query)
}
