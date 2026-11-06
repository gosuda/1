package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLDateConstructorAndArithmetic(t *testing.T) {
	graph := sqlGraph(t, `SELECT make_date(2000,3,1)-make_date(2000,2,28),make_date(2000,2,28)+2=make_date(2000,3,1),2+make_date(2000,2,28)=make_date(2000,3,1),make_date(2000,3,1)-2=make_date(2000,2,28),encode(date_send(make_date(2000,1,1)),'hex'),isfinite(make_date(-1,2,29)),ARRAY[make_date(2000,1,1)]<ARRAY[make_date(2000,1,2)]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 16, 16, 16, 25, 16, 16}, [][]model.Value{{model.Int64Val(2), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true), model.StringVal("00000000"), model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
