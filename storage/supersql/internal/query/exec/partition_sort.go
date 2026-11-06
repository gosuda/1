package exec

import (
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// The transport tag is payload, never a comparator key. Sorting the multiplexed
// stream gives a stronger order than independently sorting its partitions.
func (c *invocation) partitionSort(node physical.NodeSpec, slots []physical.Slot, layouts []model.TypeLayout, input storage.BatchCursor) (_ storage.BatchCursor, err error) {
	defer func() {
		if err != nil && input != nil {
			err = errors.Join(err, input.Close())
		}
	}()
	env := &c.portal.env
	if env.Query == nil || env.Keys == nil {
		return nil, ErrInvalid
	}
	partitions := node.Delivered.Distribution.Partitions
	invalidDistribution := node.Delivered.Distribution.Kind != physical.HashPartitioned || partitions < 2
	invalidSchema := len(layouts) != len(slots) || len(node.Delivered.Ordering) == 0
	invalidBatch := env.Limits.BatchRows <= 0 || uint64(env.Limits.BatchRows) > math.MaxUint32 || env.Limits.BatchBytes == 0
	if invalidDistribution || invalidSchema || invalidBatch {
		return nil, ErrInvalid
	}
	capped := node.Algorithm == physical.TopK
	if node.Algorithm != physical.Sort && !capped {
		return nil, ErrInvalid
	}
	if capped {
		limit := node.Limit
		invalidCap := limit.Scope != physical.PartitionLimit || limit.RuntimeBounds || limit.Offset != 0 || limit.WithTies
		missingProof := limit.Global == 0 || c.plan == nil
		if invalidCap || missingProof {
			return nil, ErrInvalid
		}
		// Only an immutable, validated plan can prove the ancestor path and its
		// exclusive ownership; a runtime NodeSpec alone cannot justify pruning.
		bound, ok := c.plan.Node(c.current)
		if !ok || bound.Metadata().Algorithm != physical.TopK || bound.Metadata().Limit != limit || bound.Delivered().OrderingCount() != uint32(len(node.Delivered.Ordering)) || !partitionSortOrder(bound.Delivered(), node.Delivered.Ordering) {
			return nil, ErrInvalid
		}
		kind, count, _, _ := bound.Delivered().Distribution()
		if kind != physical.HashPartitioned || count != partitions {
			return nil, ErrInvalid
		}
	}
	width := uint64(len(layouts))
	for _, allocation := range [...]struct{ count, bytes uint64 }{
		{1, uint64(unsafe.Sizeof(partitionTagInput{})) + uint64(unsafe.Sizeof(partitionSortCursor{}))},
		{width + 1, uint64(unsafe.Sizeof(model.TypeLayout{})) + 4*uint64(unsafe.Sizeof(columnar.Vector{}))},
		{uint64(len(node.Delivered.Ordering)), uint64(unsafe.Sizeof(exsort.Key{}))},
		{uint64(env.Limits.BatchRows), uint64(unsafe.Sizeof(uint32(0)))},
	} {
		if allocation.count > math.MaxUint64/allocation.bytes {
			return nil, columnar.ErrBatchBudget
		}
		if err := c.charge(allocation.count * allocation.bytes); err != nil {
			return nil, err
		}
	}
	if capped {
		if err := c.charge(uint64(partitions) * uint64(unsafe.Sizeof(uint64(0)))); err != nil {
			return nil, err
		}
	}
	keys, columns, err := c.orderColumns(node.Delivered.Ordering, slots, layouts)
	if err != nil {
		return nil, err
	}
	options := env.Sort
	options.Query, options.Encoder = env.Query, env.Keys
	options.BatchRows, options.BatchBytes = env.Limits.BatchRows, env.Limits.BatchBytes
	options.Layouts = make([]model.TypeLayout, len(layouts)+1)
	options.Layouts[0] = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	copy(options.Layouts[1:], layouts)
	options.Keys = make([]exsort.Key, len(keys))
	for i, key := range keys {
		options.Keys[i] = exsort.Key{Column: columns[i] + 1, Descending: key.Descending, NullsFirst: key.NullsFirst, InputCollation: node.Delivered.Ordering[i].Key.Collation}
	}
	if c.preparing {
		return nil, nil
	}
	tagged, ok := input.(partitionedCursor)
	if !ok {
		return nil, ErrInvalid
	}
	result := &partitionSortCursor{ctx: c.portal.ctx, query: env.Query, batchRows: options.BatchRows, partitions: partitions, capped: capped, count: node.Limit.Count}
	if capped && result.count == 0 {
		owned := input
		input = nil
		if err := owned.Close(); err != nil {
			return nil, err
		}
		return result, nil
	}
	transport := &partitionTagInput{input: tagged, partitions: partitions, batchRows: options.BatchRows, columns: len(layouts)}
	transport.output = make([]columnar.Vector, len(layouts)+1)
	transport.batch.Columns = make([]columnar.Vector, 0, len(layouts))
	ordered, proven := tagged.(globallyOrderedPartitions)
	if proven && ordered.globallyOrdered() && len(node.Children) == 1 && c.plan != nil {
		child, exists := c.plan.Node(node.Children[0])
		proven = exists && partitionSortOrder(child.Delivered(), node.Delivered.Ordering)
	} else {
		proven = false
	}
	input = nil
	if proven {
		result.input = transport
	} else {
		// New owns transport (and therefore input) on every outcome.
		result.input, err = exsort.New(c.portal.ctx, transport, options)
	}
	if err != nil {
		return nil, err
	}
	result.output = make([]columnar.Vector, len(layouts))
	result.batch.Columns = make([]columnar.Vector, 0, len(layouts)+1)
	result.selection = make([]uint32, options.BatchRows)
	if capped {
		result.counts = make([]uint64, partitions)
	}
	return result, nil
}

func partitionSortOrder(delivered physical.PropertyView, order []physical.OrderKey) bool {
	if delivered.OrderingCount() < uint32(len(order)) {
		return false
	}
	for i, want := range order {
		key, _ := delivered.Ordering(uint32(i))
		if key != want {
			return false
		}
	}
	return true
}

type partitionTagInput struct {
	input      partitionedCursor
	batch      columnar.Batch
	output     []columnar.Vector
	tag        [1]int64
	partitions uint32
	batchRows  int
	columns    int
	closeErr   error
}

func (c *partitionTagInput) Next(ctx context.Context, out *columnar.Batch) error {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.input == nil {
		return io.EOF
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return errors.Join(err, c.Close())
		}
		c.batch.Reset()
		c.batch.MaxRows = c.batchRows
		if out.MaxRows > 0 {
			c.batch.MaxRows = min(c.batch.MaxRows, out.MaxRows)
		}
		if err := c.input.Next(ctx, &c.batch); err != nil {
			if closeErr := c.Close(); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		if err := c.batch.Validate(max(c.batchRows, c.batch.Length)); err != nil {
			return errors.Join(err, c.Close())
		}
		if c.batch.Rows() > c.batch.MaxRows {
			return errors.Join(columnar.ErrInvalidBatch, c.Close())
		}
		if c.batch.Rows() != 0 {
			break
		}
	}
	if len(c.batch.Columns) != c.columns {
		return errors.Join(columnar.ErrValueLayout, c.Close())
	}
	if c.input.partition() >= c.partitions {
		return errors.Join(ErrInvalid, c.Close())
	}
	c.tag[0] = int64(c.input.partition())
	c.output[0] = columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Encoding: columnar.Constant, Length: c.batch.Length, Int64: c.tag[:]}
	copy(c.output[1:], c.batch.Columns)
	out.Columns, out.Length, out.Selection = c.output, c.batch.Length, c.batch.Selection
	return nil
}

func (c *partitionTagInput) Close() error {
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	c.batch.Reset()
	c.batch.Columns = nil
	clear(c.output)
	c.output = nil
	return c.closeErr
}

type partitionSortCursor struct {
	input      storage.BatchCursor
	ctx        context.Context
	query      *runtime.Query
	batch      columnar.Batch
	output     []columnar.Vector
	selection  []uint32
	counts     []uint64
	count      uint64
	position   int
	batchRows  int
	partitions uint32
	tag        uint32
	capped     bool
	closed     bool
	closeErr   error
}

func (c *partitionSortCursor) partition() uint32     { return c.tag }
func (c *partitionSortCursor) globallyOrdered() bool { return true }

func (c *partitionSortCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.closed {
		return io.EOF
	}
	defer func() {
		if err != nil {
			if closeErr := c.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	limit := c.batchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for {
		if err := errors.Join(context.Cause(ctx), context.Cause(c.ctx), context.Cause(c.query.Context())); err != nil {
			return err
		}
		if c.input == nil {
			return io.EOF
		}
		if c.position == c.batch.Rows() {
			c.batch.Reset()
			c.batch.MaxRows = limit
			if err := c.input.Next(ctx, &c.batch); err != nil {
				return err
			}
			c.position = 0
			if err := c.batch.Validate(max(c.batchRows, c.batch.Length)); err != nil {
				return err
			}
			if c.batch.Rows() > c.batch.MaxRows {
				return columnar.ErrInvalidBatch
			}
			if len(c.batch.Columns) != len(c.output)+1 {
				return columnar.ErrValueLayout
			}
			if c.batch.Rows() == 0 {
				continue
			}
		}
		start := c.position
		tagVector := &c.batch.Columns[0]
		lane := c.batch.Lane(start)
		if tagVector.IsNull(lane) || tagVector.Type.Storage != model.TypeInt64 {
			return ErrInvalid
		}
		tag := tagVector.Int64[tagVector.ValueIndex(lane)]
		if tag < 0 || uint64(tag) >= uint64(c.partitions) {
			return ErrInvalid
		}
		end := start + 1
		for end < c.batch.Rows() && end-start < limit {
			lane = c.batch.Lane(end)
			if tagVector.IsNull(lane) || tagVector.Int64[tagVector.ValueIndex(lane)] != tag {
				break
			}
			end++
		}
		c.position = end
		if c.capped {
			remaining := c.count - c.counts[tag]
			end = start + int(min(uint64(end-start), remaining))
			c.counts[tag] += uint64(end - start)
		}
		if end == start {
			continue
		}
		for row := start; row < end; row++ {
			c.selection[row-start] = uint32(c.batch.Lane(row))
		}
		// The caller may Reset these headers. Pending sorted headers must stay
		// private until the last partition run in this batch has been consumed.
		copy(c.output, c.batch.Columns[1:])
		out.Columns, out.Length, out.Selection = c.output, c.batch.Length, c.selection[:end-start]
		c.tag = uint32(tag)
		return nil
	}
}

func (c *partitionSortCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	c.batch.Reset()
	c.batch.Columns = nil
	clear(c.output)
	c.output, c.selection, c.counts = nil, nil, nil
	c.ctx, c.query = nil, nil
	return c.closeErr
}
