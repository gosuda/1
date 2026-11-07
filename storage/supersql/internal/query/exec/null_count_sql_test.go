package exec_test

import (
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLNullCountersInspectDatumNullnessNotRecordFields(t *testing.T) {
	graph := sqlGraph(t, `SELECT num_nulls(ROW(NULL,NULL),NULL),num_nonnulls(ROW(NULL,NULL),NULL),num_nulls(VARIADIC NULL::integer[]),num_nonnulls(VARIADIC ARRAY[]::integer[]),num_nulls(VARIADIC ARRAY[[1,NULL],[NULL,2]])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 23, 23}, [][]model.Value{{i(1), i(1), model.NullVal(model.TypeInt64), i(0), i(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNullCountersDoNotRequireJSONBOutputKernel(t *testing.T) {
	json := model.TypeLayout{Type: model.Type{ID: 3802, Storage: model.TypeJSONB, Modifier: -1}}
	graph := sqlGraph(t, `SELECT num_nulls($1,NULL::jsonb),num_nonnulls($1,NULL::jsonb)`, []model.TypeLayout{json})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: json, Value: model.Value{Type: model.TypeJSONB, StrVal: "{}"}}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{{model.Int64Val(1), model.Int64Val(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNullCountersSupportWideExpandedCalls(t *testing.T) {
	graph := sqlGraph(t, "SELECT num_nonnulls("+strings.Repeat("1,", 99)+"1)", nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(100)}})
	aggregateProjectionReleased(t, env.Query)
}
