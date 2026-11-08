package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSubscriptConstantProofDoesNotCrossOuterJoinNullExtension(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ARRAY[10,20])[r.n] FROM (VALUES(1))l(k) LEFT JOIN (VALUES(2))r(n) ON false`, nil)
	env := configureSetWindowEnvironment(statefulEnvironment(t))
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.NullVal(model.TypeInt64)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSubscriptConstantProofPreservesMultipleValueRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ARRAY[10,20])[n] FROM (VALUES(1),(2))t(n) ORDER BY n`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(10)}, {model.Int64Val(20)}})
	aggregateProjectionReleased(t, env.Query)
}
