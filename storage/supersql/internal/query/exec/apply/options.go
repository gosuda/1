// Package apply executes parameterized scalar, existence and lateral relations.
package apply

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type Side uint8

const (
	Left Side = iota + 1
	Right
	Mark
)

type Column struct {
	Side  Side
	Index int
}

// RightFactory borrows the immutable left row until its returned cursor closes.
// Open must honor cancellation. Returned cursor ownership transfers on every
// outcome, including an error accompanied by a nonnil cursor.
type RightFactory interface {
	Open(context.Context, []model.Value) (storage.BatchCursor, error)
}

type Options struct {
	Kind                                     ir.ApplyKind
	Outer                                    bool
	LeftLayouts, RightLayouts, OutputLayouts []model.TypeLayout
	Output                                   []Column
	Factory                                  RightFactory
	Query                                    *runtime.Query
	BatchRows                                int
	BatchBytes, MaxRowBytes, WorkBytes       uint64
}

// New transfers input ownership on all outcomes and borrows Query and Factory.
// Next and Close require serialization. Input/output batches are independently
// budgeted; WorkBytes covers owned captures, scalar results and metadata.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{input: input, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missing := c.input == nil || o.Factory == nil || o.Query == nil
	invalidLimits := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRowBytes == 0 || o.WorkBytes == 0
	if missing || invalidLimits {
		return runtime.ErrInvalidConfig
	}
	if o.MaxRowBytes > uint64(math.MaxInt)/4 || o.WorkBytes > uint64(math.MaxInt) {
		return runtime.ErrInvalidConfig
	}
	invalidKind := o.Kind < ir.ScalarApply || o.Kind > ir.LateralApply
	invalidOuter := o.Outer && o.Kind != ir.LateralApply
	if invalidKind || invalidOuter {
		return runtime.ErrInvalidConfig
	}
	if o.Kind == ir.ScalarApply && len(o.RightLayouts) != 1 {
		return columnar.ErrValueLayout
	}
	if len(o.Output) != len(o.OutputLayouts) {
		return columnar.ErrValueLayout
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return err
	}
	count := uint64(len(o.LeftLayouts) + len(o.RightLayouts) + len(o.OutputLayouts))
	unit := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(Column{}))
	base := uint64(unsafe.Sizeof(cursor{})) + 2*o.MaxRowBytes
	if base > o.WorkBytes || count > (o.WorkBytes-base)/unit {
		return runtime.ErrBudget
	}
	metadata := o.WorkBytes - base - count*unit
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(o.WorkBytes)
	if err != nil {
		return err
	}
	c.claim = claim
	copyLayouts := func(layouts []model.TypeLayout) ([]model.TypeLayout, error) {
		owned := make([]model.TypeLayout, len(layouts))
		for i, layout := range layouts {
			value, used, err := model.CopyTypeLayout(ctx, layout, metadata)
			if err != nil {
				return nil, err
			}
			owned[i], metadata = value, metadata-used
		}
		return owned, nil
	}
	if o.LeftLayouts, err = copyLayouts(o.LeftLayouts); err != nil {
		return err
	}
	if o.RightLayouts, err = copyLayouts(o.RightLayouts); err != nil {
		return err
	}
	if o.OutputLayouts, err = copyLayouts(o.OutputLayouts); err != nil {
		return err
	}
	for i, output := range o.Output {
		var layout model.TypeLayout
		switch output.Side {
		case Left:
			if output.Index < 0 || output.Index >= len(o.LeftLayouts) {
				return columnar.ErrValueLayout
			}
			layout = o.LeftLayouts[output.Index]
		case Right:
			if o.Kind == ir.ExistsApply || output.Index < 0 || output.Index >= len(o.RightLayouts) {
				return columnar.ErrValueLayout
			}
			layout = o.RightLayouts[output.Index]
		case Mark:
			if o.Kind != ir.ExistsApply || output.Index != 0 {
				return columnar.ErrValueLayout
			}
			layout = o.OutputLayouts[i]
			if layout.Type.ID != 16 || layout.Type.Storage != model.TypeBoolean || len(layout.Children) != 0 {
				return columnar.ErrValueLayout
			}
		default:
			return columnar.ErrValueLayout
		}
		if !model.TypeLayoutsEqual(layout, o.OutputLayouts[i]) {
			return columnar.ErrValueLayout
		}
	}
	o.Output = append([]Column(nil), o.Output...)
	c.leftValues = make([]model.Value, len(o.LeftLayouts))
	c.rightValues = make([]model.Value, len(o.RightLayouts))
	c.output = make([]model.Value, len(o.Output))
	c.leftBatch.Columns = make([]columnar.Vector, 0, len(o.LeftLayouts))
	c.rightBatch.Columns = make([]columnar.Vector, 0, len(o.RightLayouts))
	c.builder, err = columnar.NewBuilder(o.OutputLayouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	return nil
}
