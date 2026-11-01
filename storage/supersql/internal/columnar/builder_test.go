package columnar_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBatchBuilderRestoresNestedColumnsAfterRejectedRow(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString}}
	b, err := columnar.NewBuilder([]model.TypeLayout{array, text}, columnar.BuilderOptions{MaxRows: 4, MaxBytes: 16384})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	row := []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -1}}, ArrayVal: []model.Value{model.Int64Val(3), model.NullVal(model.TypeInt64)}}, model.StringVal("kept")}
	if err := b.Append(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	row[0].ArrayVal[0] = model.Int64Val(99)
	row[1] = model.BoolVal(true)
	if err := b.Append(t.Context(), row); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("wrong text value: %v", err)
	}
	if b.Rows() != 1 {
		t.Fatalf("rejected row changed row count: %d", b.Rows())
	}
	row[0] = model.NullVal(model.TypeArray)
	row[1] = model.StringVal("next")
	if err := b.Append(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	var out columnar.Batch
	if err := b.Publish(&out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(4); err != nil {
		t.Fatal(err)
	}
	v := out.Columns[0]
	if v.IsNull(0) || !v.IsNull(1) {
		t.Fatal("array nulls changed after rollback")
	}
	if v.Children[0].Length != 2 || v.Children[0].Int64[0] != 3 || !v.Children[0].IsNull(1) {
		t.Fatal("failed row leaked nested children or caller mutation changed prior row")
	}
	if v.Dimensions[0].LowerBound != -1 {
		t.Fatal("array lower bound lost")
	}
	strings := out.Columns[1]
	for i, want := range []string{"kept", "next"} {
		r := strings.Ranges[i]
		if got := string(strings.Data[r.Offset : r.Offset+r.Length]); got != want {
			t.Fatalf("row %d = %q, want %q", i, got, want)
		}
	}
}

func TestBatchBuilderBudgetFailurePreservesEarlierRows(t *testing.T) {
	b, err := columnar.NewBuilder([]model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}, columnar.BuilderOptions{MaxRows: 4, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := b.Append(t.Context(), []model.Value{model.BytesVal([]byte("keep"))}); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(t.Context(), []model.Value{model.BytesVal(make([]byte, 8192))}); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("oversized row: %v", err)
	}
	var out columnar.Batch
	if err := b.Publish(&out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(4); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || string(out.Columns[0].Data) != "keep" {
		t.Fatal("budget error changed earlier row")
	}
	if b.AllocatedBytes() > 4096 {
		t.Fatalf("retained bytes %d exceed budget", b.AllocatedBytes())
	}
}
