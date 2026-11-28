package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLTemporalOffsetRangeFrames(t *testing.T) {
	utc := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "UTC"}}
	newYork := utc
	newYork.TimeZone = builtin.TimeZoneContext{Name: "America/New_York"}
	for _, scenario := range []struct {
		name    string
		sql     string
		options builtin.ResolveOptions
		want    [][]model.Value
	}{
		{
			"date month clamp",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '1 month' PRECEDING AND CURRENT ROW) FROM (VALUES('2024-02-29'::date,1::int4),('2024-03-30'::date,2),('2024-03-31'::date,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(7)}},
		},
		{
			"timestamp month clamp",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '1 month' PRECEDING AND CURRENT ROW) FROM (VALUES('2024-01-31 12:00'::timestamp,1::int4),('2024-02-29 12:00'::timestamp,2),('2024-03-31 12:00'::timestamp,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(6)}},
		},
		{
			"timestamptz civil day across DST",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '1 day' PRECEDING AND CURRENT ROW) FROM (VALUES(make_timestamp(2024,3,9,11,30,0) AT TIME ZONE 'America/New_York',1::int4),(make_timestamp(2024,3,9,12,0,0) AT TIME ZONE 'America/New_York',2),(make_timestamp(2024,3,10,12,0,0) AT TIME ZONE 'America/New_York',4)) t(k,v)`,
			newYork,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(6)}},
		},
		{
			"time does not wrap",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '2 hours' PRECEDING AND CURRENT ROW) FROM (VALUES('00:30'::time,1::int4),('01:00'::time,2),('23:30'::time,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(4)}},
		},
		{
			"descending time flips in_range flags",
			`SELECT sum(v) OVER (ORDER BY k DESC RANGE BETWEEN interval '2 hours' PRECEDING AND CURRENT ROW) FROM (VALUES('00:30'::time,1::int4),('01:00'::time,2),('23:30'::time,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(4)}, {model.Int64Val(2)}, {model.Int64Val(3)}},
		},
		{
			"time following uses inclusive tail",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN CURRENT ROW AND interval '2 hours' FOLLOWING) FROM (VALUES('00:30'::time,1::int4),('01:00'::time,2),('23:30'::time,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(3)}, {model.Int64Val(2)}, {model.Int64Val(4)}},
		},
		{
			"timetz does not wrap",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '2 hours' PRECEDING AND CURRENT ROW) FROM (VALUES('00:30+00'::timetz,1::int4),('01:00+00'::timetz,2),('23:30+00'::timetz,4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(4)}},
		},
		{
			"null and temporal peers stay isolated",
			`SELECT sum(v) OVER (ORDER BY k NULLS FIRST RANGE BETWEEN interval '0' PRECEDING AND interval '0' FOLLOWING) FROM (VALUES(NULL::time,1::int4),(NULL::time,2),('01:00'::time,4),('01:00'::time,8)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(3)}, {model.Int64Val(3)}, {model.Int64Val(12)}, {model.Int64Val(12)}},
		},
		{
			"interval comparison order",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN interval '1 day' PRECEDING AND CURRENT ROW) FROM (VALUES(interval '29 days',1::int4),(interval '1 month',2),(interval '31 days',4)) t(k,v)`,
			utc,
			[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(6)}},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			prepared := prepareSemanticSQL(t, scenario.sql, nil, scenario.options)
			env := setWindowEnvironment(t)
			env.Builtin = scenario.options
			env.Limits.BatchRows = 1
			portal, err := prepared.Open(t.Context(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("temporal RANGE rows = %v, want %v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLTemporalRangeOffsetErrorsPrecedeArguments(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "UTC"}}
	for _, scenario := range []struct {
		name, offset, code string
	}{
		{"negative", `interval '-1 second'`, "22013"},
		{"null", `NULL::interval`, "22004"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			prepared := prepareSemanticSQL(t, `SELECT sum(1/x) OVER (ORDER BY k RANGE BETWEEN `+scenario.offset+` PRECEDING AND CURRENT ROW) FROM (VALUES('12:00'::time,0::int4)) t(k,x)`, nil, options)
			env := setWindowEnvironment(t)
			env.Builtin = options
			portal, err := prepared.Open(t.Context(), env)
			if portal != nil {
				closePortal(t, portal)
			}
			var diagnostic *model.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != scenario.code {
				t.Fatalf("offset error = %v, want SQLSTATE %s", err, scenario.code)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLTemporalRangeParameterSnapshot(t *testing.T) {
	interval := model.TypeLayout{Type: model.Type{ID: 1186, Modifier: -1, Storage: model.TypeInterval}}
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "UTC"}}
	prepared := prepareSemanticSQL(t, `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN $1::interval PRECEDING AND CURRENT ROW) FROM (VALUES('00:30'::time,1::int4),('01:00'::time,2),('03:00'::time,4)) t(k,v)`, []model.TypeLayout{interval}, options)
	env := setWindowEnvironment(t)
	env.Builtin = options
	env.Limits.BatchRows = 1
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: interval, Value: model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 2 * 60 * 60 * 1_000_000}}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	env.Parameters[0].Value.IntervalVal.Microseconds = 0
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(3)}, {model.Int64Val(6)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("temporal RANGE parameter snapshot = %v, want %v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}
