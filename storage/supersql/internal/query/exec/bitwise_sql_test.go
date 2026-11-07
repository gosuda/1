package exec_test

import (
	"math"
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntegerBitwiseWidthsAndNulls(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,1::int2<<n,1::int4<<n,1::int8<<n,(-1)::int8>>n,~n
 FROM (VALUES(15),(31),(32),(63),(64),(NULL)) t(n) ORDER BY n`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	n := model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 21, 23, 20, 20, 23}, [][]model.Value{
		{i(15), i(math.MinInt16), i(32768), i(32768), i(-1), i(-16)},
		{i(31), i(0), i(math.MinInt32), i(2147483648), i(-1), i(-32)},
		{i(32), i(1), i(1), i(4294967296), i(-1), i(-33)},
		{i(63), i(0), i(math.MinInt32), i(math.MinInt64), i(-1), i(-64)},
		{i(64), i(1), i(1), i(1), i(-1), i(-65)},
		{n, n, n, n, n, n},
	})
	aggregateProjectionReleased(t, env.Query)
}
