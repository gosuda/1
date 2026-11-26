package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONPathScalarOperatorsAndExactType(t *testing.T) {
	graph := sqlGraph(t, `SELECT
		'{"a":[1,2,3]}'::jsonb @? '$.a[*]?(@ > 1)',
		'{"a":[1,2,3]}'::jsonb @@ 'exists($.a[2])',
		jsonb_path_query_array('{"a":[1,2,3]}'::jsonb, '$.a[1 to last]'),
		ARRAY['$.a'::jsonpath, 'strict $[0]'::jsonpath]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil { t.Fatal(err) }
	defer closePortal(t, portal)
	pathArray := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{
		{Type: model.TypeBytes, BytesVal: append([]byte{1}, `$."a"`...)},
		{Type: model.TypeBytes, BytesVal: append([]byte{1}, `strict $[0]`...)},
	}}
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 3802, 4073}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true), {Type: model.TypeJSONB, StrVal: `[2, 3]`}, pathArray}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONPathQueryStreamsRowsWithOrdinality(t *testing.T) {
	graph := sqlGraph(t, `SELECT value, ordinality FROM jsonb_path_query('[1,2,3]'::jsonb, '$[*]?(@ >= 2)') WITH ORDINALITY AS q(value, ordinality)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil { t.Fatal(err) }
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 20}, [][]model.Value{
		{{Type: model.TypeJSONB, StrVal: `2`}, model.Int64Val(1)},
		{{Type: model.TypeJSONB, StrVal: `3`}, model.Int64Val(2)},
	})
	aggregateProjectionReleased(t, env.Query)
}
