package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLTimestampBinsBeforeOrigin(t *testing.T) {
	graph := sqlGraph(t, `SELECT date_bin(interval '10 seconds',make_timestamp(1999,12,31,23,59,59),make_timestamp(2000,1,1,0,0,0))::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("1999-12-31 23:59:50")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampTZBinUsesInstantParameters(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT date_bin(interval '10 seconds',$1,$2)`, []model.TypeLayout{layout, layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: -1}}, {Number: 2, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: 0}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{1184}, [][]model.Value{{{Type: model.TypeTimestamp, IntVal: -10000000}}})
	aggregateProjectionReleased(t, env.Query)
}
