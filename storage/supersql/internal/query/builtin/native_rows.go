package builtin

import (
	"context"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type nativeRowsPlan struct {
	id          model.FunctionID
	argument    model.Type
	result      model.Type
	resultTypes []model.DataType
	timeZone    TimeZoneContext
	metadata    uint64
}

// ResolveNativeRows resolves built-in row sources whose execution state is
// independent of the scalar callable registry.
func ResolveNativeRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	switch id {
	case 938, 939, 1066, 1067, 1068, 1069, 1191, 1192, 1293, 2331, 3259, 3260, 6160, 6161, 6274:
	default:
		return nil, &MissingImplementationError{Signature: signature}
	}
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "row source argument count does not match signature")
	}
	if len(result.Children) != len(result.FieldNames) && len(result.FieldNames) != 0 {
		return nil, sqlError("42804", "row source result field names do not match fields")
	}
	if err := validateNativeRowsLayouts(id, arguments, result); err != nil {
		return nil, err
	}
	count := len(result.Children)
	if result.Type.Storage != model.TypeComposite {
		count = 1
	}
	metadata := uint64(unsafe.Sizeof(nativeRowsPlan{})) + uint64(count)*uint64(unsafe.Sizeof(model.DataType(0))) + uint64(len(options.TimeZone.Name))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	plan := &nativeRowsPlan{id: id, result: result.Type, timeZone: options.TimeZone, metadata: metadata, resultTypes: make([]model.DataType, count)}
	if len(arguments) != 0 {
		plan.argument = arguments[0].Type
	}
	if result.Type.Storage == model.TypeComposite {
		for i := range result.Children {
			plan.resultTypes[i] = result.Children[i].Type.Storage
		}
	} else {
		plan.resultTypes[0] = result.Type.Storage
	}
	plan.timeZone.Name = strings.Clone(plan.timeZone.Name)
	return plan, nil
}

func validateNativeRowsLayouts(id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout) error {
	scalarResult := len(result.Children) == 0 && result.Type.Storage != model.TypeComposite
	switch id {
	case 1066, 1067:
		if !scalarResult || result.Type.ID != 23 || result.Type.Storage != model.TypeInt64 {
			return sqlError("42804", "generate_series result must be int4")
		}
	case 1068, 1069:
		if !scalarResult || result.Type.ID != 20 || result.Type.Storage != model.TypeInt64 {
			return sqlError("42804", "generate_series result must be int8")
		}
	case 938:
		if !scalarResult || result.Type.ID != 1114 || result.Type.Storage != model.TypeTimestamp {
			return sqlError("42804", "generate_series result must be timestamp")
		}
	case 939, 6274:
		if !scalarResult || result.Type.ID != 1184 || result.Type.Storage != model.TypeTimestamp {
			return sqlError("42804", "generate_series result must be timestamptz")
		}
	case 3259, 3260:
		if !scalarResult || result.Type.ID != 1700 || result.Type.Storage != model.TypeNumeric {
			return sqlError("42804", "generate_series result must be numeric")
		}
	case 1191, 1192:
		if !scalarResult || result.Type.ID != 23 || result.Type.Storage != model.TypeInt64 || arguments[0].Type.Storage != model.TypeArray {
			return sqlError("42804", "generate_subscripts requires an array and returns int4")
		}
	case 2331:
		if arguments[0].Type.Storage != model.TypeArray || len(arguments[0].Children) != 1 {
			return sqlError("42804", "unnest requires a concrete array layout")
		}
	case 1293:
		if arguments[0].Type.Storage != model.TypeBytes || result.Type.Storage != model.TypeBytes || len(result.Children) != 0 {
			return sqlError("42804", "multirange unnest requires range storage")
		}
	case 6160, 6161:
		if !scalarResult || result.Type.ID != 25 || result.Type.Storage != model.TypeString {
			return sqlError("42804", "string_to_table result must be text")
		}
	}
	return nil
}

func (plan *nativeRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type nativeRowsIterator struct {
	plan       *nativeRowsPlan
	row        []model.Value
	array      []model.Value
	text       string
	separator  string
	nullString string
	index      int
	current    int64
	stop       int64
	step       int64
	minimum    int64
	maximum    int64
	timestamp  TimestampSeries
	numeric    *NumericSeries
	timestamptz *TimestamptzSeries
	multirange MultirangeIterator
	kind       model.FunctionID
	maxRow     uint64
	nullMarker bool
	hasNullString bool
	claim      columnar.Allocation
	emitted    bool
	done       bool
	closed     bool
}

func (plan *nativeRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (_ RowSourceIterator, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, _ := LookupSignature(plan.id)
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "row source argument count does not match signature")
	}
	var inputBytes uint64
	for _, argument := range arguments {
		remaining := options.MaxInputBytes - min(options.MaxInputBytes, inputBytes)
		size, valueErr := model.ValueBytes(ctx, argument, remaining)
		if valueErr != nil || size > remaining {
			return nil, sqlError("54000", "row source input exceeded byte budget")
		}
		inputBytes += size
	}
	retained := uint64(unsafe.Sizeof(nativeRowsIterator{})) + uint64(len(plan.resultTypes))*uint64(unsafe.Sizeof(model.Value{}))
	if inputBytes > options.MaxMemoryBytes || retained > options.MaxMemoryBytes-inputBytes {
		return nil, sqlError("54000", "row source input exceeded memory budget")
	}
	var claim columnar.Allocation
	if options.Allocator != nil {
		claim, err = options.Allocator.Reserve(inputBytes + retained)
		if err != nil {
			return nil, err
		}
	}
	iterator := &nativeRowsIterator{plan: plan, row: make([]model.Value, len(plan.resultTypes)), kind: plan.id, maxRow: options.MaxRowBytes, minimum: math.MinInt64, maximum: math.MaxInt64, claim: claim}
	fail := func(failure error) (RowSourceIterator, error) {
		return nil, errors.Join(failure, iterator.Close())
	}
	if signature.Strict {
		for _, argument := range arguments {
			if argument.IsNull {
				iterator.done = true
				return iterator, nil
			}
		}
	}
	switch plan.id {
	case 1066, 1067, 1068, 1069:
		iterator.current, iterator.stop, iterator.step = arguments[0].IntVal, arguments[1].IntVal, 1
		if len(arguments) == 3 {
			iterator.step = arguments[2].IntVal
		}
		if iterator.step == 0 {
			return fail(sqlError("22023", "step size cannot equal zero"))
		}
		if plan.result.ID == 23 {
			iterator.minimum, iterator.maximum = math.MinInt32, math.MaxInt32
		}
		iterator.done = (iterator.step > 0 && iterator.current > iterator.stop) || (iterator.step < 0 && iterator.current < iterator.stop)
	case 938:
		iterator.timestamp, err = NewTimestampSeries(arguments[0].IntVal, arguments[1].IntVal, arguments[2].IntervalVal)
	case 939, 6274:
		var explicit *string
		if len(arguments) == 4 {
			explicit = &arguments[3].StrVal
		}
		iterator.timestamptz, err = NewTimestamptzSeries(ctx, arguments[0].IntVal, arguments[1].IntVal, arguments[2].IntervalVal, TimestamptzSeriesOptions{TimeZone: plan.timeZone, ExplicitZone: explicit, MemoryBytes: options.MaxMemoryBytes - inputBytes - retained, Allocator: options.Allocator})
	case 3259, 3260:
		step := "1"
		if len(arguments) == 3 {
			step = arguments[2].StrVal
		}
		iterator.numeric, err = NewNumericSeries(ctx, arguments[0].StrVal, arguments[1].StrVal, step, NumericSeriesOptions{MemoryBytes: options.MaxMemoryBytes - inputBytes - retained, ValueBytes: options.MaxRowBytes, Allocator: options.Allocator})
	case 1191, 1192:
		dimension := arguments[1].IntVal
		if dimension < 1 || dimension > int64(len(arguments[0].Dimensions)) {
			iterator.done = true
			break
		}
		bounds := arguments[0].Dimensions[dimension-1]
		iterator.current = int64(bounds.LowerBound)
		iterator.stop = iterator.current + int64(bounds.Length) - 1
		iterator.step = 1
		if len(arguments) == 3 && arguments[2].IntVal != 0 {
			iterator.current, iterator.stop, iterator.step = iterator.stop, iterator.current, -1
		}
		iterator.done = bounds.Length == 0
	case 2331:
		iterator.array = arguments[0].ArrayVal
	case 1293:
		iterator.multirange, err = NewMultirangeIterator(ctx, plan.argument.ID, arguments[0])
	case 6160, 6161:
		if arguments[0].IsNull {
			iterator.done = true
			break
		}
		iterator.text = arguments[0].StrVal
		if arguments[1].IsNull {
			iterator.nullMarker = true
		} else {
			iterator.separator = arguments[1].StrVal
		}
		if len(arguments) == 3 && !arguments[2].IsNull {
			iterator.nullString = arguments[2].StrVal
			iterator.hasNullString = true
		}
		iterator.done = iterator.text == ""
	}
	if err != nil {
		return fail(err)
	}
	return iterator, nil
}

func (iterator *nativeRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator == nil || iterator.closed || iterator.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	clear(iterator.row)
	switch iterator.kind {
	case 1066, 1067, 1068, 1069, 1191, 1192:
		iterator.row[0] = model.Int64Val(iterator.current)
		overflow := iterator.step > 0 && iterator.current > iterator.maximum-iterator.step || iterator.step < 0 && iterator.current < iterator.minimum-iterator.step
		if overflow {
			iterator.done = true
		} else {
			iterator.current += iterator.step
			iterator.done = iterator.step > 0 && iterator.current > iterator.stop || iterator.step < 0 && iterator.current < iterator.stop
		}
	case 938:
		value, ok, err := iterator.timestamp.Next(ctx)
		if err != nil || !ok {
			iterator.done = !ok
			return nil, false, err
		}
		iterator.row[0] = value
	case 939, 6274:
		value, ok, err := iterator.timestamptz.Peek(ctx)
		if err != nil || !ok {
			iterator.done = !ok
			return nil, false, err
		}
		iterator.row[0] = value
		if err := iterator.timestamptz.Advance(); err != nil {
			return nil, false, err
		}
	case 3259, 3260:
		value, ok, err := iterator.numeric.Peek(ctx)
		if err != nil || !ok {
			iterator.done = !ok
			return nil, false, err
		}
		iterator.row[0] = value
		if err := iterator.numeric.Advance(); err != nil {
			return nil, false, err
		}
	case 2331:
		if iterator.index >= len(iterator.array) {
			iterator.done = true
			return nil, false, nil
		}
		value := iterator.array[iterator.index]
		iterator.index++
		if iterator.plan.result.Storage == model.TypeComposite {
			if value.IsNull {
				for i, typ := range iterator.plan.resultTypes {
					iterator.row[i] = model.NullVal(typ)
				}
			} else {
				if len(value.ArrayVal) != len(iterator.row) {
					return nil, false, sqlError("42804", "unnest composite value does not match result layout")
				}
				copy(iterator.row, value.ArrayVal)
			}
		} else {
			iterator.row[0] = value
		}
	case 1293:
		value, ok, err := iterator.multirange.NextBorrowed(ctx)
		if err != nil || !ok {
			iterator.done = !ok
			return nil, false, err
		}
		iterator.row[0] = model.BytesVal(value)
	case 6160, 6161:
		piece, more := iterator.nextStringPiece()
		iterator.done = !more
		if iterator.hasNullString && piece == iterator.nullString {
			iterator.row[0] = model.NullVal(model.TypeString)
		} else {
			iterator.row[0] = model.StringVal(piece)
		}
	}
	if _, err := model.ValueBytes(ctx, model.Value{Type: model.TypeArray, ArrayVal: iterator.row}, iterator.maxRow); err != nil {
		return nil, false, sqlError("54000", "row source output exceeded byte budget")
	}
	return iterator.row, true, nil
}

func (iterator *nativeRowsIterator) nextStringPiece() (string, bool) {
	if iterator.emitted && iterator.text == "" {
		return "", false
	}
	iterator.emitted = true
	if iterator.nullMarker {
		_, size := utf8.DecodeRuneInString(iterator.text)
		piece := iterator.text[:size]
		iterator.text = iterator.text[size:]
		return piece, iterator.text != ""
	}
	if iterator.separator == "" {
		piece := iterator.text
		iterator.text = ""
		return piece, false
	}
	index := strings.Index(iterator.text, iterator.separator)
	if index < 0 {
		piece := iterator.text
		iterator.text = ""
		return piece, false
	}
	piece := iterator.text[:index]
	iterator.text = iterator.text[index+len(iterator.separator):]
	return piece, true
}

func (iterator *nativeRowsIterator) Close() error {
	if iterator == nil || iterator.closed {
		return nil
	}
	iterator.closed, iterator.done = true, true
	iterator.array = nil
	iterator.text, iterator.separator, iterator.nullString = "", "", ""
	iterator.multirange.Close()
	var err error
	if iterator.numeric != nil {
		err = errors.Join(err, iterator.numeric.Close())
		iterator.numeric = nil
	}
	if iterator.timestamptz != nil {
		err = errors.Join(err, iterator.timestamptz.Close())
		iterator.timestamptz = nil
	}
	clear(iterator.row)
	iterator.row = nil
	iterator.plan = nil
	if iterator.claim != nil {
		iterator.claim.Release()
		iterator.claim = nil
	}
	return err
}
