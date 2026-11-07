package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLNullIfPromotesFirstOperandAndMasksOuterBranch(t *testing.T) {
	graph := sqlGraph(t, `SELECT x,NULLIF(x,2.0),CASE WHEN x=0 THEN NULL::integer ELSE NULLIF(10/x,5) END
 FROM (VALUES(0),(1),(2),(NULL)) t(x) ORDER BY x`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	numericNull := model.NullVal(model.TypeNumeric)
	numeric := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	assertAnyValueRows(t, portal, []model.TypeID{23, 1700, 23}, [][]model.Value{
		{i(0), numeric("0"), null}, {i(1), numeric("1"), i(10)}, {i(2), numericNull, null}, {null, numericNull, null},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNullIfEvaluatesPromotedFirstOperandOnce(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT NULLIF(public.tick(),0) FROM (VALUES(1),(2),(3)) t(x) ORDER BY x`, applyEnvironment(t))
	i := model.Int64Val
	assertLateralReplayRows(t, portal, tick, [][]model.Value{{i(1)}, {i(2)}, {i(3)}}, 3)
}

func TestSQLNullIfEvaluatesSecondOperandWithNullFirst(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT NULLIF(NULL::bigint,public.tick()) FROM (VALUES(1),(2),(3)) t(x) ORDER BY x`, applyEnvironment(t))
	null := model.NullVal(model.TypeInt64)
	assertLateralReplayRows(t, portal, tick, [][]model.Value{{null}, {null}, {null}}, 3)
}
