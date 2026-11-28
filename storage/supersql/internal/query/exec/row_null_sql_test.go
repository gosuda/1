package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRowNullPredicatesAreNotComplements(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(a,b) IS NULL,ROW(a,b) IS NOT NULL
 FROM (VALUES(1,NULL::integer,NULL::integer),(2,1,NULL),(3,NULL,2),(4,1,2)) t(k,a,b) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes, no := model.BoolVal(true), model.BoolVal(false)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{yes, no}, {no, no}, {no, no}, {no, yes}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowNullPredicatesDoNotRecurseIntoFields(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(ROW(NULL::integer)) IS NULL,ROW(ROW(NULL::integer)) IS NOT NULL,
 ROW() IS NULL,ROW() IS NOT NULL,NULL::record IS NULL,NULL::record IS NOT NULL`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes, no := model.BoolVal(true), model.BoolVal(false)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16, 16}, [][]model.Value{{no, yes, yes, yes, yes, no}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowNullPredicateEvaluatesRowOnce(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT ROW(NULL::bigint,public.tick()) IS NULL FROM (VALUES(1),(2),(3)) t(k) ORDER BY k`, applyEnvironment(t))
	no := model.BoolVal(false)
	assertLateralReplayRows(t, portal, tick, [][]model.Value{{no}, {no}, {no}}, 3)
}
