// Package window evaluates windows over contiguous partitions with ordered peers.
package window

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type OrderKey struct {
	Column     int
	Descending bool
	NullsFirst bool
	InputCollation model.CollationID
}

type RangeSupport struct {
	Function       model.FunctionID
	Input          model.TypeLayout
	Offset         model.TypeLayout
	InputCollation model.CollationID
}

// ArgumentExpression is evaluated only when a function accesses that argument's
// row. Eval returns an owned value; the window cursor borrows the expression.
type ArgumentExpression interface {
	Layout() model.TypeLayout
	Eval(context.Context, []model.Value) (model.Value, error)
}

type Call struct {
	Function       model.FunctionID
	InputCollation model.CollationID
	Arguments      []int
	// Non-nil entries replace Arguments[i], which must be -1. A nonempty slice
	// must have the same length as Arguments and outlive the window cursor.
	Expressions []ArgumentExpression
	// Filter is borrowed and evaluated before arguments; SQL NULL rejects the row.
	Filter ArgumentExpression
}

// OutputColumns maps the output prefix; Calls append results in declaration order.
// Frame offsets are evaluated once by the caller. Nil means absent, not SQL NULL.
// PartitionMemoryBytes may be zero to force spill; other limits must be positive.
// WorkBytes bounds owned work, excluding input and output batches. Provision three
// query files and readers. Query and Encoder are borrowed; input ownership always
// transfers. Options and their slices are copied during construction.
type Options struct {
	InputLayouts           []model.TypeLayout
	OutputLayouts          []model.TypeLayout
	OutputColumns          []int
	PartitionColumns       []int
	OrderBy                []OrderKey
	Calls                  []Call
	Frame                  ir.Frame
	StartOffset, EndOffset *model.Value
	RangeSupport           [2]RangeSupport
	Encoder                storage.KeyEncoder
	Query                  *runtime.Query
	Builtin                builtin.ResolveOptions
	BatchRows              int
	BatchBytes             uint64
	MaxRecordBytes         uint64
	PartitionMemoryBytes   uint64
	MaxPartitionBytes      uint64
	MaxPartitionRows       uint64
	WorkBytes              uint64
	// Aggregates supplies SQL-defined aggregate declarations. A declared
	// aggregate reaches the same replay machinery a pinned one uses: the frame is
	// rebuilt from its start for every row, so no inverse transition is needed.
	Aggregates aggregate.AggregateResolver
}

type UnsupportedError struct {
	Function model.FunctionID
	Reason   string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("window %d is not executable: %s", e.Function, e.Reason)
}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	defer func() { o.Builtin = builtin.ResolveOptions{} }()
	missingInput := c.input == nil || o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidPartitions := o.MaxPartitionBytes == 0 || o.MaxPartitionRows == 0 || o.WorkBytes == 0
	if missingInput || invalidBatch || invalidPartitions {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.PartitionMemoryBytes, o.MaxPartitionBytes, o.MaxPartitionRows, o.WorkBytes} {
		if n > uint64(math.MaxInt)/128 {
			return runtime.ErrInvalidConfig
		}
	}
	if len(o.OutputLayouts) != len(o.OutputColumns)+len(o.Calls) {
		return columnar.ErrValueLayout
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	if err := c.validateFrame(); err != nil {
		return err
	}
	validColumn := func(i int) bool { return i >= 0 && i < len(o.InputLayouts) }
	for i, col := range o.OutputColumns {
		if !validColumn(col) || !reflect.DeepEqual(o.InputLayouts[col], o.OutputLayouts[i]) {
			return columnar.ErrValueLayout
		}
	}
	for _, col := range o.PartitionColumns {
		if !validColumn(col) {
			return columnar.ErrValueLayout
		}
	}
	for _, key := range o.OrderBy {
		if !validColumn(key.Column) {
			return columnar.ErrValueLayout
		}
	}
	argumentCopies := 0
	for i, call := range o.Calls {
		if err := validateCall(ctx, call, o.InputLayouts, o.OutputLayouts[len(o.OutputColumns)+i], o.Aggregates); err != nil {
			return err
		}
		argumentCopies = max(argumentCopies, additionalAggregateArgumentCopies(call))
	}
	base := uint64(unsafe.Sizeof(cursor{})) + 4096 + uint64(len(o.InputLayouts)+len(o.OutputLayouts)+len(o.Calls)+len(o.OrderBy)+len(o.PartitionColumns))*1024
	c.metadata = base + 4096
	// Three spools and readers each own codec/decode buffers. Current input,
	// current output and key copies remain live while navigation reads another row.
	fixed := base + c.rangeMetadata + o.MaxRecordBytes*uint64(32+8*len(o.Calls))
	argumentScratch := uint64(0)
	for _, call := range o.Calls {
		argumentScratch = max(argumentScratch, uint64(len(call.Arguments)))
	}
	fixed += argumentScratch * uint64(unsafe.Sizeof(model.Value{}))
	if argumentCopies > 0 {
		argumentWork := o.MaxRecordBytes * uint64(8*argumentCopies)
		if fixed > o.WorkBytes || argumentWork > o.WorkBytes-fixed {
			return resource("window workspace cannot admit aggregate arguments", nil)
		}
		fixed += argumentWork
	}
	spoolPeak := 3*(c.metadata+o.MaxRecordBytes*18+16384) + o.PartitionMemoryBytes
	if fixed > o.WorkBytes || spoolPeak > o.WorkBytes-fixed {
		return resource("window workspace cannot admit partition", nil)
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(fixed)
	if err != nil {
		return err
	}
	c.claim = claim
	rangeWork, err := c.configureNumericRanges(ctx, o.WorkBytes-fixed-spoolPeak)
	if err != nil {
		return err
	}
	remaining := base / 2
	copyLayouts := func(layouts []model.TypeLayout) ([]model.TypeLayout, error) {
		owned := make([]model.TypeLayout, len(layouts))
		for i, layout := range layouts {
			value, used, err := model.CopyTypeLayout(ctx, layout, remaining)
			if err != nil {
				return nil, err
			}
			owned[i], remaining = value, remaining-used
		}
		return owned, nil
	}
	if o.InputLayouts, err = copyLayouts(o.InputLayouts); err != nil {
		return err
	}
	if o.OutputLayouts, err = copyLayouts(o.OutputLayouts); err != nil {
		return err
	}
	o.OutputColumns = append([]int(nil), o.OutputColumns...)
	o.PartitionColumns = append([]int(nil), o.PartitionColumns...)
	o.OrderBy = append([]OrderKey(nil), o.OrderBy...)
	calls := make([]Call, len(o.Calls))
	for i, call := range o.Calls {
		calls[i] = Call{Function: call.Function, InputCollation: call.InputCollation, Arguments: append([]int(nil), call.Arguments...), Expressions: append([]ArgumentExpression(nil), call.Expressions...), Filter: call.Filter}
	}
	o.Calls = calls
	o.StartOffset, o.EndOffset, o.RangeSupport = nil, nil, [2]RangeSupport{}
	c.partitionKeys = make([]storage.KeyColumn, len(o.PartitionColumns))
	for i, col := range o.PartitionColumns {
		c.partitionKeys[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: o.InputLayouts[col].Type, Children: o.InputLayouts[col].Children}
	}
	c.orderKeys = make([]storage.KeyColumn, len(o.OrderBy))
	for i, key := range o.OrderBy {
		c.orderKeys[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: o.InputLayouts[key.Column].Type, Children: o.InputLayouts[key.Column].Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	for _, keys := range [][]storage.KeyColumn{c.partitionKeys, c.orderKeys} {
		if _, err := o.Encoder.EncodePrefix(ctx, keys, nil, o.MaxRecordBytes); err != nil {
			return err
		}
	}
	// Per-row aggregate scratch is allocated once for the widest call in the plan.
	c.aggregateArguments = make([]model.Value, int(argumentScratch))
	c.keyValues = make([]model.Value, max(len(c.partitionKeys), len(c.orderKeys)))
	c.inputValues = make([]model.Value, len(o.InputLayouts))
	c.current = make([]model.Value, len(o.InputLayouts))
	c.output = make([]model.Value, len(o.OutputLayouts))
	c.tiles = make([]tileState, len(o.Calls))
	if err := c.configureAggregates(ctx, o.WorkBytes-fixed-spoolPeak-rangeWork); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	c.builder, err = columnar.NewBuilder(o.OutputLayouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	return err
}

// CheckSupport checks executable function/frame support without opening input.
func CheckSupport(ctx context.Context, options Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(options.OutputLayouts) != len(options.OutputColumns)+len(options.Calls) {
		return columnar.ErrValueLayout
	}
	c := cursor{options: options}
	if err := c.validateFrame(); err != nil {
		return err
	}
	for i, call := range options.Calls {
		output := options.OutputLayouts[len(options.OutputColumns)+i]
		if err := validateCall(ctx, call, options.InputLayouts, output, options.Aggregates); err != nil {
			return err
		}
		if err := checkAggregateSupport(ctx, call, options, output); err != nil {
			return err
		}
	}
	return nil
}

// declaredLayoutMatches compares a declared layout with the planner's concrete
// one. A declaration may carry a polymorphic or unrefined modifier that the
// planner instantiates concretely, so identity and storage are what must agree.
func declaredLayoutMatches(declared, actual model.TypeLayout) bool {
	if declared.Type.ID == 0 {
		return true
	}
	if declared.Type.Storage == model.TypeAny {
		return true
	}
	return declared.Type.ID == actual.Type.ID && declared.Type.Storage == actual.Type.Storage &&
		len(declared.Children) == len(actual.Children)
}

func validateCall(ctx context.Context, call Call, input []model.TypeLayout, output model.TypeLayout, aggregates aggregate.AggregateResolver) error {
	id := call.Function
	signature, found := builtin.LookupSignature(id)
	isAggregate := found && signature.Kind == 'a'
	// A SQL-declared aggregate has no pinned signature. Its declaration is the
	// executable contract, so the argument identity comes from there; windowed
	// execution is available because the frame is replayed from its start and
	// therefore requires no inverse transition.
	declared := false
	if !found && aggregates != nil {
		declaration, ok, err := aggregates.ResolveAggregate(ctx, id)
		if err != nil {
			return err
		}
		declared = ok
		if ok {
			if len(call.Arguments) != len(declaration.Arguments) {
				return columnar.ErrValueLayout
			}
			for i, column := range call.Arguments {
				var actual model.TypeLayout
				if len(call.Expressions) != 0 && call.Expressions[i] != nil {
					actual = call.Expressions[i].Layout()
				} else if column >= 0 && column < len(input) {
					actual = input[column]
				} else {
					return columnar.ErrValueLayout
				}
				if !declaredLayoutMatches(declaration.Arguments[i], actual) {
					return columnar.ErrValueLayout
				}
			}
			if !declaredLayoutMatches(declaration.Result, output) {
				return columnar.ErrValueLayout
			}
		}
	}
	if !isAggregate && !declared && (id < 3100 || id > 3114) {
		return &UnsupportedError{Function: id, Reason: "function has no exact window implementation"}
	}
	if call.Filter != nil {
		if !isAggregate && !declared {
			return &UnsupportedError{Function: id, Reason: "FILTER requires an aggregate"}
		}
		layout := call.Filter.Layout()
		if layout.Type.ID != 16 || layout.Type.Storage != model.TypeBoolean {
			return columnar.ErrValueLayout
		}
	}
	argc := len(call.Arguments)
	switch {
	case declared:
	case isAggregate:
		argc = int(signature.ArgumentCount)
	default:
		switch id {
		case 3100, 3101, 3102, 3103, 3104:
			argc = 0
		case 3105, 3106, 3109, 3112, 3113:
			argc = 1
		case 3107, 3110, 3114:
			argc = 2
		case 3108, 3111:
			argc = 3
		}
	}
	if len(call.Arguments) != argc {
		return columnar.ErrValueLayout
	}
	if len(call.Expressions) != 0 && len(call.Expressions) != argc {
		return columnar.ErrValueLayout
	}
	for i, col := range call.Arguments {
		if len(call.Expressions) != 0 && call.Expressions[i] != nil {
			if col != -1 {
				return columnar.ErrValueLayout
			}
			continue
		}
		if col < 0 || col >= len(input) {
			return columnar.ErrValueLayout
		}
	}
	if isAggregate || declared {
		return nil
	}
	if id <= 3102 {
		if output.Type.ID != 20 || output.Type.Storage != model.TypeInt64 {
			return columnar.ErrValueLayout
		}
	} else if id <= 3104 {
		if output.Type.ID != 701 || output.Type.Storage != model.TypeFloat64 {
			return columnar.ErrValueLayout
		}
	} else if id == 3105 {
		argument := call.argumentLayout(input, 0)
		if argument.Type.ID != 23 || argument.Type.Storage != model.TypeInt64 || output.Type.ID != 23 || output.Type.Storage != model.TypeInt64 {
			return columnar.ErrValueLayout
		}
	} else {
		if !sameValueLayout(call.argumentLayout(input, 0), output) {
			return columnar.ErrValueLayout
		}
		if argc >= 2 && (call.argumentLayout(input, 1).Type.ID != 23 || call.argumentLayout(input, 1).Type.Storage != model.TypeInt64) {
			return columnar.ErrValueLayout
		}
		if argc == 3 && !sameValueLayout(call.argumentLayout(input, 2), output) {
			return columnar.ErrValueLayout
		}
	}
	return nil
}

func (call Call) argumentLayout(input []model.TypeLayout, argument int) model.TypeLayout {
	if len(call.Expressions) != 0 && call.Expressions[argument] != nil {
		return call.Expressions[argument].Layout()
	}
	return input[call.Arguments[argument]]
}

func sameValueLayout(input, output model.TypeLayout) bool {
	if builtin.TypeCollatable(input.Type) {
		input.Type.Collation = output.Type.Collation
	}
	return reflect.DeepEqual(input, output)
}

func (c *cursor) validateFrame() error {
	f := c.options.Frame
	invalidMode := f.Mode < ir.RowsFrame || f.Mode > ir.GroupsFrame || f.Exclusion > ir.ExcludeTies
	invalidStart := f.Start.Kind < ir.UnboundedPreceding || f.Start.Kind >= ir.UnboundedFollowing
	invalidEnd := f.End.Kind <= ir.UnboundedPreceding || f.End.Kind > ir.UnboundedFollowing
	if invalidMode || invalidStart || invalidEnd || f.Start.Kind > f.End.Kind {
		return runtime.ErrInvalidConfig
	}
	if f.Mode == ir.GroupsFrame && len(c.options.OrderBy) == 0 {
		return runtime.ErrInvalidConfig
	}
	for i, bound := range []ir.FrameBound{f.Start, f.End} {
		offset := c.options.StartOffset
		if i == 1 {
			offset = c.options.EndOffset
		}
		isOffset := bound.Kind == ir.OffsetPreceding || bound.Kind == ir.OffsetFollowing
		support := c.options.RangeSupport[i]
		if !isOffset {
			if offset != nil || bound.Offset != 0 || !reflect.DeepEqual(support, RangeSupport{}) {
				return runtime.ErrInvalidConfig
			}
			continue
		}
		if offset == nil {
			return runtime.ErrInvalidConfig
		}
		if offset.IsNull {
			return frameOffsetNull(i)
		}
		if f.Mode == ir.RangeFrame {
			if err := c.validateRangeKey(i, offset); err != nil {
				return err
			}
			if c.rangeNumeric {
				if offset.Type != model.TypeNumeric {
					return columnar.ErrValueLayout
				}
				continue
			}
			if c.rangeFloat {
				if err := c.floatRangeOffset(offset, i); err != nil {
					return err
				}
				continue
			}
			if c.rangeTemporal {
				continue
			}
		} else if !reflect.DeepEqual(support, RangeSupport{}) {
			return runtime.ErrInvalidConfig
		}
		if offset.Type != model.TypeInt64 {
			return columnar.ErrValueLayout
		}
		if offset.IntVal < 0 {
			if f.Mode == ir.RangeFrame {
				return &model.Error{Code: "22013", Message: "invalid preceding or following size in window function"}
			}
			side := "starting"
			if i == 1 {
				side = "ending"
			}
			return &model.Error{Code: "22013", Message: "frame " + side + " offset must not be negative"}
		}
		if i == 0 {
			c.startOffset = offset.IntVal
		} else {
			c.endOffset = offset.IntVal
		}
	}
	return nil
}

func frameOffsetNull(index int) error {
	side := "starting"
	if index == 1 {
		side = "ending"
	}
	return &model.Error{Code: "22004", Message: "frame " + side + " offset must not be null"}
}
