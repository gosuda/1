package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLDistinctFromNullTruthTable(t *testing.T) {
	graph := sqlGraph(t, `SELECT a IS DISTINCT FROM b,a IS NOT DISTINCT FROM b
 FROM (VALUES(1,1,1),(2,1,2),(3,NULL,1),(4,1,NULL),(5,NULL,NULL)) t(k,a,b) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	f, tv := model.BoolVal(false), model.BoolVal(true)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{f, tv}, {tv, f}, {tv, f}, {tv, f}, {f, tv}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLDistinctFromUsesSQLNaNAndNumericEquality(t *testing.T) {
	graph := sqlGraph(t, `SELECT 'NaN'::float8 IS DISTINCT FROM 'NaN'::float8,
 '1.00'::numeric IS NOT DISTINCT FROM 1::integer,
 NULL::text IS NOT DISTINCT FROM NULL::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16}, [][]model.Value{{model.BoolVal(false), model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLDistinctFromEvaluatesEachOperandOnce(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT public.tick() IS DISTINCT FROM 0 FROM (VALUES(1),(2),(3)) t(k) ORDER BY k`, applyEnvironment(t))
	yes := model.BoolVal(true)
	assertLateralReplayRows(t, portal, tick, [][]model.Value{{yes}, {yes}, {yes}}, 3)
}

func TestSQLDistinctFromEvaluatesRightWithNullLeft(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT NULL::bigint IS DISTINCT FROM public.tick() FROM (VALUES(1),(2),(3)) t(k) ORDER BY k`, applyEnvironment(t))
	yes := model.BoolVal(true)
	assertLateralReplayRows(t, portal, tick, [][]model.Value{{yes}, {yes}, {yes}}, 3)
}
