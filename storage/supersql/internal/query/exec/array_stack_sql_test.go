package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLArrayStackConstructorsAndElementCoercion(t *testing.T) {
	graph := sqlGraph(t, `SELECT ARRAY[[n,2],[3,4]],ARRAY[ARRAY[n,2],ARRAY[3,4]::bigint[]],
 ARRAY[[1.6,2.4],[3.5,4.5]]::integer[] FROM (VALUES(1),(5)) t(n) ORDER BY n`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	matrix := func(values ...int64) model.Value {
		out := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 2}}}
		for _, value := range values {
			out.ArrayVal = append(out.ArrayVal, model.Int64Val(value))
		}
		return out
	}
	assertAnyValueRows(t, portal, []model.TypeID{1007, 1016, 1007}, [][]model.Value{
		{matrix(1, 2, 3, 4), matrix(1, 2, 3, 4), matrix(2, 2, 4, 5)},
		{matrix(5, 2, 3, 4), matrix(5, 2, 3, 4), matrix(2, 2, 4, 5)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayStackPreservesInnerLowerBounds(t *testing.T) {
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	first := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64)}, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}}}
	second := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(4)}, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}}}
	graph := sqlGraph(t, `SELECT array_dims(ARRAY[$1,$2]),cardinality(ARRAY[$1,$2]),ARRAY[$1,$2]`, []model.TypeLayout{layout, layout})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: first}, {Number: 2, Layout: layout, Value: second}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: -2, Length: 2}}}
	assertAnyValueRows(t, portal, []model.TypeID{25, 23, 1007}, [][]model.Value{{model.StringVal("[1:2][-2:-1]"), model.Int64Val(4), want}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayStackEmptyNullAndShapeErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT cardinality(ARRAY[NULL::integer[],ARRAY[]::integer[]]),array_ndims(ARRAY[NULL::integer[],NULL::integer[]]),
 CASE WHEN n=0 THEN ARRAY[]::integer[] ELSE ARRAY[ARRAY[1],ARRAY[2,3]] END
 FROM (VALUES(0)) t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	var batch columnar.Batch
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || batch.Columns[0].Int64[0] != 0 || !batch.Columns[1].IsNull(0) || batch.Columns[2].IsNull(0) || batch.Columns[2].Ranges[0].Length != 0 {
		t.Fatalf("empty/null constructors: %+v", batch)
	}
	closePortal(t, portal)
	aggregateProjectionReleased(t, env.Query)
	for _, sql := range []string{`SELECT ARRAY[ARRAY[1],ARRAY[2,3]]`, `SELECT ARRAY[ARRAY[1],NULL::integer[]]`, `SELECT ARRAY[ARRAY[]::integer[],ARRAY[1]]`} {
		t.Run(sql, func(t *testing.T) {
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), sqlGraph(t, sql, nil), env)
			if err == nil {
				defer closePortal(t, portal)
				err = portal.Next(t.Context(), &columnar.Batch{})
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "2202E" {
				t.Fatalf("shape error: %v; want 2202E", err)
			}
		})
	}
}
