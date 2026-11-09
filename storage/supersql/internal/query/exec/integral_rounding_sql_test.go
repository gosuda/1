package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLDirectedIntegralRoundingPreservesWideNumeric(t *testing.T) {
	graph := sqlGraph(t, `SELECT ceil(n),ceiling(n),floor(n),ceil(f),floor(f)
		FROM (VALUES('9007199254740993.001'::numeric,(-0.1)::float8),
		('-9007199254740993.001'::numeric,1.1::float8)) t(n,f) ORDER BY n DESC`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	numeric := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	f := model.Float64Val
	assertAnyValueRows(t, portal, []model.TypeID{1700, 1700, 1700, 701, 701}, [][]model.Value{
		{numeric("9007199254740994"), numeric("9007199254740994"), numeric("9007199254740993"), f(0), f(-1)},
		{numeric("-9007199254740993"), numeric("-9007199254740993"), numeric("-9007199254740994"), f(2), f(1)},
	})
	aggregateProjectionReleased(t, env.Query)
}
