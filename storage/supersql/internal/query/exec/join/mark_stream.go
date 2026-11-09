package join

import (
	"context"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *cursor) readMarkLeft(ctx context.Context) error {
	if c.markEOF {
		return io.EOF
	}
	for c.markIndex >= c.markBatch.Rows() {
		if err := c.check(ctx); err != nil {
			return err
		}
		c.markBatch.Reset()
		c.markBatch.MaxRows = c.leftDemand
		err := c.left.Next(ctx, &c.markBatch)
		if err == io.EOF {
			c.markEOF = true
			input := c.left
			c.left = nil
			if err := input.Close(); err != nil {
				return err
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		if err := c.markBatch.Validate(max(c.markBatch.Length, c.leftDemand)); err != nil {
			return err
		}
		c.markIndex = 0
		if c.markBatch.Rows() == 0 {
			continue
		}
		if c.markBatch.Rows() > c.leftDemand || len(c.markBatch.Columns) != len(c.options.LeftLayouts) {
			return columnar.ErrInvalidBatch
		}
		for i, layout := range c.options.LeftLayouts {
			if !c.markBatch.Columns[i].MatchesLayout(layout) {
				return columnar.ErrValueLayout
			}
		}
	}
	lane := c.markBatch.Lane(c.markIndex)
	c.markIndex++
	if err := c.prepareRow(ctx, &c.markBatch, lane, Left, c.markRow); err != nil {
		return err
	}
	c.leftRow = c.markRow
	c.markReady = true
	return nil
}

func (c *cursor) initializeMark(ctx context.Context) error {
	err := c.readMarkLeft(ctx)
	if err == io.EOF {
		input := c.right
		c.right = nil
		return input.Close()
	}
	if err != nil {
		return err
	}
	c.partitions[0].right, err = c.newSpool(ctx, Right)
	if err != nil {
		return err
	}
	return c.consume(ctx, Right)
}

func (c *cursor) nextMark(ctx context.Context) error {
	if !c.markReady {
		if err := c.readMarkLeft(ctx); err != nil {
			return err
		}
	}
	if c.rightReader == nil {
		reader, err := c.partitions[0].right.Reader(ctx)
		if err != nil {
			return err
		}
		c.rightReader = reader
	} else if err := c.rightReader.Seek(ctx, 0); err != nil {
		return err
	}
	c.mark = model.BoolVal(c.options.MarkKind == ir.AllSubquery)
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		_, right, err := c.rightReader.NextRow(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		truth, err := c.matches(ctx, c.leftRow, right)
		if err != nil {
			return err
		}
		c.accumulateMark(truth)
	}
	c.mapRow(c.output, c.options.Output, c.leftRow, nil)
	c.markReady = false
	return nil
}
