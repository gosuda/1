package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONBCanonicalizationAndTextExtraction(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{"aa":1,"b":2,"a":3,"aa":4.00}'::jsonb::text,'{"a":1,"a": [ 2, 3 ]}'::json->'a','{"a":"x\ny"}'::json->>'a','[1,"last",null]'::json->>-2,'[1,null]'::json->>-1,jsonb_typeof('null'::jsonb),jsonb_array_length('[1,[2,3],{}]'::jsonb)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 114, 25, 25, 25, 25, 23}, [][]model.Value{{model.StringVal(`{"a": 3, "b": 2, "aa": 4.00}`), {Type: model.TypeJSONB, StrVal: `[ 2, 3 ]`}, model.StringVal("x\ny"), model.StringVal("last"), model.NullVal(model.TypeString), model.StringVal("null"), model.Int64Val(3)}})
	aggregateProjectionReleased(t, env.Query)
}
