package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayFillPreservesDeclaredBoundsAndNullElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_fill('x'::text,ARRAY[2,2])::text,
 array_fill(NULL::integer,ARRAY[2,1],ARRAY[-2,4])::text,
 array_fill(7,'[-1:0]={2,3}'::integer[],'[4:5]={-2,7}'::integer[])::text,
 array_fill(9,ARRAY[0,2])::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{s("{{x,x},{x,x}}"), s("[-2:-1][4:4]={{NULL},{NULL}}"), s("[-2:-1][7:9]={{7,7,7},{7,7,7}}"), s("{}")}})
	aggregateProjectionReleased(t, env.Query)
}
