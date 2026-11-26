package neosepia

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestGatherPAXReclaimsUnselectedArrayChildren(t *testing.T) {
	source := columnar.Batch{Length: 3, Columns: []columnar.Vector{
		{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Length: 3, Nulls: []byte{4},
			Ranges:      []columnar.Range{{Offset: 0, Length: 2}, {Offset: 2, Length: 1}, {Offset: 3, Length: 1}},
			ShapeRanges: []columnar.Range{{Offset: 0, Length: 1}, {Offset: 1, Length: 1}, {Offset: 1, Length: 1}},
			Dimensions:  []model.ArrayDimension{{Length: 2, LowerBound: -3}, {Length: 1, LowerBound: 7}},
			Children:    []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 4, Int64: []int64{11, 22, 99, 77}}}},
		{Type: model.Type{ID: 25, Storage: model.TypeString}, Encoding: columnar.Dictionary, Length: 3, Nulls: []byte{4},
			Data: []byte("retiredkept"), Ranges: []columnar.Range{{Length: 7}, {Offset: 7, Length: 4}}, Indices: []uint32{0, 1, 999}},
	}}
	selected, used, err := gatherDecodedBlock(t.Context(), &source, []uint32{1, 2}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := gatherDecodedBlock(t.Context(), &source, []uint32{1, 2}, used-1); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("insufficient gather budget: %v", err)
	}
	versions := blockVersions{Keys: encTestKeyVector("b", "c"), CommitTS: []model.Timestamp{20, 20}, Ordinals: []uint64{1, 2}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert}}
	encoded, err := encodeBlock(t.Context(), encTestTarget(), versions, &selected, []model.ColumnID{1, 2}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	block, err := openBlock(encoded, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	array, err := block.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(array.Children[0].Int64, []int64{99}) {
		t.Fatalf("encoded array retained discarded values: %v", array.Children[0].Int64)
	}
	value, _, err := columnar.CopyValue(t.Context(), &array, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(value.Dimensions, []model.ArrayDimension{{Length: 1, LowerBound: 7}}) {
		t.Fatalf("selected array dimensions: %v", value.Dimensions)
	}
	text, err := block.Column(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	textValue, _, err := columnar.CopyValue(t.Context(), &text, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if textValue.StrVal != "kept" {
		t.Fatalf("selected dictionary value: %q", textValue.StrVal)
	}
	if !array.IsNull(1) || !text.IsNull(1) {
		t.Fatal("gathered NULL lane became a stored value")
	}
}
