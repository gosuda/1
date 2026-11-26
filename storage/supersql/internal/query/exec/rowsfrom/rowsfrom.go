// Package rowsfrom zips table-function streams with typed NULL padding.
package rowsfrom

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
)

type Input struct {
	Cursor  storage.BatchCursor
	Layouts []model.TypeLayout
}

type Options struct {
	Output     []model.TypeLayout
	Ordinality bool
	Query      *runtime.Query
	BatchRows  int
	BatchBytes uint64
}

type inputState struct {
	input   Input
	batch   columnar.Batch
	row     int
	columns int
}

type cursor struct {
	options       Options
	life          context.Context
	inputs        []inputState
	vectors       []*columnar.Vector
	lanes         []int
	ordinal       [1]int64
	ordinalVector columnar.Vector
	position      int64
	builder       *columnar.Builder
	output        columnar.Batch
	claim         columnar.Allocation
	closed        bool
	eof           bool
	closeErr      error
}

var errOrdinalityOverflow = &model.Error{Code: "22003", Message: "bigint out of range"}

// New owns every input cursor on every outcome, without pulling it. Layouts and
// Query are borrowed immutable until Close. ctx governs the cursor lifetime;
// Next contexts govern only that call. Next and Close must be serialized.
func New(ctx context.Context, inputs []Input, options Options) (_ storage.BatchCursor, err error) {
	var c *cursor
	defer func() {
		if err != nil {
			err = errors.Join(err, context.Cause(ctx))
			if options.Query != nil {
				err = errors.Join(err, context.Cause(options.Query.Context()))
			}
			if c != nil {
				err = errors.Join(err, c.Close())
			} else {
				err = errors.Join(err, closeInputCursors(inputs))
			}
		}
	}()
	if options.Query == nil || len(inputs) < 2 || options.BatchRows <= 0 || options.BatchBytes == 0 || options.BatchBytes > uint64(math.MaxInt) {
		return nil, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(options.Query.Context())); err != nil {
		return nil, err
	}
	call, cancel := context.WithCancelCause(ctx)
	query := options.Query.Context()
	stopQuery := context.AfterFunc(query, func() { cancel(context.Cause(query)) })
	defer func() { stopQuery(); cancel(nil) }()
	bytes, columns, err := metadata(call, inputs, options)
	if err != nil {
		return nil, err
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(bytes)
	if err != nil {
		return nil, err
	}
	c = &cursor{options: options, life: ctx, claim: claim}
	c.inputs = make([]inputState, len(inputs))
	headers := make([]columnar.Vector, columns)
	offset := 0
	for i, input := range inputs {
		end := offset + len(input.Layouts)
		c.inputs[i] = inputState{input: input, columns: len(input.Layouts), batch: columnar.Batch{Columns: headers[offset:offset:end]}}
		offset = end
	}
	c.vectors = make([]*columnar.Vector, len(options.Output))
	c.lanes = make([]int, len(options.Output))
	c.output.Columns = make([]columnar.Vector, 0, len(options.Output))
	if options.Ordinality {
		c.ordinalVector = columnar.Vector{Type: options.Output[len(options.Output)-1].Type, Length: 1, Int64: c.ordinal[:]}
	}
	c.builder, err = columnar.NewBuilder(options.Output, columnar.BuilderOptions{
		MaxRows: options.BatchRows, MaxBytes: options.BatchBytes,
		Allocator: options.Query.Allocator(runtime.OutputMemory),
	})
	if err != nil {
		return nil, err
	}
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func closeInputCursors(inputs []Input) error {
	var err error
	for _, input := range inputs {
		if input.Cursor != nil {
			err = errors.Join(err, input.Cursor.Close())
		}
	}
	return err
}

func metadata(ctx context.Context, inputs []Input, options Options) (uint64, int, error) {
	limit := uint64(options.Query.Usage().Work.Limit)
	remaining := limit
	admit := func(count, size uint64) bool {
		if size != 0 && count > remaining/size {
			return false
		}
		remaining -= count * size
		return true
	}
	if !admit(1, uint64(unsafe.Sizeof(cursor{}))+uint64(unsafe.Sizeof(columnar.Builder{}))) ||
		!admit(uint64(len(inputs)), uint64(unsafe.Sizeof(inputState{}))) ||
		!admit(uint64(len(options.Output)), uint64(unsafe.Sizeof((*columnar.Vector)(nil)))+uint64(unsafe.Sizeof(int(0)))+uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(model.TypeLayout{}))) {
		return 0, 0, runtime.ErrBudget
	}
	columns := len(options.Output)
	if options.Ordinality {
		columns--
		if columns < 0 || options.Output[columns].Type != (model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}) || len(options.Output[columns].Children) != 0 {
			return 0, 0, columnar.ErrValueLayout
		}
	}
	for _, layout := range options.Output {
		size, err := model.TypeLayoutBytes(ctx, layout, remaining)
		if err != nil {
			return 0, 0, err
		}
		remaining -= size
	}
	offset := 0
	for _, input := range inputs {
		if err := context.Cause(ctx); err != nil {
			return 0, 0, err
		}
		if input.Cursor == nil {
			return 0, 0, runtime.ErrInvalidConfig
		}
		if len(input.Layouts) > columns-offset {
			return 0, 0, columnar.ErrValueLayout
		}
		if !admit(uint64(len(input.Layouts)), uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(model.TypeLayout{}))) {
			return 0, 0, runtime.ErrBudget
		}
		for i, layout := range input.Layouts {
			size, err := model.TypeLayoutBytes(ctx, layout, remaining)
			if err != nil {
				return 0, 0, err
			}
			remaining -= size
			if err := sameLayout(ctx, layout, options.Output[offset+i]); err != nil {
				return 0, 0, err
			}
		}
		offset += len(input.Layouts)
	}
	if offset != columns {
		return 0, 0, columnar.ErrValueLayout
	}
	return limit - remaining, columns, nil
}

// Both descriptors have passed bounded-depth validation before comparison.
func sameLayout(ctx context.Context, actual, expected model.TypeLayout) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if actual.Type != expected.Type || len(actual.Children) != len(expected.Children) {
		return columnar.ErrValueLayout
	}
	for i := range actual.Children {
		if err := sameLayout(ctx, actual.Children[i], expected.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

func vectorLayout(ctx context.Context, vector *columnar.Vector, layout model.TypeLayout) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if vector.Type != layout.Type || len(vector.Children) != len(layout.Children) {
		return columnar.ErrValueLayout
	}
	for i := range layout.Children {
		if err := vectorLayout(ctx, &vector.Children[i], layout.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

func (input *inputState) close() error {
	var err error
	input.batch.Reset()
	if input.input.Cursor != nil {
		err = input.input.Cursor.Close()
	}
	*input = inputState{columns: input.columns}
	return err
}

func (input *inputState) load(ctx context.Context, demand int) error {
	for input.input.Cursor != nil && input.row == input.batch.Rows() {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		input.batch.Reset()
		input.batch.MaxRows = demand
		input.row = 0
		err := input.input.Cursor.Next(ctx, &input.batch)
		if err == io.EOF {
			return input.close()
		}
		if err != nil {
			return err
		}
		// Selection lanes address physical Length, not the logical demand cap.
		if input.batch.Length < 0 || input.batch.Rows() > demand || len(input.batch.Columns) != len(input.input.Layouts) {
			return columnar.ErrInvalidBatch
		}
		for _, lane := range input.batch.Selection {
			if err := context.Cause(ctx); err != nil {
				return err
			}
			if uint64(lane) >= uint64(input.batch.Length) {
				return columnar.ErrInvalidBatch
			}
		}
		for i := range input.batch.Columns {
			vector := &input.batch.Columns[i]
			if vector.Length < input.batch.Length {
				return columnar.ErrInvalidBatch
			}
			if err := vectorLayout(ctx, vector, input.input.Layouts[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.life), context.Cause(c.options.Query.Context()))
}

func (c *cursor) prepare(ctx context.Context, demand int) (bool, error) {
	live, offset := false, 0
	for i := range c.inputs {
		input := &c.inputs[i]
		width := input.columns
		if err := input.load(ctx, demand); err != nil {
			return false, err
		}
		if input.input.Cursor != nil {
			live = true
			lane := input.batch.Lane(input.row)
			for j := range width {
				c.vectors[offset+j] = &input.batch.Columns[j]
				c.lanes[offset+j] = lane
			}
		}
		offset += width
	}
	if live && c.options.Ordinality {
		if c.position == math.MaxInt64 {
			return false, errOrdinalityOverflow
		}
		c.ordinal[0] = c.position + 1
		c.vectors[len(c.vectors)-1] = &c.ordinalVector
		c.lanes[len(c.lanes)-1] = 0
	}
	return live, nil
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		if out != nil {
			out.Reset()
		}
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
		clear(c.vectors)
		if err != nil {
			out.Reset()
			if cause := c.check(ctx); cause != nil {
				err = errors.Join(err, cause)
			}
			if cleanup := c.Close(); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
			c.eof = err == io.EOF
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	call, cancel := context.WithCancelCause(ctx)
	life, query := c.life, c.options.Query.Context()
	stopLife := context.AfterFunc(life, func() { cancel(context.Cause(life)) })
	stopQuery := context.AfterFunc(query, func() { cancel(context.Cause(query)) })
	defer func() { stopLife(); stopQuery(); cancel(nil) }()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.builder.Reset()
	for c.builder.Rows() < limit {
		if err := c.check(ctx); err != nil {
			return err
		}
		live, err := c.prepare(call, limit-c.builder.Rows())
		if err != nil {
			return err
		}
		if !live {
			break
		}
		err = c.builder.AppendVectors(call, c.vectors, c.lanes)
		clear(c.vectors)
		if err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		for i := range c.inputs {
			if c.inputs[i].input.Cursor != nil {
				c.inputs[i].row++
			}
		}
		if c.options.Ordinality {
			c.position++
		}
	}
	if c.builder.Rows() == 0 {
		return io.EOF
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	if err := c.builder.Publish(&c.output); err != nil {
		return err
	}
	out.Columns, out.Length = c.output.Columns, c.output.Length
	return nil
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	clear(c.vectors)
	for i := range c.inputs {
		c.closeErr = errors.Join(c.closeErr, c.inputs[i].close())
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.inputs, c.vectors, c.lanes = nil, nil, nil
	c.output = columnar.Batch{}
	c.ordinalVector = columnar.Vector{}
	c.options = Options{}
	c.life = nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
