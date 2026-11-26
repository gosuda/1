package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLIntervalFieldQualifiersAndPrecision(t *testing.T) {
	graph := sqlGraph(t, `SELECT (interval '1 year 2 mons 3 days 04:05:06.123456' YEAR)::text,(interval '1 year 2 mons 3 days 04:05:06.123456' MONTH)::text,(interval '1 year 2 mons 3 days 04:05:06.123456' DAY TO MINUTE)::text,(interval '-1.2345' SECOND(3))::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	text := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{text("1 year"), text("1 year 2 mons"), text("1 year 2 mons 3 days 04:05:00"), text("-00:00:01.235")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLIntervalParametersUseArithmeticAndTypmods(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1186, Storage: model.TypeInterval, Modifier: -1}}
	graph := sqlGraph(t, `SELECT ($1::interval day to minute)::text,justify_interval($1)::text,$1 < interval '2 months',array_position(ARRAY[interval '1 month'],interval '30 days'),(ROW($1,1)<ROW(interval '2 months',2))`, []model.TypeLayout{layout})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Days: 30, Microseconds: 90_061_234_567}}}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 16, 23, 16}, [][]model.Value{{model.StringVal("30 days 25:01:00"), model.StringVal("1 mon 1 day 01:01:01.234567"), model.BoolVal(true), model.Int64Val(1), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestPreparedIntervalStyleAgreesForLiteralsAndParameters(t *testing.T) {
	settings := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, IntervalStyle: builtin.IntervalSQLStandard}
	layout := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT (interval '-1 2:03:04')::text,($1::interval)::text`, []model.TypeLayout{layout}, settings)
	env := statefulEnvironment(t)
	env.Builtin = settings
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.StringVal("-1 2:03:04")}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	text := model.StringVal("-1 2:03:04")
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{text, text}})
	aggregateProjectionReleased(t, env.Query)
	changed := statefulEnvironment(t)
	changed.Builtin = settings
	changed.Builtin.IntervalStyle = builtin.IntervalPostgres
	_, err = prepared.Open(t.Context(), changed)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "0A000" {
		t.Fatalf("changed interval style error=%v, want 0A000", err)
	}
	aggregateProjectionReleased(t, changed.Query)
}

func TestPreparedIntervalPrecisionReductionRemainsObservable(t *testing.T) {
	settings := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}
	prepared := prepareSemanticSQL(t, `SELECT interval(8) '1.23456789'::text`, nil, settings)
	warnings := prepared.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("precision warnings=%v, want one", warnings)
	}
	if warnings[0].Code != "22023" {
		t.Fatalf("precision warning=%v, want 22023", warnings[0])
	}
	env := statefulEnvironment(t)
	env.Builtin = settings
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("00:00:01.234568")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLIntervalFieldExtractionAndBinaryOutput(t *testing.T) {
	graph := sqlGraph(t, `SELECT extract(epoch FROM interval '1 year -0.000001 seconds'),date_part('hour',interval '25 hours'),date_trunc('quarter',interval '-14 months 3 days')::text,encode(interval_send(interval '1 mon -2 days -0.000001 seconds'),'hex')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1700, 701, 25, 25}, [][]model.Value{{{Type: model.TypeNumeric, StrVal: "31557599.999999"}, model.Float64Val(25), model.StringVal("-1 years"), model.StringVal("fffffffffffffffffffffffe00000001")}})
	aggregateProjectionReleased(t, env.Query)
}
