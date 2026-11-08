package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONBLookupPreservesScalarAndNullRules(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{"a":1,"a": {"bb":2,"a":1.00}}'::jsonb->'a','[0,"last"]'::jsonb->>-1,'42.00'::jsonb->0,'null'::jsonb->>-1,'{"a":1}'::jsonb->0`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 25, 3802, 25, 3802}, [][]model.Value{{{Type: model.TypeJSONB, StrVal: `{"a": 1.00, "bb": 2}`}, model.StringVal("last"), {Type: model.TypeJSONB, StrVal: "42.00"}, model.NullVal(model.TypeString), model.NullVal(model.TypeJSONB)}})
	aggregateProjectionReleased(t, env.Query)
}
