package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONTextCodecsPreserveNullAndNumericLexemes(t *testing.T) {
	graph := sqlGraph(t, `SELECT (' {"a":1e999999,"a":"\u0000"} '::json)::text,json_typeof('null'::json),json_array_length('[1,[2,3],{"x":4},null]'::json),encode(json_send(' [1, 2] '::json),'hex')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 23, 25}, [][]model.Value{{model.StringVal(` {"a":1e999999,"a":"\u0000"} `), model.StringVal("null"), model.Int64Val(4), model.StringVal("205b312c20325d20")}})
	aggregateProjectionReleased(t, env.Query)
}
