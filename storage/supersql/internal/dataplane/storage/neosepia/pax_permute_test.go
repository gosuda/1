package neosepia

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPAXPermutationKeepsNestedFieldsWithTheirRows(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
	layout := model.TypeLayout{Type: model.Type{ID: 40000, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer, {Type: model.Type{ID: 25, Storage: model.TypeString}}, {Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 3, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Close(); err != nil {
			t.Error(err)
		}
	})
	rows := []model.Value{
		{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(10), model.StringVal("a"), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.Int64Val(100), model.Int64Val(101)}}}},
		model.NullVal(model.TypeComposite),
		{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(30), model.StringVal("c"), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 5}}, ArrayVal: []model.Value{model.Int64Val(300)}}}},
	}
	for _, row := range rows {
		if err := builder.Append(t.Context(), []model.Value{row}); err != nil {
			t.Fatal(err)
		}
	}
	var source columnar.Batch
	if err := builder.Publish(&source); err != nil {
		t.Fatal(err)
	}
	permuted, _, err := permuteBlock(t.Context(), &source, []uint32{2, 1, 0}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	versions := blockVersions{Keys: encTestKeyVector("a", "b", "c"), CommitTS: []model.Timestamp{1, 1, 1}, Ordinals: []uint64{3, 2, 1}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert}}
	data, err := encodeBlock(t.Context(), encTestTarget(), versions, &permuted, []model.ColumnID{1}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	block, err := openBlock(data, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	vector, err := block.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !vector.IsNull(1) || vector.Children[0].Int64[0] != 30 || vector.Children[0].Int64[2] != 10 {
		t.Fatal("composite fields detached from their rows")
	}
	text := &vector.Children[1]
	first, last := text.Ranges[text.ValueIndex(0)], text.Ranges[text.ValueIndex(2)]
	if string(text.Data[first.Offset:first.Offset+first.Length]) != "c" || string(text.Data[last.Offset:last.Offset+last.Length]) != "a" {
		t.Fatal("variable-width fields detached from their rows")
	}
	array := &vector.Children[2]
	first, last = array.Ranges[0], array.Ranges[2]
	if first.Length != 1 || last.Length != 2 || array.Children[0].Int64[first.Offset] != 300 || array.Children[0].Int64[last.Offset+1] != 101 {
		t.Fatal("array elements detached from their rows")
	}
	if array.Dimensions[array.ShapeRanges[0].Offset].LowerBound != 5 || array.Dimensions[array.ShapeRanges[2].Offset].LowerBound != math.MaxInt32-1 {
		t.Fatal("array ranges or lower bounds detached from their rows")
	}
	if source.Columns[0].Children[0].Int64[0] != 10 {
		t.Fatal("permutation mutated its borrowed source")
	}
	if _, _, err := permuteBlock(t.Context(), &source, []uint32{0, 0, 2}, 1<<20); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("duplicate lane accepted: %v", err)
	}
	if _, _, err := permuteBlock(t.Context(), &source, []uint32{2, 1, 0}, 1); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("allocation bound ignored: %v", err)
	}
}
