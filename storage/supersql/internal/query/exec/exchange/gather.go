package exchange

import (
	"context"
	"errors"
	"io"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type gather struct {
	mu                        sync.Mutex
	sources                   *Sources
	builder                   *columnar.Builder
	claim                     columnar.Allocation
	batch                     columnar.Batch
	row                       []model.Value
	index, inputCount, offset int
	batchRows                 int
	batchBytes                uint64
	closed                    bool
	closeErr                  error
}

// NewGather owns every child on every outcome. Its output is a bag, with no
// ordering guarantee; one output batch and one transport slot per input coexist.
func NewGather(ctx context.Context, inputs []storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	sources, err := NewSources(ctx, inputs, options)
	if err != nil {
		return nil, err
	}
	c := &gather{sources: sources, inputCount: len(inputs), batchRows: options.BatchRows, batchBytes: options.BatchBytes}
	metadata := uint64(unsafe.Sizeof(gather{})) + uint64(unsafe.Sizeof(columnar.Builder{})) + options.BatchBytes + uint64(len(options.Layouts))*(uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(model.Value{})))
	if metadata > options.MetadataBytes-sources.budget.metadata {
		return nil, errors.Join(exchangeBudget("admit gather metadata", nil), c.Close())
	}
	c.claim, err = options.Query.Allocator(runtime.WorkMemory).Reserve(metadata)
	if err == nil {
		c.builder, err = columnar.NewBuilder(sources.options.Layouts, columnar.BuilderOptions{MaxRows: options.BatchRows, MaxBytes: options.BatchBytes, Allocator: options.Query.Allocator(runtime.OutputMemory)})
	}
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.batch.Columns = make([]columnar.Vector, 0, len(options.Layouts))
	c.row = make([]model.Value, len(options.Layouts))
	return c, nil
}

func (c *gather) next(ctx context.Context, out *columnar.Batch) error {
	if c.closed {
		return errors.Join(runtime.ErrClosed, c.closeErr)
	}
	if out == nil || out.MaxRows < 0 {
		return runtime.ErrInvalidConfig
	}
	out.Reset()
	c.builder.Reset()
	if err := errors.Join(context.Cause(ctx), context.Cause(c.sources.ctx)); err != nil {
		return err
	}
	demand := c.batchRows
	if out.MaxRows > 0 {
		demand = min(demand, out.MaxRows)
	}
	for c.builder.Rows() < demand {
		if c.offset == c.batch.Rows() {
			if c.index == c.inputCount {
				break
			}
			c.batch.Reset()
			c.batch.MaxRows = demand - c.builder.Rows()
			c.offset = 0
			err := c.sources.Next(ctx, c.index, &c.batch)
			if err == io.EOF {
				c.index++
				continue
			}
			if err != nil {
				return err
			}
		}
		if err := copySourceRow(ctx, &c.batch, c.offset, c.row, c.batchBytes); err != nil {
			return exchangeCopyError(err)
		}
		err := c.builder.Append(ctx, c.row)
		clear(c.row)
		if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
			break
		}
		if err != nil {
			return exchangeCopyError(err)
		}
		c.offset++
	}
	if c.builder.Rows() == 0 {
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *gather) Next(ctx context.Context, out *columnar.Batch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.next(ctx, out)
	if err == io.EOF {
		if cleanup := c.closeLocked(); cleanup != nil {
			return cleanup
		}
		return io.EOF
	}
	if err != nil {
		err = errors.Join(err, c.closeLocked())
	}
	return err
}

func (c *gather) closeLocked() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.closeErr = c.sources.Close()
	c.batch.Reset()
	c.batch.Columns = nil
	c.row = nil
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}

func (c *gather) Close() error {
	c.sources.cancel(runtime.ErrClosed)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}
