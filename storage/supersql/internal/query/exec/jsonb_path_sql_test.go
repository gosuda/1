package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONBPathsUseCanonicalPrefixAndScalarRules(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{"a":{"b":1},"a":{}}'::jsonb #>> ARRAY['a','b'],jsonb_extract_path_text('{"a":[1],"a":[2,3]}'::jsonb,'a','-1'),'42'::jsonb #> ARRAY['0'],'{"a":null}'::jsonb ? 'a','"a"'::jsonb ? 'a','[1,null,"a"]'::jsonb ?| ARRAY['1','a'],'{"a":1}'::jsonb ?& ARRAY['a',NULL]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 3802, 16, 16, 16, 16}, [][]model.Value{{model.NullVal(model.TypeString), model.StringVal("3"), model.NullVal(model.TypeJSONB), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
