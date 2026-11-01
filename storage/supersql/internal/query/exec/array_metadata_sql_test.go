package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLArrayMetadataUsesParameterDimensions(t *testing.T) {
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	i, s := model.Int64Val, model.StringVal
	nullInt, nullText := model.NullVal(model.TypeInt64), model.NullVal(model.TypeString)
	for _, tc := range []struct {
		name  string
		value model.Value
		want  []model.Value
	}{
		{"nondefault multidimensional bounds", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(1), nullInt, i(3), i(4)}, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}}, []model.Value{s("[-2:-1][4:5]"), i(2), i(-2), i(5), i(2), i(4), nullInt}},
		{"empty", model.Value{Type: model.TypeArray}, []model.Value{nullText, nullInt, nullInt, nullInt, nullInt, i(0), nullInt}},
		{"NULL", model.NullVal(model.TypeArray), []model.Value{nullText, nullInt, nullInt, nullInt, nullInt, nullInt, nullInt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT array_dims($1),array_ndims($1),array_lower($1,1),array_upper($1,2),array_length($1,2),cardinality($1),array_length($1,0)`, []model.TypeLayout{layout})
			env := statefulEnvironment(t)
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: tc.value}}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{25, 23, 23, 23, 23, 23, 23}, [][]model.Value{tc.want})
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLArrayMetadataAcceptsDifferentElementTypes(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_dims(ARRAY[s,NULL]),cardinality(ARRAY[s,NULL]),array_length(ARRAY[1,NULL,3],1)
 FROM (VALUES(1,'a'::text),(2,NULL::text)) t(k,s) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	row := []model.Value{model.StringVal("[1:2]"), model.Int64Val(2), model.Int64Val(3)}
	assertAnyValueRows(t, portal, []model.TypeID{25, 23, 23}, [][]model.Value{row, row})
	aggregateProjectionReleased(t, env.Query)
}
