package exchange

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

type MergeOptions struct {
	Sources     Options
	Keys        []externalsort.Key
	Encoder     storage.KeyEncoder
	MaxKeyBytes uint64
	WorkBytes   uint64
}

var ErrUnsortedInput = errors.New("ordered exchange input is not monotonic")

type orderedHead struct {
	batch columnar.Batch
	key   []byte
}

type orderedMerge struct {
	ctx           context.Context
	options       MergeOptions
	sources       *Sources
	heads         []orderedHead
	heap          []int
	keyLayout     []storage.KeyColumn
	keyValues     []model.Value
	row           []model.Value
	builder       *columnar.Builder
	output        columnar.Batch
	work          columnar.Allocation
	outputHeaders columnar.Allocation
	advance       int
	pending       bool
	initialized   bool
	closed        bool
	closeErr      error
}

func mergeResource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

// mergeWork includes both old and replacement keys. Numeric exponent expansion
// is independent of encoded size; the builtin codec caps it at 32768 digits.
func mergeWork(ctx context.Context, inputCount int, o MergeOptions) (uint64, error) {
	if err := CheckSupport(ctx, inputCount, o.Sources); err != nil {
		return 0, err
	}
	if o.Encoder == nil || o.MaxKeyBytes == 0 || o.WorkBytes == 0 || o.WorkBytes > uint64(math.MaxInt) || o.MaxKeyBytes > uint64(math.MaxInt) {
		return 0, runtime.ErrInvalidConfig
	}
	remaining := o.WorkBytes
	admit := func(count, size uint64) bool {
		if size != 0 && count > remaining/size {
			return false
		}
		remaining -= count * size
		return true
	}
	for _, allocation := range [...]struct{ count, bytes uint64 }{
		{1, uint64(unsafe.Sizeof(orderedMerge{}))},
		{uint64(inputCount), uint64(unsafe.Sizeof(orderedHead{})) + uint64(unsafe.Sizeof(int(0)))},
		{uint64(len(o.Keys)), uint64(unsafe.Sizeof(externalsort.Key{})) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))},
		{uint64(len(o.Sources.Layouts)), uint64(unsafe.Sizeof(model.Value{}))},
		{uint64(inputCount), 2 * o.MaxKeyBytes},
		{1, o.MaxKeyBytes},
		{8, o.Sources.BatchBytes},
	} {
		if !admit(allocation.count, allocation.bytes) {
			return 0, mergeResource("admit ordered exchange workspace", nil)
		}
	}
	for _, layout := range o.Sources.Layouts {
		if err := mergeLayoutBytes(ctx, layout, &remaining, 0); err != nil {
			return 0, err
		}
	}
	for _, key := range o.Keys {
		if key.Column < 0 || key.Column >= len(o.Sources.Layouts) {
			return 0, storage.ErrInvalidScanPlan
		}
		layout := o.Sources.Layouts[key.Column]
		keyBytes, err := storage.KeyLayoutBytes(ctx, []storage.KeyColumn{{Type: layout.Type, Children: layout.Children}}, remaining)
		if err != nil {
			return 0, err
		}
		remaining -= keyBytes
		if o.Sources.Layouts[key.Column].Type.ID == 1700 && !admit(1, 1<<20) {
			return 0, mergeResource("admit ordered exchange numeric workspace", nil)
		}
	}
	if !admit(uint64(inputCount), uint64(len(o.Sources.Layouts))*uint64(unsafe.Sizeof(columnar.Vector{}))) {
		return 0, mergeResource("admit ordered exchange input headers", nil)
	}
	return o.WorkBytes - remaining, nil
}

func mergeLayoutBytes(ctx context.Context, layout model.TypeLayout, remaining *uint64, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return model.ErrInvalidTypeLayout
	}
	size := uint64(unsafe.Sizeof(model.TypeLayout{}))
	if *remaining < size {
		return mergeResource("admit ordered exchange schema", nil)
	}
	*remaining -= size
	nameHeaders := uint64(len(layout.FieldNames)) * uint64(unsafe.Sizeof(""))
	if nameHeaders > *remaining {
		return mergeResource("admit ordered exchange schema", nil)
	}
	*remaining -= nameHeaders
	for _, name := range layout.FieldNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uint64(len(name)) > *remaining {
			return mergeResource("admit ordered exchange schema", nil)
		}
		*remaining -= uint64(len(name))
	}
	for _, child := range layout.Children {
		if err := mergeLayoutBytes(ctx, child, remaining, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func mergeKeyLayout(o MergeOptions) []storage.KeyColumn {
	layout := make([]storage.KeyColumn, len(o.Keys))
	for i, key := range o.Keys {
		typ := o.Sources.Layouts[key.Column].Type
		if key.InputCollation != 0 && builtin.TypeCollatable(typ) {
			typ.Collation = key.InputCollation
		}
		layout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: typ, Children: o.Sources.Layouts[key.Column].Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	return layout
}

// CheckMergeSupport validates and admits metadata without consuming any input.
func CheckMergeSupport(ctx context.Context, inputCount int, options MergeOptions) error {
	work, err := mergeWork(ctx, inputCount, options)
	if err != nil {
		return err
	}
	claim, err := options.Sources.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return err
	}
	defer claim.Release()
	_, err = options.Encoder.EncodePrefix(ctx, mergeKeyLayout(options), nil, options.MaxKeyBytes)
	return err
}

// NewOrderedMerge owns every input on every outcome. Query and Encoder remain
// borrowed. Next and Close must be serialized, as for other execution cursors.
func NewOrderedMerge(ctx context.Context, inputs []storage.BatchCursor, options MergeOptions) (storage.BatchCursor, error) {
	work, err := mergeWork(ctx, len(inputs), options)
	if err != nil {
		return nil, errors.Join(err, closeMergeInputs(inputs))
	}
	claim, err := options.Sources.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return nil, errors.Join(err, closeMergeInputs(inputs))
	}
	c := &orderedMerge{ctx: ctx, options: options, work: claim, advance: -1}
	c.keyLayout, _, err = storage.CopyKeyLayout(ctx, mergeKeyLayout(options), work)
	if err != nil {
		return nil, errors.Join(err, closeMergeInputs(inputs), c.Close())
	}
	if _, err := options.Encoder.EncodePrefix(ctx, c.keyLayout, nil, options.MaxKeyBytes); err != nil {
		return nil, errors.Join(err, closeMergeInputs(inputs), c.Close())
	}
	c.options.Sources.Layouts = make([]model.TypeLayout, len(options.Sources.Layouts))
	remaining := work
	for i, layout := range options.Sources.Layouts {
		owned, size, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return nil, errors.Join(err, closeMergeInputs(inputs), c.Close())
		}
		remaining -= size
		c.options.Sources.Layouts[i] = owned
	}
	c.options.Keys = make([]externalsort.Key, len(options.Keys))
	copy(c.options.Keys, options.Keys)
	c.heads = make([]orderedHead, len(inputs))
	for i := range c.heads {
		c.heads[i].batch = columnar.Batch{Columns: make([]columnar.Vector, 0, len(options.Sources.Layouts)), MaxRows: 1}
	}
	c.heap = make([]int, 0, len(inputs))
	c.row = make([]model.Value, len(options.Sources.Layouts))
	c.keyValues = make([]model.Value, len(options.Keys))
	c.sources, err = NewSources(ctx, inputs, c.options.Sources)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	headerBytes := uint64(len(options.Sources.Layouts))*uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(columnar.Builder{}))
	c.outputHeaders, err = options.Sources.Query.Allocator(runtime.OutputMemory).Reserve(headerBytes)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.output.Columns = make([]columnar.Vector, 0, len(options.Sources.Layouts))
	c.builder, err = columnar.NewBuilder(c.options.Sources.Layouts, columnar.BuilderOptions{MaxRows: options.Sources.BatchRows, MaxBytes: options.Sources.BatchBytes, Allocator: options.Sources.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func closeMergeInputs(inputs []storage.BatchCursor) error {
	var err error
	for _, input := range inputs {
		if input != nil {
			err = errors.Join(err, input.Close())
		}
	}
	return err
}

func (c *orderedMerge) load(ctx context.Context, index int) error {
	head := &c.heads[index]
	for {
		head.batch.Reset()
		err := c.sources.Next(ctx, index, &head.batch)
		if err == io.EOF {
			head.key = nil
			return io.EOF
		}
		if err != nil {
			return err
		}
		if head.batch.Rows() == 0 {
			continue
		}
		if head.batch.Rows() != 1 || len(head.batch.Columns) != len(c.row) {
			return columnar.ErrInvalidBatch
		}
		break
	}
	remaining := c.options.Sources.BatchBytes * 8
	defer clear(c.keyValues)
	for i, key := range c.options.Keys {
		value, used, err := columnar.CopyValue(ctx, &head.batch.Columns[key.Column], head.batch.Lane(0), remaining)
		if err != nil {
			return err
		}
		remaining -= used
		c.keyValues[i] = value
	}
	key, err := c.options.Encoder.EncodePrefix(ctx, c.keyLayout, c.keyValues, c.options.MaxKeyBytes)
	if err != nil {
		return err
	}
	if uint64(cap(key)) > c.options.MaxKeyBytes {
		return mergeResource("ordered exchange key exceeds bound", nil)
	}
	if head.key != nil && bytes.Compare(head.key, key) > 0 {
		return fmt.Errorf("input %d: %w", index, ErrUnsortedInput)
	}
	head.key = key
	return nil
}

func (c *orderedMerge) less(a, b int) bool {
	order := bytes.Compare(c.heads[a].key, c.heads[b].key)
	return order < 0 || order == 0 && a < b
}

func (c *orderedMerge) push(index int) {
	c.heap = append(c.heap, index)
	child := len(c.heap) - 1
	for child > 0 {
		parent := (child - 1) / 2
		if !c.less(c.heap[child], c.heap[parent]) {
			break
		}
		c.heap[child], c.heap[parent] = c.heap[parent], c.heap[child]
		child = parent
	}
}

func (c *orderedMerge) pop() int {
	index := c.heap[0]
	last := len(c.heap) - 1
	c.heap[0] = c.heap[last]
	c.heap = c.heap[:last]
	parent := 0
	for child := 1; child < len(c.heap); child = parent*2 + 1 {
		if child+1 < len(c.heap) && c.less(c.heap[child+1], c.heap[child]) {
			child++
		}
		if !c.less(c.heap[child], c.heap[parent]) {
			break
		}
		c.heap[child], c.heap[parent] = c.heap[parent], c.heap[child]
		parent = child
	}
	return index
}

func (c *orderedMerge) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if c.closed {
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil && err != io.EOF {
			err = errors.Join(err, c.Close())
		}
	}()
	if out == nil || out.MaxRows < 0 {
		return columnar.ErrInvalidBatch
	}
	out.Reset()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := context.Cause(c.options.Sources.Query.Context()); err != nil {
		return err
	}
	if !c.initialized {
		for i := range c.heads {
			if err := c.load(ctx, i); err != nil {
				if err == io.EOF {
					continue
				}
				return err
			}
			c.push(i)
		}
		c.initialized = true
	}
	c.builder.Reset()
	limit := c.options.Sources.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.builder.Rows() < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := context.Cause(c.ctx); err != nil {
			return err
		}
		if err := context.Cause(c.options.Sources.Query.Context()); err != nil {
			return err
		}
		// Advance only when another output row is demanded.
		if c.advance >= 0 {
			index := c.advance
			c.advance = -1
			if err := c.load(ctx, index); err != nil {
				if err != io.EOF {
					return err
				}
			} else {
				c.push(index)
			}
		}
		if len(c.heap) == 0 {
			break
		}
		if !c.pending {
			head := &c.heads[c.heap[0]]
			remaining := c.options.Sources.BatchBytes * 8
			for i := range c.row {
				value, used, err := columnar.CopyValue(ctx, &head.batch.Columns[i], head.batch.Lane(0), remaining)
				if err != nil {
					return err
				}
				remaining -= used
				c.row[i] = value
			}
			c.pending = true
		}
		if err := c.builder.Append(ctx, c.row); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		clear(c.row)
		c.pending = false
		c.advance = c.pop()
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	if err := c.builder.Publish(&c.output); err != nil {
		return err
	}
	out.Columns, out.Length = c.output.Columns, c.output.Length
	return nil
}

func (c *orderedMerge) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.sources != nil {
		c.closeErr = errors.Join(c.closeErr, c.sources.Close())
		c.sources = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.heads, c.heap, c.keyLayout, c.keyValues, c.row = nil, nil, nil, nil, nil
	c.options.Keys, c.options.Sources.Layouts = nil, nil
	c.output = columnar.Batch{}
	if c.outputHeaders != nil {
		c.outputHeaders.Release()
		c.outputHeaders = nil
	}
	if c.work != nil {
		c.work.Release()
		c.work = nil
	}
	return c.closeErr
}
