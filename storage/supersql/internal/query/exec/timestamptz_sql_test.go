package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLTimestampTZInstantGroupingAndBinaryOutput(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT encode(timestamptz_send(t),'hex'),count(*) FROM (VALUES($1),($2),($1)) v(t) GROUP BY t ORDER BY t`, []model.TypeLayout{layout, layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := setWindowEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: -1}}, {Number: 2, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: 1}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("ffffffffffffffff"), model.Int64Val(2)}, {model.StringVal("0000000000000001"), model.Int64Val(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampTZInstantExtremaAndElapsedDuration(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT min(t),max(t),max(t)-min(t),isfinite(min(t)),bool_and(t <= $2) FROM (VALUES($1),($2)) v(t)`, []model.TypeLayout{layout, layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := setWindowEnvironment(t)
	left, right := model.Value{Type: model.TypeTimestamp, IntVal: -1}, model.Value{Type: model.TypeTimestamp, IntVal: 1}
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: left}, {Number: 2, Layout: layout, Value: right}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1184, 1184, 1186, 16, 16}, [][]model.Value{{left, right, {Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 2}}, model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
