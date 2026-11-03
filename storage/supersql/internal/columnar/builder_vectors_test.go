package columnar_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func vectorAppendBuilder(t *testing.T, layouts []model.TypeLayout, bytes uint64) *columnar.Builder {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 4, MaxBytes: bytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}

func TestAppendVectorsReadsOnlySelectedDictionaryPayload(t *testing.T) {
	typ := model.Type{ID: 17, Storage: model.TypeBytes}
	b := vectorAppendBuilder(t, []model.TypeLayout{{Type: typ}}, 4096)
	source := columnar.Vector{Type: typ, Encoding: columnar.Dictionary, Length: 3, Nulls: []byte{1}, Indices: []uint32{math.MaxUint32, 0, math.MaxUint32}, Ranges: []columnar.Range{{Length: 3}}, Data: []byte("abc")}
	for _, lane := range []int{0, 1} {
		if err := b.AppendVectors(t.Context(), []*columnar.Vector{&source}, []int{lane}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AppendVectors(t.Context(), []*columnar.Vector{nil}, []int{-1}); err != nil {
		t.Fatal(err)
	}
	clear(source.Data)
	var batch columnar.Batch
	if err := b.Publish(&batch); err != nil {
		t.Fatal(err)
	}
	for lane, want := range []model.Value{model.NullVal(model.TypeBytes), model.BytesVal([]byte("abc")), model.NullVal(model.TypeBytes)} {
		got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], lane, 4096)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lane %d=%v,%v want %v", lane, got, err, want)
		}
	}
}

func TestAppendVectorsRollsBackNestedRowsAfterBudgetFailure(t *testing.T) {
	bytes := model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}
	array := model.TypeLayout{Type: model.Type{ID: 1001, Storage: model.TypeArray}, Children: []model.TypeLayout{bytes}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{array}}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.BytesVal([]byte("kept")), model.NullVal(model.TypeBytes)}}}}
	sourceBuilder := vectorAppendBuilder(t, []model.TypeLayout{record}, 16384)
	if err := sourceBuilder.Append(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	var source columnar.Batch
	if err := sourceBuilder.Publish(&source); err != nil {
		t.Fatal(err)
	}
	b := vectorAppendBuilder(t, []model.TypeLayout{record, bytes}, 16384)
	text := columnar.Vector{Type: bytes.Type, Encoding: columnar.Constant, Length: 1, Data: []byte("ok"), Ranges: []columnar.Range{{Length: 2}}}
	vectors := []*columnar.Vector{&source.Columns[0], &text}
	lanes := []int{0, 0}
	if err := b.AppendVectors(t.Context(), vectors, lanes); err != nil {
		t.Fatal(err)
	}
	text.Data = make([]byte, 32768)
	text.Ranges[0].Length = 32768
	if err := b.AppendVectors(t.Context(), vectors, lanes); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("oversized row=%v", err)
	}
	if err := b.AppendVectors(t.Context(), []*columnar.Vector{nil, nil}, lanes); err != nil {
		t.Fatal(err)
	}
	clear(source.Columns[0].Children[0].Children[0].Data)
	clear(source.Columns[0].Children[0].Dimensions)
	var batch columnar.Batch
	if err := b.Publish(&batch); err != nil {
		t.Fatal(err)
	}
	if err := batch.Validate(4); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 2 {
		t.Fatalf("failed row retained: rows=%d", batch.Rows())
	}
	got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 16384)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("nested row=%v,%v want %v", got, err, value)
	}
	null, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 1, 0)
	if err != nil || !null.IsNull {
		t.Fatalf("padded composite=%v,%v", null, err)
	}
	textValue, _, err := columnar.CopyValue(t.Context(), &batch.Columns[1], 0, 2)
	if err != nil || string(textValue.BytesVal) != "ok" {
		t.Fatalf("earlier text changed=%v,%v", textValue, err)
	}
}

func TestAppendVectorsRejectsDifferentSQLIdentityEvenForNull(t *testing.T) {
	b := vectorAppendBuilder(t, []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}}, 4096)
	source := columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Encoding: columnar.Flat, Length: 1, Nulls: []byte{1}}
	if err := b.AppendVectors(t.Context(), []*columnar.Vector{&source}, []int{0}); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("different NULL SQL identity=%v", err)
	}
	if err := b.AppendVectors(t.Context(), []*columnar.Vector{nil}, []int{0}); err != nil {
		t.Fatal(err)
	}
	var batch columnar.Batch
	if err := b.Publish(&batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || !batch.Columns[0].IsNull(0) {
		t.Fatal("failed layout check changed output")
	}
}
