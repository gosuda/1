package join_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var errFutureMarkInput = errors.New("later left row failed")

type deferredMarkFailure struct{ *source }

func (s deferredMarkFailure) Next(ctx context.Context, out *columnar.Batch) error {
	if s.index != 0 {
		return errFutureMarkInput
	}
	return s.source.Next(ctx, out)
}

func TestMarkReturnsDemandedRowBeforeFutureInputFailure(t *testing.T) {
	for _, closeEarly := range []bool{false, true} {
		name := "later demand observes failure"
		if closeEarly {
			name = "early close skips failure"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			query := newQuery(t, runtime.OSFileSystem{}, dir)
			options := markOptions(t, query, ir.AnySubquery, 0)
			left := deferredMarkFailure{newSource(t, options.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 20)})}
			right := newSource(t, options.RightLayouts, [][]model.Value{pair(1, 1), pair(2, 2)})
			cursor := open(t, t.Context(), left, right, options)
			batch := columnar.Batch{MaxRows: 1}
			if err := cursor.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 2 {
				t.Fatalf("first mark batch=%+v", batch)
			}
			if batch.Columns[0].Int64[batch.Lane(0)] != 10 || batch.Columns[1].Int64[batch.Lane(0)] != 1 {
				t.Fatalf("first mark output=%+v", batch)
			}
			if !closeEarly {
				if err := cursor.Next(t.Context(), &batch); !errors.Is(err, errFutureMarkInput) || batch.Rows() != 0 {
					t.Fatalf("later mark demand=%v rows=%d", err, batch.Rows())
				}
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, query, dir)
		})
	}
}
