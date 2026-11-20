package exec

import (
	"context"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type selectedPartitionInput struct{ done bool }

func (s *selectedPartitionInput) partition() uint32 { return 1 }
func (s *selectedPartitionInput) Close() error      { return nil }
func (s *selectedPartitionInput) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out.Reset()
	if s.done {
		return io.EOF
	}
	s.done = true
	values := make([]int64, 16)
	values[9], values[2] = 7, 11
	out.Columns = append(out.Columns, columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 16, Int64: values})
	out.Length = 16
	out.Selection = []uint32{9, 2}
	return nil
}

func TestPartitionTagPreservesSparsePhysicalLanes(t *testing.T) {
	input := &partitionTagInput{input: &selectedPartitionInput{}, partitions: 4, batchRows: 2, columns: 1, output: make([]columnar.Vector, 2)}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	})
	out := columnar.Batch{MaxRows: 2}
	if err := input.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 2 || out.Length != 16 {
		t.Fatalf("selected rows=%d physical length=%d", out.Rows(), out.Length)
	}
	for row, want := range []int64{7, 11} {
		lane := out.Lane(row)
		tag := out.Columns[0].Int64[out.Columns[0].ValueIndex(lane)]
		value := out.Columns[1].Int64[out.Columns[1].ValueIndex(lane)]
		if tag != 1 || value != want {
			t.Fatalf("row %d tag=%d value=%d, want tag=1 value=%d", row, tag, value, want)
		}
	}
}
