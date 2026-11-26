package builtin

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type failingNativeTypeInput struct{}

func (failingNativeTypeInput) ValidateTypeInput(context.Context, string, string, int64, uint64) (*model.Error, error) {
	return &model.Error{Code: "22000", Message: "invalid input"}, nil
}

func TestNativeTypeInputErrorInfoUsesNullOptionalFields(t *testing.T) {
	call := compileNativeTypeInput(6211)
	result, err := call(t.Context(), []model.Value{model.StringVal("bad"), model.StringVal("integer")}, CallBudget{
		OutputBytes: 1024,
		MemoryBytes: 1024,
		Native:      model.NativeScalarRuntime{TypeInput: failingNativeTypeInput{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != model.TypeComposite || result.IsNull || len(result.ArrayVal) != 4 {
		t.Fatalf("error info = %v", result)
	}
	if result.ArrayVal[0].StrVal != "invalid input" || !result.ArrayVal[1].IsNull || !result.ArrayVal[2].IsNull || result.ArrayVal[3].StrVal != "22000" {
		t.Fatalf("error fields = %v", result.ArrayVal)
	}
}
