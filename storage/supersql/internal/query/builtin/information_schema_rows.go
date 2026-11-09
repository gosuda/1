package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type expandArrayRowsPlan struct {
	element  model.TypeLayout
	metadata uint64
}

func ResolveExpandArrayRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.InformationSchemaFunctions == nil {
		return nil, &UnknownFunctionError{ID: id}
	}
	function, known := options.InformationSchemaFunctions.ResolveInformationSchemaFunction(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	if function.Operation != InformationExpandArray {
		return nil, &MissingImplementationError{Signature: function.Signature}
	}
	if len(arguments) != 1 || arguments[0].Type.Storage != model.TypeArray || len(arguments[0].Children) != 1 {
		return nil, sqlError("42804", "information_schema._pg_expandarray requires one concrete array argument")
	}
	if result.Type.Storage != model.TypeComposite || len(result.Children) != 2 || result.Children[0].Type != arguments[0].Children[0].Type || result.Children[1].Type.ID != 23 || result.Children[1].Type.Storage != model.TypeInt64 {
		return nil, sqlError("42804", "information_schema._pg_expandarray result must contain the array element and int4 subscript")
	}
	metadata := uint64(unsafe.Sizeof(expandArrayRowsPlan{}))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	return &expandArrayRowsPlan{element: arguments[0].Children[0], metadata: metadata}, nil
}

func (plan *expandArrayRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type expandArrayRowsIterator struct {
	values   []model.Value
	row      [2]model.Value
	position int
	claim    columnar.Allocation
	closed   bool
}

func (plan *expandArrayRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (RowSourceIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(arguments) != 1 || arguments[0].Type != model.TypeArray {
		return nil, sqlError("42804", "information_schema._pg_expandarray requires one array value")
	}
	inputBytes, err := model.ValueBytes(ctx, arguments[0], options.MaxInputBytes)
	if err != nil || inputBytes > options.MaxInputBytes {
		return nil, sqlError("54000", "information schema array input exceeded byte budget")
	}
	iterator := &expandArrayRowsIterator{}
	if arguments[0].IsNull || len(arguments[0].Dimensions) == 0 {
		return iterator, nil
	}
	count, valid := arrayElementCount(arguments[0].Dimensions)
	if !valid || count != len(arguments[0].ArrayVal) {
		return nil, sqlError("2202E", "array dimensions do not match array contents")
	}
	iterator.values = arguments[0].ArrayVal
	retained := uint64(unsafe.Sizeof(*iterator))
	if retained > options.MaxMemoryBytes {
		return nil, sqlError("54000", "information schema array row source exceeded memory budget")
	}
	if options.Allocator != nil {
		iterator.claim, err = options.Allocator.Reserve(retained)
		if err != nil {
			return nil, err
		}
	}
	for _, value := range iterator.values {
		rowBytes, valueErr := model.ValueBytes(ctx, value, options.MaxRowBytes)
		if valueErr != nil || rowBytes+uint64(unsafe.Sizeof(int64(0))) > options.MaxRowBytes {
			_ = iterator.Close()
			return nil, sqlError("54000", "information schema array row exceeded byte budget")
		}
	}
	return iterator, nil
}

func arrayElementCount(dimensions []model.ArrayDimension) (int, bool) {
	count := uint64(1)
	for _, dimension := range dimensions {
		if dimension.Length < 0 {
			return 0, false
		}
		length := uint64(dimension.Length)
		if length != 0 && count > uint64(math.MaxInt)/length {
			return 0, false
		}
		count *= length
	}
	return int(count), true
}

func (iterator *expandArrayRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if iterator.closed {
		return nil, false, sqlError("55000", "information schema array iterator is closed")
	}
	if iterator.position == len(iterator.values) {
		return nil, false, nil
	}
	value := iterator.values[iterator.position]
	iterator.row[0] = value
	iterator.row[1] = model.Int64Val(int64(iterator.position + 1))
	iterator.position++
	return iterator.row[:], true, nil
}

func (iterator *expandArrayRowsIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.values = nil
	iterator.claim.Release()
	iterator.claim = nil
	iterator.closed = true
	return nil
}

var _ RowSourcePlan = (*expandArrayRowsPlan)(nil)
var _ RowSourceIterator = (*expandArrayRowsIterator)(nil)
