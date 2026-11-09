package columnar_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestDictionaryNullDoesNotAddressPayload(t *testing.T) {
	v := columnar.Vector{
		Type: model.Type{ID: 20, Storage: model.TypeInt64}, Encoding: columnar.Dictionary,
		Offset: 1, Length: 2, Nulls: []byte{0b10}, Indices: []uint32{99, math.MaxUint32, 0}, Int64: []int64{42},
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("NULL dictionary lane must not require payload: %v", err)
	}
	if !v.IsNull(0) || v.IsNull(1) {
		t.Fatal("dictionary nulls must follow logical lanes")
	}
	if got := v.Int64[v.ValueIndex(1)]; got != 42 {
		t.Fatalf("selected value = %d, want 42", got)
	}
}

func TestArrayShapeMustMatchChildCount(t *testing.T) {
	v := columnar.Vector{
		Type: model.Type{ID: 1007, Storage: model.TypeArray}, Length: 1,
		Ranges: []columnar.Range{{Length: 2}}, ShapeRanges: []columnar.Range{{Length: 1}},
		Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: -2}},
		Children:   []columnar.Vector{{Type: model.Type{ID: 23, Storage: model.TypeInt64}, Length: 2, Int64: []int64{7, 8}}},
	}
	if err := v.Validate(); !errors.Is(err, columnar.ErrInvalidVector) {
		t.Fatalf("mismatched shape: %v", err)
	}
}

func TestBooleanVectorRejectsNonBooleanPayload(t *testing.T) {
	v := columnar.Vector{Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Length: 1, Int64: []int64{2}}
	if err := v.Validate(); !errors.Is(err, columnar.ErrInvalidVector) {
		t.Fatalf("non-Boolean payload: %v", err)
	}
}

func TestEmptySelectionIsNotUnfilteredBatch(t *testing.T) {
	b := columnar.Batch{Length: 3, Selection: []uint32{}}
	if err := b.Validate(3); err != nil {
		t.Fatal(err)
	}
	if b.Rows() != 0 {
		t.Fatalf("filtered rows = %d, want 0", b.Rows())
	}
	b.Selection = []uint32{2, 0, 2}
	if b.Rows() != 3 || b.Lane(0) != 2 || b.Lane(1) != 0 || b.Lane(2) != 2 {
		t.Fatal("selection order or duplicates lost")
	}
}

func TestBatchResetReleasesBorrowedPayloads(t *testing.T) {
	data := []byte("borrowed")
	headers := make([]columnar.Vector, 2)
	headers[0].Data = data
	headers[1].Children = []columnar.Vector{{Data: data}}
	b := columnar.Batch{Columns: headers[:1], Length: 1, Selection: []uint32{0}}
	b.Reset()
	if b.Rows() != 0 || len(b.Columns) != 0 {
		t.Fatal("reset batch retained rows")
	}
	for _, v := range headers {
		if v.Data != nil || v.Children != nil {
			t.Fatal("reset retained borrowed payload outside visible header length")
		}
	}
	if string(data) != "borrowed" {
		t.Fatal("reset changed borrowed data")
	}
}
