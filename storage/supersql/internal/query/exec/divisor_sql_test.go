package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntegerDivisorsPreserveZeroAndMaskOverflow(t *testing.T) {
	graph := sqlGraph(t, `SELECT k,lcm(a,b),CASE WHEN a=0 THEN NULL::bigint ELSE gcd(a,b) END
 FROM (VALUES(1,(-6)::bigint,4::bigint),(2,0::bigint,(-9223372036854775808)::bigint),(3,NULL::bigint,9::bigint)) t(k,a,b) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 20, 20}, [][]model.Value{{i(1), i(12), i(2)}, {i(2), i(0), n}, {i(3), n, n}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericDivisorsPreserveScaleAndSpecialValues(t *testing.T) {
	graph := sqlGraph(t, `SELECT gcd(a,b),lcm(a,b) FROM (VALUES
 (1,'1.200'::numeric,'0.18'::numeric),(2,'-0.00'::numeric,'3.1'::numeric),
 (3,'Infinity'::numeric,'0'::numeric)) t(k,a,b) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	n := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	assertAnyValueRows(t, portal, []model.TypeID{1700, 1700}, [][]model.Value{{n("0.060"), n("3.600")}, {n("3.10"), n("0.00")}, {n("NaN"), n("NaN")}})
	aggregateProjectionReleased(t, env.Query)
}
