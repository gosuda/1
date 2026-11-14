package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUnknownBuiltinArgumentsSelectAndCoerce(t *testing.T) {
	graph := sqlGraph(t, `SELECT sqrt('4'),ceil(NULL),length('abc'),power(NULL,3)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	null := model.NullVal(model.TypeFloat64)
	assertAnyValueRows(t, portal, []model.TypeID{701, 701, 23, 701}, [][]model.Value{{model.Float64Val(2), null, model.Int64Val(3), null}})
	aggregateProjectionReleased(t, env.Query)
}
