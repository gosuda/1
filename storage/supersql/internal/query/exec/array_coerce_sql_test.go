package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLArrayCoercePreservesShapeAndAppliesTypmods(t *testing.T) {
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	dims := []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}
	input := model.Value{Type: model.TypeArray, Dimensions: dims, ArrayVal: []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4)}}
	graph := sqlGraph(t, `SELECT $1::numeric(4,1)[],$1::text[]`, []model.TypeLayout{layout})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: input}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	numeric := model.Value{Type: model.TypeArray, Dimensions: dims, ArrayVal: []model.Value{{Type: model.TypeNumeric, StrVal: "1.0"}, model.NullVal(model.TypeNumeric), {Type: model.TypeNumeric, StrVal: "3.0"}, {Type: model.TypeNumeric, StrVal: "4.0"}}}
	text := model.Value{Type: model.TypeArray, Dimensions: dims, ArrayVal: []model.Value{model.StringVal("1"), model.NullVal(model.TypeString), model.StringVal("3"), model.StringVal("4")}}
	assertAnyValueRows(t, portal, []model.TypeID{1231, 1009}, [][]model.Value{{numeric, text}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayCoerceRunsCompleteChainPerElement(t *testing.T) {
	layout := arrayAggregateLayout(1009, model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}})
	graph := sqlGraph(t, `SELECT $1::numeric(2,0)[]`, []model.TypeLayout{layout})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: arrayAggregateValue(model.StringVal("100"), model.StringVal("bad"))}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err == nil {
		defer closePortal(t, portal)
		err = portal.Next(t.Context(), &columnar.Batch{})
	}
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22003" {
		t.Fatalf("array conversion first error: %v; want first-element numeric overflow", err)
	}
}

func TestSQLArrayCommonTypeDropsConflictingElementTypmods(t *testing.T) {
	graph := sqlGraph(t, `SELECT ARRAY[ARRAY[1]::integer[],ARRAY[2.3]::numeric(4,1)[]],ARRAY[ARRAY['ab']::varchar(2)[],ARRAY['cde']::varchar(3)[]]`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	dims := []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 1}}
	assertAnyValueRows(t, portal, []model.TypeID{1231, 1015}, [][]model.Value{{
		{Type: model.TypeArray, Dimensions: dims, ArrayVal: []model.Value{{Type: model.TypeNumeric, StrVal: "1"}, {Type: model.TypeNumeric, StrVal: "2.3"}}},
		{Type: model.TypeArray, Dimensions: dims, ArrayVal: []model.Value{model.StringVal("ab"), model.StringVal("cde")}},
	}})
	aggregateProjectionReleased(t, env.Query)
}
