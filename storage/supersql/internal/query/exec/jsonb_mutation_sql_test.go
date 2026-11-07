package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONBConcatReplacesNestedObjectsWithoutDeepMerge(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{"a":{"x":1},"keep":2}'::jsonb || '{"a":{"y":3},"b":4}'::jsonb, '{}'::jsonb || '[]'::jsonb, '1.00'::jsonb || '[]'::jsonb`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 3802, 3802}, [][]model.Value{{
		{Type: model.TypeJSONB, StrVal: `{"a": {"y": 3}, "b": 4, "keep": 2}`},
		{Type: model.TypeJSONB, StrVal: `[{}]`},
		{Type: model.TypeJSONB, StrVal: `[1.00]`},
	}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONBSubscriptsReadObjectAndArrayPaths(t *testing.T) {
	graph := sqlGraph(t, `SELECT ('{"a":[10,{"b":20}]}'::jsonb)['a'][1]['b'], ('[10,20]'::jsonb)[-1], ('{"a":null}'::jsonb)['a'], ('{"a":1}'::jsonb)['missing'], ('[1,2]'::jsonb)['0'::text], ('{"0":"v"}'::jsonb)[0], ('{"a":1}'::jsonb)[NULL::text]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 3802, 3802, 3802, 3802, 3802, 3802}, [][]model.Value{{
		{Type: model.TypeJSONB, StrVal: "20"},
		{Type: model.TypeJSONB, StrVal: "20"},
		{Type: model.TypeJSONB, StrVal: "null"},
		model.NullVal(model.TypeJSONB),
		{Type: model.TypeJSONB, StrVal: "1"},
		{Type: model.TypeJSONB, StrVal: `"v"`},
		model.NullVal(model.TypeJSONB),
	}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONBDeletionUsesRootSpecificSelectors(t *testing.T) {
	graph := sqlGraph(t, `SELECT '{"a":1,"b":2}'::jsonb - 'a', '["a",1,"a",null]'::jsonb - 'a', '[1,2,3]'::jsonb - (-1), '{"a":{"b":[10,20]}}'::jsonb #- ARRAY['a','b','-1'], '{"a":1,"b":2,"c":3}'::jsonb - ARRAY['a','c',NULL]::text[]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 3802, 3802, 3802, 3802}, [][]model.Value{{
		{Type: model.TypeJSONB, StrVal: `{"b": 2}`},
		{Type: model.TypeJSONB, StrVal: `[1, null]`},
		{Type: model.TypeJSONB, StrVal: `[1, 2]`},
		{Type: model.TypeJSONB, StrVal: `{"a": {"b": [10]}}`},
		{Type: model.TypeJSONB, StrVal: `{"b": 2}`},
	}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONBDeleteDistinguishesNullVariadicElementFromArray(t *testing.T) {
	graph := sqlGraph(t, `SELECT jsonb_delete('{"a":1,"b":2}'::jsonb,'a',NULL), jsonb_delete('{"a":1,"b":2}'::jsonb,VARIADIC NULL::text[])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 3802}, [][]model.Value{{
		{Type: model.TypeJSONB, StrVal: `{"b": 2}`},
		model.NullVal(model.TypeJSONB),
	}})
	aggregateProjectionReleased(t, env.Query)
}
