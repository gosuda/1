package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLNamedTimezoneCivilTransitions(t *testing.T) {
	graph := sqlGraph(t, `SELECT
 (make_timestamp(2024,3,10,2,30,0) AT TIME ZONE 'America/New_York' AT TIME ZONE 'UTC')::text,
 (make_timestamp(2024,11,3,1,30,0) AT TIME ZONE 'America/New_York' AT TIME ZONE 'UTC')::text,
 (make_timestamp(2011,12,30,12,0,0) AT TIME ZONE 'Pacific/Apia' AT TIME ZONE 'UTC')::text,
 (make_timestamp(1800,1,1,12,0,0) AT TIME ZONE 'Europe/Paris' AT TIME ZONE 'UTC')::text,
 (make_timestamp(10000,7,1,12,0,0) AT TIME ZONE 'America/New_York' AT TIME ZONE 'UTC')::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25}, [][]model.Value{{model.StringVal("2024-03-10 07:30:00"), model.StringVal("2024-11-03 06:30:00"), model.StringVal("2011-12-30 22:00:00"), model.StringVal("1800-01-01 11:50:39"), model.StringVal("10000-07-01 16:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNamedTimezoneVariesByInputLane(t *testing.T) {
	graph := sqlGraph(t, `SELECT (to_timestamp(946684800) AT TIME ZONE z)::text FROM (VALUES('America/New_York'),('aSia/Kathmandu'),('UTC')) v(z)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("1999-12-31 19:00:00")}, {model.StringVal("2000-01-01 05:45:00")}, {model.StringVal("2000-01-01 00:00:00")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestPreparedNamedTimezoneRejectsChangedSessionContext(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT make_timestamp(2000,1,1,0,0,0) AT TIME ZONE 'LMT'`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1184}, [][]model.Value{{{Type: model.TypeTimestamp, IntVal: 17762000000}}})
	aggregateProjectionReleased(t, env.Query)
	changed := statefulEnvironment(t)
	changed.Builtin = options
	changed.Builtin.TimeZone = builtin.TimeZoneContext{Name: "UTC"}
	rejected, err := prepared.Open(t.Context(), changed)
	if rejected != nil {
		closePortal(t, rejected)
	}
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "0A000" {
		t.Fatalf("changed prepared timezone context: %v", err)
	}
	aggregateProjectionReleased(t, changed.Query)
}
