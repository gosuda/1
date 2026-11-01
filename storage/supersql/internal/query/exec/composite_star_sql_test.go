package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLCompositeStarExpandsTypedTargetColumns(t *testing.T) {
	graph := sqlGraph(t, `SELECT 9 AS before,(ROW(1,'x'::text)).* AS ignored,8 AS after`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25, 23}, [][]model.Value{{model.Int64Val(9), model.Int64Val(1), model.StringVal("x"), model.Int64Val(8)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowConstructorExpandsRelationAndCompositeStars(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ROW(t.*,(ROW(3,4)).*)).f3,(ROW(t.*,(ROW(3,4)).*)).f4 FROM (VALUES(1,2))t(a,b)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{{model.Int64Val(3), model.Int64Val(4)}})
	aggregateProjectionReleased(t, env.Query)
}
