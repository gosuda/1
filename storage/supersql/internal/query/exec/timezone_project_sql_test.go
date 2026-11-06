package exec_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLTimezoneDateProjectionPreservesOutOfInputRangeDay(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "<-30>30"}}
	prepared := prepareSemanticSQL(t, `SELECT d::text,extract(year FROM d),extract(dow FROM d),extract(isodow FROM d),extract(doy FROM d),extract(week FROM d),extract(isoyear FROM d),count(*) FROM (SELECT t::date AS d FROM (VALUES($1),($1)) v(t)) q GROUP BY d`, []model.TypeLayout{layout}, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: -211813488000000000}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 1700, 1700, 1700, 1700, 1700, 1700, 20}, [][]model.Value{{model.StringVal("4714-11-22 BC"), {Type: model.TypeNumeric, StrVal: "-4714"}, {Type: model.TypeNumeric, StrVal: "6"}, {Type: model.TypeNumeric, StrVal: "6"}, {Type: model.TypeNumeric, StrVal: "326"}, {Type: model.TypeNumeric, StrVal: "47"}, {Type: model.TypeNumeric, StrVal: "-4714"}, model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimezoneDateTimeInfinityAndLocalClock(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	prepared := prepareSemanticSQL(t, `SELECT t::date::text,t::time::text FROM (VALUES($1),($2),($3)) v(t)`, []model.TypeLayout{layout, layout, layout}, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	for i, v := range []int64{0, math.MinInt64, math.MaxInt64} {
		env.Parameters = append(env.Parameters, expr.ParameterValue{Number: uint32(i + 1), Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: v}})
	}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{model.StringVal("1999-12-31"), model.StringVal("19:00:00")}, {model.StringVal("-infinity"), model.NullVal(model.TypeString)}, {model.StringVal("infinity"), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}
