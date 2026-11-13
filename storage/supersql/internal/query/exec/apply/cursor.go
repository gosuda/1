package apply

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var errCardinality = &model.Error{Code: "21000", Message: "more than one row returned by a scalar subquery"}
var errFactoryCursor = errors.New("Apply factory returned no cursor")

type cursor struct {
	options                         Options
	input, right                    storage.BatchCursor
	ctx                             context.Context
	cancel                          context.CancelFunc
	cancelRight                     context.CancelFunc
	stopQuery                       func() bool
	claim                           columnar.Allocation
	builder                         *columnar.Builder
	leftBatch, rightBatch           columnar.Batch
	leftIndex, rightIndex           int
	leftDemand                      int
	leftValues, rightValues, output []model.Value
	haveLeft, matched, closed, eof  bool
	closeErr                        error
}

func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.ctx))
}

func (c *cursor) readRow(ctx context.Context, source storage.BatchCursor, batch *columnar.Batch, index *int, layouts []model.TypeLayout, limit int) (int, error) {
	if err := c.check(ctx); err != nil {
		return 0, err
	}
	return readRow(ctx, source, batch, index, layouts, limit)
}

func readRow(ctx context.Context, source storage.BatchCursor, batch *columnar.Batch, index *int, layouts []model.TypeLayout, limit int) (int, error) {
	for *index >= batch.Rows() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		batch.Reset()
		batch.MaxRows = limit
		if err := source.Next(ctx, batch); err != nil {
			return 0, err
		}
		if err := batch.Validate(max(batch.Length, limit)); err != nil {
			return 0, err
		}
		*index = 0
		if batch.Rows() == 0 {
			continue
		}
		if batch.Rows() > limit || len(batch.Columns) != len(layouts) {
			return 0, columnar.ErrInvalidBatch
		}
		for i := range layouts {
			if !batch.Columns[i].MatchesLayout(layouts[i]) {
				return 0, columnar.ErrValueLayout
			}
		}
	}
	lane := batch.Lane(*index)
	*index++
	return lane, nil
}

func (c *cursor) copyRow(ctx context.Context, batch *columnar.Batch, lane int, values []model.Value) error {
	clear(values)
	remaining := c.options.MaxRowBytes
	for i := range values {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, remaining)
		if err != nil {
			return err
		}
		values[i], remaining = value, remaining-used
	}
	return nil
}

func (c *cursor) startLeft(ctx context.Context) error {
	lane, err := c.readRow(ctx, c.input, &c.leftBatch, &c.leftIndex, c.options.LeftLayouts, c.leftDemand)
	if err != nil {
		if err == io.EOF {
			c.eof = true
		}
		return err
	}
	if err := c.copyRow(ctx, &c.leftBatch, lane, c.leftValues); err != nil {
		return err
	}
	c.haveLeft, c.matched = true, false
	c.rightIndex = 0
	c.rightBatch.Reset()
	rightContext, cancel := context.WithCancel(c.ctx)
	c.cancelRight = cancel
	stop := context.AfterFunc(ctx, cancel)
	c.right, err = c.options.Factory.Open(rightContext, c.leftValues)
	stop()
	if err != nil {
		return err
	}
	if c.right == nil {
		return errFactoryCursor
	}
	return nil
}

func (c *cursor) closeRight() error {
	if c.cancelRight != nil {
		c.cancelRight()
		c.cancelRight = nil
	}
	if c.right == nil {
		return nil
	}
	right := c.right
	c.right = nil
	err := right.Close()
	c.rightBatch.Reset()
	c.rightIndex = 0
	return err
}

type rowPresence struct {
	right bool
	mark  bool
}

func (c *cursor) emit(presence rowPresence) []model.Value {
	for i, column := range c.options.Output {
		switch column.Side {
		case Left:
			c.output[i] = c.leftValues[column.Index]
		case Right:
			if presence.right {
				c.output[i] = c.rightValues[column.Index]
			} else {
				c.output[i] = model.NullVal(c.options.RightLayouts[column.Index].Type.Storage)
			}
		case Mark:
			c.output[i] = model.BoolVal(presence.mark)
		}
	}
	return c.output
}

func (c *cursor) readRight(ctx context.Context) (int, error) {
	limit := c.options.BatchRows
	if c.options.Kind == ir.ExistsApply {
		limit = 1
	}
	if c.options.Kind == ir.ScalarApply {
		limit = min(limit, 2)
	}
	return c.readRow(ctx, c.right, &c.rightBatch, &c.rightIndex, c.options.RightLayouts, limit)
}

func (c *cursor) scalar(ctx context.Context) ([]model.Value, error) {
	lane, err := c.readRight(ctx)
	present := err == nil
	if err != nil && err != io.EOF {
		return nil, err
	}
	if present {
		if err := c.copyRow(ctx, &c.rightBatch, lane, c.rightValues); err != nil {
			return nil, err
		}
		_, err := c.readRight(ctx)
		if err == nil {
			return nil, errCardinality
		}
		if err != io.EOF {
			return nil, err
		}
	}
	if err := c.closeRight(); err != nil {
		return nil, err
	}
	c.haveLeft = false
	return c.emit(rowPresence{right: present}), nil
}

func (c *cursor) exists(ctx context.Context) ([]model.Value, error) {
	_, err := c.readRight(ctx)
	present := err == nil
	if err != nil && err != io.EOF {
		return nil, err
	}
	if err := c.closeRight(); err != nil {
		return nil, err
	}
	c.haveLeft = false
	return c.emit(rowPresence{mark: present}), nil
}

func (c *cursor) next(ctx context.Context) ([]model.Value, error) {
	if c.eof {
		return nil, io.EOF
	}
	for {
		if err := c.check(ctx); err != nil {
			return nil, err
		}
		if !c.haveLeft {
			if err := c.startLeft(ctx); err != nil {
				return nil, err
			}
		}
		switch c.options.Kind {
		case ir.ScalarApply:
			return c.scalar(ctx)
		case ir.ExistsApply:
			return c.exists(ctx)
		}
		lane, err := c.readRight(ctx)
		if err == nil {
			if err := c.copyRow(ctx, &c.rightBatch, lane, c.rightValues); err != nil {
				return nil, err
			}
			c.matched = true
			return c.emit(rowPresence{right: true}), nil
		}
		if err != io.EOF {
			return nil, err
		}
		if err := c.closeRight(); err != nil {
			return nil, err
		}
		c.haveLeft = false
		if c.options.Outer && !c.matched {
			return c.emit(rowPresence{}), nil
		}
	}
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.closed {
		if c.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil {
			c.eof = err == io.EOF
			cleanup := c.Close()
			if cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.builder.Reset()
	for count := 0; count < limit; count++ {
		c.leftDemand = 1
		if c.options.Kind != ir.LateralApply {
			c.leftDemand = limit - count
		}
		row, err := c.next(call)
		if err == io.EOF {
			if count == 0 {
				return io.EOF
			}
			break
		}
		if err != nil {
			return err
		}
		if err := c.builder.Append(call, row); err != nil {
			return err
		}
	}
	return c.builder.Publish(out)
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.stopQuery != nil {
		c.stopQuery()
		c.stopQuery = nil
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.closeErr = c.closeRight()
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.leftBatch.Reset()
	c.rightBatch.Reset()
	c.leftValues, c.rightValues, c.output = nil, nil, nil
	c.options = Options{}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
