package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLByteAccessUsesLittleBitNumbering(t *testing.T) {
	graph := sqlGraph(t, `SELECT get_byte(x,1),get_bit(x,0),get_bit(x,7),bit_count(x),
 set_byte(x,1,-1),set_bit(x,0,0),set_bit(x,9,1)
 FROM (VALUES('\x8100ff'::bytea)) t(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	b, i := model.BytesVal, model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 20, 17, 17, 17}, [][]model.Value{{
		i(0), i(1), i(1), i(10), b([]byte{129, 255, 255}), b([]byte{128, 0, 255}), b([]byte{129, 2, 255}),
	}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLByteAccessMasksOutOfRangeIndex(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN n<0 THEN 0 ELSE get_byte(x,n) END,bit_count(x)
 FROM (VALUES(1,'\x00ff'::bytea,-1),(2,'\x00ff'::bytea,1),(3,NULL::bytea,1)) t(k,x,n) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 20}, [][]model.Value{
		{i(0), i(8)}, {i(255), i(8)}, {model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)},
	})
	aggregateProjectionReleased(t, env.Query)
}
