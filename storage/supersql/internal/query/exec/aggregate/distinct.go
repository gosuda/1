package aggregate

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

// Each DISTINCT call adds a qualification lane after ordering by grouping key
// and argument tuple. Later sorts carry those lanes unchanged, so separate
// DISTINCT tuples and FILTER predicates do not interfere or need unbounded sets.
func (c *cursor) prepareDistinct(ctx context.Context) error {
	for i := range c.calls {
		if !c.calls[i].distinct {
			continue
		}
		if err := c.prepareDistinctCall(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

func (c *cursor) prepareDistinctCall(ctx context.Context, index int) error {
	call := &c.calls[index]
	keys := make([]externalsort.Key, 0, len(c.options.GroupColumns)+call.arity)
	keyLayout := make([]storage.KeyColumn, 0, len(c.options.GroupColumns)+call.arity)
	for _, column := range c.options.GroupColumns {
		keys = append(keys, externalsort.Key{Column: column})
		keyLayout = append(keyLayout, storage.KeyColumn{ColumnID: model.ColumnID(len(keyLayout) + 1), Type: c.layouts[column].Type, Children: c.groupKey[len(keyLayout)].Children})
	}
	for i := range call.arity {
		keys = append(keys, externalsort.Key{Column: call.arguments[i], InputCollation: call.key[i].Type.Collation})
		keyLayout = append(keyLayout, storage.KeyColumn{ColumnID: model.ColumnID(len(keyLayout) + 1), Type: call.key[i].Type, Children: call.key[i].Children})
	}
	if c.partition != nil {
		reader, err := c.partition.Reader(ctx)
		if err != nil {
			return err
		}
		builder, err := columnar.NewBuilder(c.layouts, columnar.BuilderOptions{MaxRows: c.options.BatchRows, MaxBytes: c.options.BatchBytes, Allocator: c.options.Query.Allocator(runtime.WorkMemory)})
		if err != nil {
			return errors.Join(err, reader.Close())
		}
		c.input = &spoolCursor{reader: reader, builder: builder, maxRows: c.options.BatchRows}
	}
	input := c.input
	c.input = nil
	sorted, err := externalsort.New(ctx, input, externalsort.Options{Layouts: c.layouts, Keys: keys, Encoder: c.options.Encoder, Query: c.options.Query, BatchRows: c.options.BatchRows, BatchBytes: c.options.BatchBytes, MaxRecordBytes: c.options.MaxRecordBytes, RunBytes: c.options.PartitionMemoryBytes, MaxRuns: c.options.MaxPartitions, MergeFanIn: 2, WorkBytes: c.sortWork})
	if err != nil {
		return err
	}
	c.input = sorted
	oldWidth := len(c.layouts)
	nextLayouts := append(c.layouts, model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	c.deferred, err = c.newSpool(ctx, nextLayouts, true)
	if err != nil {
		return err
	}
	var previous []byte
	batch := columnar.Batch{MaxRows: c.options.BatchRows}
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		err := sorted.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for lane := range batch.Rows() {
			previous, err = c.distinctRow(ctx, *call, &batch, batch.Lane(lane), keyLayout, previous)
			if err != nil {
				return err
			}
		}
		batch.Reset()
	}
	if err := sorted.Close(); err != nil {
		c.input = nil
		return err
	}
	c.input = nil
	if c.partition != nil {
		if err := c.partition.Close(); err != nil {
			c.partition = nil
			return err
		}
		c.partition = nil
	}
	if err := c.deferred.Seal(ctx); err != nil {
		return err
	}
	c.partition, c.deferred = c.deferred, nil
	c.layouts = nextLayouts
	call.filter, call.distinct = oldWidth, false
	clear(c.row)
	clear(c.keyValues)
	return nil
}

func (c *cursor) distinctRow(ctx context.Context, call compiledCall, batch *columnar.Batch, lane int, keyLayout []storage.KeyColumn, previous []byte) ([]byte, error) {
	oldWidth := len(c.layouts)
	remaining := c.options.MaxRecordBytes * 8
	for column := range oldWidth {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[column], lane, remaining)
		if err != nil {
			return nil, err
		}
		c.row[column], remaining = value, remaining-used
	}
	arguments := c.arguments[:call.arity]
	for i := range call.arity {
		arguments[i] = c.row[call.arguments[i]]
	}
	eligible := qualifies(call, c.row) && call.acceptsArguments(arguments)
	if eligible {
		for j, column := range c.options.GroupColumns {
			c.keyValues[j] = c.row[column]
		}
		for i := range call.arity {
			c.keyValues[len(c.groupKey)+i] = c.row[call.arguments[i]]
		}
		key, err := c.options.Encoder.EncodePrefix(ctx, keyLayout, c.keyValues[:len(keyLayout)], c.options.MaxRecordBytes)
		if err != nil {
			return nil, err
		}
		eligible = !bytes.Equal(previous, key)
		previous = key
	}
	c.row[oldWidth] = model.BoolVal(eligible)
	_, err := c.deferred.AppendRow(ctx, c.row[:oldWidth+1])
	return previous, err
}

type spoolCursor struct {
	reader   *rowstore.Reader
	builder  *columnar.Builder
	pending  []model.Value
	maxRows  int
	closed   bool
	closeErr error
}

func (c *spoolCursor) Next(ctx context.Context, out *columnar.Batch) error {
	if c.closed {
		return io.EOF
	}
	out.Reset()
	c.builder.Reset()
	limit := c.maxRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.builder.Rows() < limit {
		if c.pending == nil {
			_, row, err := c.reader.NextRow(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return errors.Join(err, c.Close())
			}
			c.pending = row
		}
		if err := c.builder.Append(ctx, c.pending); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return errors.Join(err, c.Close())
		}
		c.pending = nil
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *spoolCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.closeErr = errors.Join(c.reader.Close(), c.builder.Close())
	c.pending = nil
	return c.closeErr
}
