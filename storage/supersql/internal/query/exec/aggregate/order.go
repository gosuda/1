package aggregate

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

// DISTINCT passes reorder the source. Carry its ordinal through those passes
// so equal ORDER BY keys and unordered sibling calls retain source order.
func (c *cursor) preserveSourceOrder(ctx context.Context) error {
	width := len(c.layouts)
	layouts := append(c.layouts, model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	var err error
	c.deferred, err = c.newSpool(ctx, layouts, true)
	if err != nil {
		return err
	}
	var ordinal int64
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		row, err := c.inputRow(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if ordinal == math.MaxInt64 {
			return resource("aggregate source ordinal overflow", nil)
		}
		copy(c.row, row)
		c.row[width] = model.Int64Val(ordinal)
		if _, err := c.deferred.AppendRow(ctx, c.row[:width+1]); err != nil {
			return err
		}
		ordinal++
	}
	if err := c.finishOrderSpool(ctx, layouts); err != nil {
		return err
	}
	c.sourceOrdinal = width
	return nil
}

// One tagged spool and one sort serve every call. FILTER/DISTINCT qualification
// remains in the original lanes; even disqualified rows retain their group.
func (c *cursor) prepareOrder(ctx context.Context) error {
	width := len(c.layouts)
	layouts := append(c.layouts,
		model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}},
	)
	var err error
	if c.partition != nil {
		c.reader, err = c.partition.Reader(ctx)
		if err != nil {
			return err
		}
	}
	c.deferred, err = c.newSpool(ctx, layouts, true)
	if err != nil {
		return err
	}
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		row, err := c.inputRow(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		copy(c.row, row)
		for i, call := range c.calls {
			if err := c.check(ctx); err != nil {
				return err
			}
			var encoded []byte
			if len(call.order) != 0 && qualifies(call, row) {
				for j, key := range call.order {
					c.keyValues[j] = row[key.Column]
				}
				encoded, err = c.options.Encoder.EncodePrefix(ctx, call.orderKey, c.keyValues[:len(call.order)], c.options.MaxRecordBytes)
				if err != nil {
					return err
				}
			}
			c.row[width], c.row[width+1] = model.Int64Val(int64(i)), model.BytesVal(encoded)
			if _, err := c.deferred.AppendRow(ctx, c.row[:width+2]); err != nil {
				return err
			}
		}
	}
	if err := c.finishOrderSpool(ctx, layouts); err != nil {
		return err
	}
	reader, err := c.partition.Reader(ctx)
	if err != nil {
		return err
	}
	builder, err := columnar.NewBuilder(c.layouts, columnar.BuilderOptions{MaxRows: c.options.BatchRows, MaxBytes: c.options.BatchBytes, Allocator: c.options.Query.Allocator(runtime.WorkMemory)})
	if err != nil {
		return errors.Join(err, reader.Close())
	}
	input := &spoolCursor{reader: reader, builder: builder, maxRows: c.options.BatchRows}
	keys := make([]externalsort.Key, 0, len(c.options.GroupColumns)+3)
	for _, column := range c.options.GroupColumns {
		keys = append(keys, externalsort.Key{Column: column})
	}
	keys = append(keys, externalsort.Key{Column: width}, externalsort.Key{Column: width + 1})
	if c.sourceOrdinal >= 0 {
		keys = append(keys, externalsort.Key{Column: c.sourceOrdinal})
	}
	c.input, err = externalsort.New(c.ctx, input, externalsort.Options{Layouts: c.layouts, Keys: keys, Encoder: c.options.Encoder, Query: c.options.Query, BatchRows: c.options.BatchRows, BatchBytes: c.options.BatchBytes, MaxRecordBytes: c.options.MaxRecordBytes, RunBytes: c.options.PartitionMemoryBytes, MaxRuns: c.options.MaxPartitions, MergeFanIn: 2, WorkBytes: c.sortWork})
	if err != nil {
		return err
	}
	c.orderTag = width
	return nil
}

func (c *cursor) finishOrderSpool(ctx context.Context, layouts []model.TypeLayout) error {
	var err error
	if c.input != nil {
		err = errors.Join(err, c.input.Close())
		c.input = nil
	}
	if c.reader != nil {
		err = errors.Join(err, c.reader.Close())
		c.reader = nil
	}
	if c.partition != nil {
		err = errors.Join(err, c.partition.Close())
		c.partition = nil
	}
	if err != nil {
		return err
	}
	if err := c.deferred.Seal(ctx); err != nil {
		return err
	}
	c.partition, c.deferred = c.deferred, nil
	c.layouts = layouts
	c.batch.Reset()
	c.lane = 0
	clear(c.row)
	clear(c.keyValues)
	return nil
}
