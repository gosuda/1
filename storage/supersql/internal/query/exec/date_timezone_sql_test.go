package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLDateTimezonePromotionResolvesSkippedDate(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "Pacific/Apia"}}
	prepared := prepareSemanticSQL(t, `SELECT make_date(2011,12,30)::timestamptz::text`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("2011-12-31 00:00:00+14")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLDateTimezonePromotionCanRecoverProjectedDay(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "<-30>30"}}
	layout := model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT ($1::timestamptz AT TIME ZONE interval '0')::text`, []model.TypeLayout{layout}, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: -2451546}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("4714-11-24 06:00:00 BC")}})
	aggregateProjectionReleased(t, env.Query)
}
