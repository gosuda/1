package columnar_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCopyValueDoesNotAddressNullDictionaryPayload(t *testing.T) {
	vector := columnar.Vector{Type: model.Type{ID: 17, Storage: model.TypeBytes}, Encoding: columnar.Dictionary, Length: 2, Nulls: []byte{1}, Indices: []uint32{^uint32(0), 0}, Ranges: []columnar.Range{{Length: 3}}, Data: []byte("abc")}
	null, _, err := columnar.CopyValue(t.Context(), &vector, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !null.IsNull {
		t.Fatal("NULL dictionary lane became a value")
	}
	value, used, err := columnar.CopyValue(t.Context(), &vector, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(cap(value.BytesVal)) > used {
		t.Fatalf("retained %d bytes against charge %d", cap(value.BytesVal), used)
	}
	clear(vector.Data)
	if string(value.BytesVal) != "abc" {
		t.Fatal("point value retained borrowed dictionary bytes")
	}
}

func TestCopyValueOwnsNestedPayloadAndMaximumArrayBound(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1001, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Close(); err != nil {
			t.Error(err)
		}
	})
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.BytesVal([]byte("payload")), model.NullVal(model.TypeBytes)}}
	if err := builder.Append(t.Context(), []model.Value{input}); err != nil {
		t.Fatal(err)
	}
	input.Dimensions[0].LowerBound = 1
	input.ArrayVal[0].BytesVal[0] = 'X'
	input.ArrayVal[1] = model.BytesVal([]byte("changed"))
	var batch columnar.Batch
	if err := builder.Publish(&batch); err != nil {
		t.Fatal(err)
	}
	if err := batch.Columns[0].Validate(); err != nil {
		t.Fatal(err)
	}
	value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 1); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("owned payload budget: %v", err)
	}
	clear(batch.Columns[0].Children[0].Data)
	clear(batch.Columns[0].Dimensions)
	if value.Type != model.TypeArray || value.IsNull || len(value.Dimensions) != 1 || value.Dimensions[0].Length != 2 || value.Dimensions[0].LowerBound != math.MaxInt32-1 {
		t.Fatalf("maximum array bound changed: %+v", value)
	}
	if len(value.ArrayVal) != 2 || string(value.ArrayVal[0].BytesVal) != "payload" || !value.ArrayVal[1].IsNull || value.ArrayVal[1].Type != model.TypeBytes {
		t.Fatalf("nested point value changed with its source: %+v", value.ArrayVal)
	}
}

func TestBuilderRejectsArrayLastSubscriptOverflow(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Close(); err != nil {
			t.Error(err)
		}
	})
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}}
	if err := builder.Append(t.Context(), []model.Value{input}); !errors.Is(err, model.ErrInvalidArrayShape) {
		t.Fatalf("unrepresentable last subscript: got %v, want invalid shape", err)
	}
}
