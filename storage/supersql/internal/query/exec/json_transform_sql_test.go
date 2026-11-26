package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONTransformationsPreserveDistinctRepresentations(t *testing.T) {
	graph := sqlGraph(t, `SELECT json_strip_nulls('{"a":1,"a":null,"b":[null,1]}'::json),jsonb_strip_nulls('{"a":1,"a":null,"b":[null,1]}'::jsonb),jsonb_strip_nulls('{"b":[null,1]}'::jsonb,true),jsonb_pretty('{"a":[]}'::jsonb)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{114, 3802, 3802, 25}, [][]model.Value{{{Type: model.TypeJSONB, StrVal: `{"a":1,"b":[null,1]}`}, {Type: model.TypeJSONB, StrVal: `{"b": [null, 1]}`}, {Type: model.TypeJSONB, StrVal: `{"b": [1]}`}, model.StringVal("{\n    \"a\": [\n    ]\n}")}})
	aggregateProjectionReleased(t, env.Query)
}
