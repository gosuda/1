package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayConstructionPreservesBoundsAndNullElements(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_append(a,n)::text,array_prepend(n,a)::text,array_cat(a,ARRAY[n])::text
 FROM (VALUES('[-2:-1]={1,2}'::integer[],3),(NULL::integer[],NULL::integer),(ARRAY[]::integer[],4))t(a,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{
		{s("[-2:0]={1,2,3}"), s("[-2:0]={3,1,2}"), s("[-2:0]={1,2,3}")},
		{s("{NULL}"), s("{NULL}"), s("{NULL}")},
		{s("{4}"), s("{4}"), s("{4}")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayConcatenationPromotesElementTypes(t *testing.T) {
	for _, tc := range []struct{ name, expression, want string }{
		{"append", `ARRAY[1::smallint]||2::bigint`, "{1,2}"},
		{"prepend", `0::numeric||ARRAY[1::integer]`, "{0,1}"},
		{"concatenate", `ARRAY[1]||ARRAY[2.5]`, "{1,2.5}"},
		{"erase conflicting typmods", `array_append(ARRAY['x'::varchar(2)],'long'::varchar)`, "{x,long}"},
		{"collated element", `array_append(ARRAY['a' COLLATE "C"],'b')`, "{a,b}"},
		{"unknown right array", `ARRAY[1]||'{2,3}'`, "{1,2,3}"},
		{"unknown null array", `ARRAY[1]||NULL`, "{1}"},
		{"unknown left array", `'{0}'||ARRAY[1]`, "{0,1}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT ("+tc.expression+")::text", nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal(tc.want)}})
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLArrayCompatibleTypesPreserveBidirectionalCandidate(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_append(ARRAY['a'::text],'b'::varchar),array_append(ARRAY['a'::varchar],'b'::text)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{model.StringVal("a"), model.StringVal("b")}}
	assertAnyValueRows(t, portal, []model.TypeID{1009, 1015}, [][]model.Value{{value, value}})
	aggregateProjectionReleased(t, env.Query)
}
