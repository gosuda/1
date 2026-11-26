package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLOverlayUsesCharacterPositions(t *testing.T) {
	graph := sqlGraph(t, `SELECT overlay(s PLACING 'XY' FROM 2),overlay(s PLACING 'Q' FROM 2 FOR n)
 FROM (VALUES(1,'a界🙂z'::text,1),(2,'a界🙂z'::text,-1),(3,NULL::text,1)) t(k,s,n) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{
		{s("aXYz"), s("aQ🙂z")}, {s("aXYz"), s("aQa界🙂z")},
		{model.NullVal(model.TypeString), model.NullVal(model.TypeString)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLOverlayByteaAndMaskedStartError(t *testing.T) {
	graph := sqlGraph(t, `SELECT overlay(x PLACING '\xff00'::bytea FROM 2),
 overlay(x PLACING '\xff'::bytea FROM 2 FOR -1),
 CASE WHEN n=0 THEN x ELSE overlay(x PLACING '\xff'::bytea FROM n FOR 1) END
 FROM (VALUES(1,'\x00010203'::bytea,0),(2,'\x00010203'::bytea,4)) t(k,x,n) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	b := model.BytesVal
	assertAnyValueRows(t, portal, []model.TypeID{17, 17, 17}, [][]model.Value{
		{b([]byte{0, 255, 0, 3}), b([]byte{0, 255, 0, 1, 2, 3}), b([]byte{0, 1, 2, 3})},
		{b([]byte{0, 255, 0, 3}), b([]byte{0, 255, 0, 1, 2, 3}), b([]byte{0, 1, 2, 255})},
	})
	aggregateProjectionReleased(t, env.Query)
}
