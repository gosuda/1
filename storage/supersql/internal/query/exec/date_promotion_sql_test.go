package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLDatePromotionPreservesEpochAndTimeAddition(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT date_part('epoch',$1),$1::timestamp,($1+make_time(24,0,0))::text`, []model.TypeLayout{layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: -2451546}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{701, 1114, 25}, [][]model.Value{{model.Float64Val(-210866889600), {Type: model.TypeTimestamp, IntVal: -211813574400000000}, model.StringVal("4714-11-24 00:00:00 BC")}})
	aggregateProjectionReleased(t, env.Query)
}
