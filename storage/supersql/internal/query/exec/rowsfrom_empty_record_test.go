package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLRowsFromZeroFieldInputContributesMultiplicity(t *testing.T) {
	record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}}
	array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}
	specs := []model.DescriptorSpec{
		{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}},
		{ID: 30, OID: 90000, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "empty_record"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: record, Array: 31}},
	}
	snapshot := make(lateralReplayCatalog, len(specs))
	for i, spec := range specs {
		var err error
		snapshot[i], err = model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	graph := sqlGraphWithCatalog(t, `SELECT * FROM ROWS FROM(unnest($1),generate_series(7,7)) WITH ORDINALITY AS t(x,n)`, snapshot, []model.TypeLayout{array})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: array, Value: model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: -2}}, ArrayVal: []model.Value{{Type: model.TypeComposite}, model.NullVal(model.TypeComposite), {Type: model.TypeComposite}}}}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, n := model.Int64Val, model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 20}, [][]model.Value{{i(7), i(1)}, {n, i(2)}, {n, i(3)}})
	aggregateProjectionReleased(t, env.Query)
}
