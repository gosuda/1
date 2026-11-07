package modify

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Bound the source before external sorting, not only its sorted output.
type limitedInput struct {
	input   storage.BatchCursor
	maximum uint64
	rows    uint64
}

func (c *limitedInput) Next(ctx context.Context, out *columnar.Batch) error {
	if err := c.input.Next(ctx, out); err != nil {
		return err
	}
	rows := out.Rows()
	if rows < 0 || uint64(rows) > c.maximum-c.rows {
		return resource("modify input row limit")
	}
	c.rows += uint64(rows)
	return nil
}

func (c *limitedInput) Close() error { return c.input.Close() }
