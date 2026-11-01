package columnar

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrBatchBudget = errors.New("columnar batch exceeds memory budget")
	ErrValueLayout = errors.New("datum does not match bound vector layout")
)

// Builder owns contiguous vector headers and typed payload buffers. Publish
// copies only root headers; Append/Reset invalidate its borrowed payloads.
// Failed Append restores all lengths, so callers can flush preceding rows.
// Close drops ownership without mutating already published nested headers.
type Builder struct {
	nodes      []Vector
	marks      []vectorMark
	columns    int
	rows       int
	maxRows    int
	limit      uint64
	allocated  uint64
	claim      Allocation
	operations uint64
	closed     bool
}

type vectorMark struct {
	length     int
	nulls      int
	integers   int
	floats     int
	data       int
	ranges     int
	float32s   int
	intervals  int
	shapes     int
	dimensions int
}

func NewBuilder(layouts []model.TypeLayout, options BuilderOptions) (*Builder, error) {
	maxRows, maxBytes := options.MaxRows, options.MaxBytes
	if maxRows <= 0 || maxBytes == 0 {
		return nil, ErrBatchBudget
	}
	count := uint64(0)
	for _, layout := range layouts {
		if err := countLayoutNodes(layout, &count, maxBytes, 0); err != nil {
			return nil, err
		}
		if err := layout.Validate(); err != nil {
			return nil, err
		}
	}
	cost := uint64(unsafe.Sizeof(Vector{})) + uint64(unsafe.Sizeof(vectorMark{}))
	if count > maxBytes/cost || count > uint64(math.MaxInt) {
		return nil, ErrBatchBudget
	}
	var claim Allocation
	if options.Allocator != nil {
		var err error
		claim, err = options.Allocator.Reserve(count * cost)
		if err != nil {
			return nil, err
		}
	}
	b := &Builder{nodes: make([]Vector, int(count)), marks: make([]vectorMark, int(count)), columns: len(layouts), maxRows: maxRows, limit: maxBytes, allocated: count * cost, claim: claim}
	next := len(layouts)
	for i, layout := range layouts {
		b.initialize(&b.nodes[i], layout, &next)
	}
	return b, nil
}

func countLayoutNodes(layout model.TypeLayout, count *uint64, budget uint64, depth int) error {
	if depth > 64 {
		return model.ErrInvalidTypeLayout
	}
	cost := uint64(unsafe.Sizeof(Vector{})) + uint64(unsafe.Sizeof(vectorMark{}))
	if *count >= budget/cost {
		return ErrBatchBudget
	}
	*count++
	for _, child := range layout.Children {
		if err := countLayoutNodes(child, count, budget, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) initialize(v *Vector, layout model.TypeLayout, next *int) {
	v.Type = layout.Type
	if len(layout.Children) == 0 {
		return
	}
	start := *next
	*next += len(layout.Children)
	v.Children = b.nodes[start:*next]
	for i, child := range layout.Children {
		b.initialize(&v.Children[i], child, next)
	}
}

func (b *Builder) Rows() int { return b.rows }

// AllocatedBytes counts retained header/payload buffers, not the Builder itself
// or the caller-owned headers used by Publish.
func (b *Builder) AllocatedBytes() uint64 { return b.allocated }

func (b *Builder) Append(ctx context.Context, values []model.Value) error {
	if b.closed {
		return ErrInvalidBatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(values) != b.columns {
		return ErrValueLayout
	}
	if b.rows == b.maxRows {
		return ErrBatchBudget
	}
	b.saveMarks()
	for i, value := range values {
		if err := b.appendValue(ctx, &b.nodes[i], value); err != nil {
			b.restoreMarks()
			return err
		}
	}
	b.rows++
	return nil
}

func (b *Builder) Publish(out *Batch) error {
	if b.closed || out == nil || out.MaxRows < 0 {
		return ErrInvalidBatch
	}
	if out.MaxRows > 0 && b.rows > out.MaxRows {
		return ErrBatchBudget
	}
	out.Reset()
	if cap(out.Columns) < b.columns {
		out.Columns = make([]Vector, b.columns)
	} else {
		out.Columns = out.Columns[:b.columns]
	}
	copy(out.Columns, b.nodes[:b.columns])
	out.Length = b.rows
	return nil
}

func (b *Builder) Reset() {
	for i := range b.nodes {
		v := &b.nodes[i]
		v.Length = 0
		v.Nulls = v.Nulls[:0]
		v.Int64 = v.Int64[:0]
		v.Float64 = v.Float64[:0]
		v.Data = v.Data[:0]
		v.Ranges = v.Ranges[:0]
		v.Float32 = v.Float32[:0]
		v.Intervals = v.Intervals[:0]
		v.ShapeRanges = v.ShapeRanges[:0]
		v.Dimensions = v.Dimensions[:0]
	}
	b.rows = 0
}

func (b *Builder) Close() error {
	b.nodes = nil
	b.marks = nil
	if b.claim != nil {
		b.claim.Release()
		b.claim = nil
	}
	b.rows = 0
	b.allocated = 0
	b.closed = true
	return nil
}

func (b *Builder) saveMarks() {
	for i := range b.nodes {
		v := &b.nodes[i]
		b.marks[i] = vectorMark{v.Length, len(v.Nulls), len(v.Int64), len(v.Float64), len(v.Data), len(v.Ranges), len(v.Float32), len(v.Intervals), len(v.ShapeRanges), len(v.Dimensions)}
	}
}

func (b *Builder) restoreMarks() {
	for i, m := range b.marks {
		v := &b.nodes[i]
		v.Length = m.length
		v.Nulls = v.Nulls[:m.nulls]
		v.Int64 = v.Int64[:m.integers]
		v.Float64 = v.Float64[:m.floats]
		v.Data = v.Data[:m.data]
		v.Ranges = v.Ranges[:m.ranges]
		v.Float32 = v.Float32[:m.float32s]
		v.Intervals = v.Intervals[:m.intervals]
		v.ShapeRanges = v.ShapeRanges[:m.shapes]
		v.Dimensions = v.Dimensions[:m.dimensions]
	}
}

func reserve[T any](b *Builder, target *[]T, additional int) error {
	values := *target
	if additional < 0 || additional > math.MaxInt-len(values) {
		return ErrBatchBudget
	}
	need := len(values) + additional
	if need <= cap(values) {
		*target = values[:need]
		return nil
	}
	var element T
	size := uint64(unsafe.Sizeof(element))
	available := (b.limit - b.allocated) / size
	if uint64(need) > available {
		return ErrBatchBudget
	}
	capacity := max(uint64(need), min(uint64(max(8, cap(values)))*2, available))
	if capacity > uint64(math.MaxInt) {
		return ErrBatchBudget
	}
	peak := b.allocated + capacity*size
	if b.claim != nil {
		if err := b.claim.Resize(peak); err != nil {
			return err
		}
	}
	grown := make([]T, need, int(capacity))
	copy(grown, values)
	oldBytes := uint64(cap(values)) * size
	*target = grown
	values = nil
	b.allocated = peak - oldBytes
	if b.claim != nil {
		return b.claim.Resize(b.allocated)
	}
	return nil
}

func (b *Builder) appendValue(ctx context.Context, v *Vector, value model.Value) error {
	if value.Type != v.Type.Storage {
		return ErrValueLayout
	}
	if err := b.appendNull(ctx, v, value.IsNull); err != nil {
		return err
	}
	if err := b.appendPayload(ctx, v, value); err != nil {
		return err
	}
	v.Length++
	return nil
}

func (b *Builder) appendNull(ctx context.Context, v *Vector, null bool) error {
	b.operations++
	if b.operations&1023 == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := reserve(b, &v.Nulls, (v.Length+8)/8-len(v.Nulls)); err != nil {
		return err
	}
	mask := byte(1 << uint(v.Length%8))
	v.Nulls[v.Length/8] &^= mask
	if null {
		v.Nulls[v.Length/8] |= mask
	}
	return nil
}

func (b *Builder) appendPayload(ctx context.Context, v *Vector, value model.Value) error {
	var err error
	switch v.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		if !value.IsNull && value.Type == model.TypeBoolean && value.IntVal != 0 && value.IntVal != 1 {
			return ErrValueLayout
		}
		err = appendScalar(b, &v.Int64, value.IntVal, value.IsNull)
	case model.TypeFloat64:
		err = appendScalar(b, &v.Float64, value.FloatVal, value.IsNull)
	case model.TypeInterval:
		err = appendScalar(b, &v.Intervals, value.IntervalVal, value.IsNull)
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		return b.appendBytes(v, value)
	case model.TypeVectorF32:
		return b.appendFloat32(v, value)
	case model.TypeArray:
		return b.appendArray(ctx, v, value)
	case model.TypeComposite:
		if !value.IsNull && len(value.ArrayVal) != len(v.Children) {
			return ErrValueLayout
		}
		for i := range v.Children {
			field := model.NullVal(v.Children[i].Type.Storage)
			if !value.IsNull {
				field = value.ArrayVal[i]
			}
			if err := b.appendValue(ctx, &v.Children[i], field); err != nil {
				return err
			}
		}
	default:
		return ErrValueLayout
	}
	return err
}

func appendScalar[T any](b *Builder, values *[]T, value T, null bool) error {
	index := len(*values)
	if err := reserve(b, values, 1); err != nil {
		return err
	}
	var zero T
	if null {
		value = zero
	}
	(*values)[index] = value
	return nil
}

func (b *Builder) appendBytes(v *Vector, value model.Value) error {
	size := len(value.StrVal)
	if value.Type == model.TypeBytes {
		size = len(value.BytesVal)
	}
	if value.IsNull {
		size = 0
	}
	if uint64(len(v.Data))+uint64(size) > math.MaxUint32 {
		return ErrBatchBudget
	}
	start := len(v.Data)
	if err := reserve(b, &v.Data, size); err != nil {
		return err
	}
	if !value.IsNull {
		if value.Type == model.TypeBytes {
			copy(v.Data[start:], value.BytesVal)
		} else {
			copy(v.Data[start:], value.StrVal)
		}
	}
	return appendScalar(b, &v.Ranges, Range{Offset: uint32(start), Length: uint32(size)}, false)
}

func (b *Builder) appendFloat32(v *Vector, value model.Value) error {
	count := len(value.VectorVal)
	if value.IsNull {
		count = 0
	}
	start := len(v.Float32)
	if uint64(start)+uint64(count) > math.MaxUint32 {
		return ErrBatchBudget
	}
	if err := reserve(b, &v.Float32, count); err != nil {
		return err
	}
	if !value.IsNull {
		copy(v.Float32[start:], value.VectorVal)
	}
	return appendScalar(b, &v.Ranges, Range{Offset: uint32(start), Length: uint32(count)}, false)
}

func (b *Builder) appendArray(ctx context.Context, v *Vector, value model.Value) error {
	count, dimensions := len(value.ArrayVal), len(value.Dimensions)
	if value.IsNull {
		count, dimensions = 0, 0
	} else {
		if err := model.ValidateArrayShape(value.Dimensions, uint64(count)); err != nil {
			return err
		}
	}
	childStart, shapeStart := v.Children[0].Length, len(v.Dimensions)
	if uint64(childStart)+uint64(count) > math.MaxUint32 {
		return ErrBatchBudget
	}
	if uint64(shapeStart)+uint64(dimensions) > math.MaxUint32 {
		return ErrBatchBudget
	}
	if err := reserve(b, &v.Dimensions, dimensions); err != nil {
		return err
	}
	if !value.IsNull {
		copy(v.Dimensions[shapeStart:], value.Dimensions)
	}
	if err := appendScalar(b, &v.ShapeRanges, Range{Offset: uint32(shapeStart), Length: uint32(dimensions)}, false); err != nil {
		return err
	}
	if err := appendScalar(b, &v.Ranges, Range{Offset: uint32(childStart), Length: uint32(count)}, false); err != nil {
		return err
	}
	for _, element := range value.ArrayVal[:count] {
		if err := b.appendValue(ctx, &v.Children[0], element); err != nil {
			return err
		}
	}
	return nil
}
