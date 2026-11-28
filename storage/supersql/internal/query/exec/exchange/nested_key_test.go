package exchange

import (
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestHashRepartitionOwnsNestedKeyDeclarations(t *testing.T) {
	query, _ := transportRuntime(t, runtime.QueryConfig{})
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1}}
	array := model.TypeLayout{Type: model.Type{ID: 1231, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{numeric}}
	layouts := []model.TypeLayout{array, transportInt}
	value := func(text string, null bool) model.Value {
		return model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{{Type: model.TypeNumeric, StrVal: text, IsNull: null}}}
	}
	left := hashRows(t, layouts, [][]model.Value{{value("1.00", false), model.Int64Val(1)}, {value("", true), model.Int64Val(3)}})
	right := hashRows(t, layouts, [][]model.Value{{value("1", false), model.Int64Val(2)}, {value("", true), model.Int64Val(4)}})
	options := hashOptions(t, query)
	options.Sources.Layouts = layouts
	cursor := openHash(t, []storage.BatchCursor{left, right}, options)
	layouts[0].Children[0].Type.ID = 20
	partitions := make(map[int64]uint32)
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 1}}
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range out.Batch.Rows() {
			value, _, err := columnar.CopyValue(t.Context(), &out.Batch.Columns[1], out.Batch.Lane(row), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if value.IntVal < 1 || value.IntVal > 4 {
				t.Fatalf("unexpected row identity %d", value.IntVal)
			}
			if _, duplicate := partitions[value.IntVal]; duplicate {
				t.Fatalf("duplicate row %d", value.IntVal)
			}
			partitions[value.IntVal] = out.Partition
		}
	}
	if len(partitions) != 4 || partitions[1] != partitions[2] || partitions[3] != partitions[4] {
		t.Fatalf("equivalent nested keys were separated: %v", partitions)
	}
	hashReleased(t, query)
}
