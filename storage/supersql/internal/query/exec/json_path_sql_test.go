package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONPathUsesTypedVariadicArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT json_extract_path_text('{"a":["first","last"]}'::json,'a','-1'),'{"a":{"b":1},"a":{}}'::json #>> ARRAY['a','b'],' [ 1, 2 ] '::json #> ARRAY[]::text[],json_extract_path_text('"x"'::json,VARIADIC ARRAY[]::text[])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 114, 25}, [][]model.Value{{model.StringVal("last"), model.StringVal("1"), {Type: model.TypeJSONB, StrVal: "[ 1, 2 ]"}, model.StringVal("x")}})
	aggregateProjectionReleased(t, env.Query)
}
