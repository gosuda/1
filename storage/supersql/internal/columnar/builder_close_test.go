package columnar_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBuilderCloseDoesNotMutatePublishedNestedBuffers(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	array := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{array}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Close(); err != nil {
			t.Error(err)
		}
	})
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}
	if err := builder.Append(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	var batch columnar.Batch
	if err := builder.Publish(&batch); err != nil {
		t.Fatal(err)
	}
	if err := builder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := batch.Validate(1); err != nil {
		t.Fatalf("owner close invalidated published child headers: %v", err)
	}
	copied, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.ArrayVal) != 2 || copied.ArrayVal[0].IntVal != 7 || !copied.ArrayVal[1].IsNull {
		t.Fatalf("owner close changed published array: %+v", copied)
	}
}
