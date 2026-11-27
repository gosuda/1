package builtin

import (
	"context"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type arraySortCodec struct {
	element          Callable
	lookupErr        error
	elementType      model.TypeID
	missingArrayType bool
}

func resolveArraySort(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if signature.ID < 6388 || signature.ID > 6390 || len(arguments) != int(signature.ID-6388)+1 {
		return Callable{}, sqlError("42883", "array sort argument count does not match bound signature")
	}
	_, element, err := arrayCodecLayout(arguments[0], options)
	if err != nil {
		return Callable{}, err
	}
	if !sameArrayConstructionLayout(arguments[0], result) {
		return Callable{}, sqlError("42804", "array sort result differs from its input layout")
	}
	for _, argument := range arguments[1:] {
		if argument.Type.ID != 16 || argument.Type.Storage != model.TypeBoolean || len(argument.Children) != 0 {
			return Callable{}, sqlError("42804", "array sort ordering arguments must be boolean")
		}
	}
	metadata := uint64(unsafe.Sizeof(Callable{})) + uint64(unsafe.Sizeof(arraySortCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "array sort metadata budget exceeded")
	}
	selection, err := resolveArrayElementComparison(ctx, element, element, false, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	info, known := LookupType(element.Type.ID)
	codec := &arraySortCodec{
		element: selection.callable, lookupErr: selection.lookupErr, elementType: element.Type.ID,
		missingArrayType: known && info.Array == 0,
	}
	callable := Callable{
		signature: signature, resultStorage: model.TypeArray,
		metadataBytes: metadata + selection.metadataBytes, kernel: codec.call,
	}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	callable.signature.Result = result.Type.ID
	return callable, nil
}

func (codec *arraySortCodec) call(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	input := arguments[0]
	if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	rows := 0
	if len(input.Dimensions) != 0 {
		rows = int(input.Dimensions[0].Length)
	}
	// PostgreSQL returns before looking up operators, including for empty vectors.
	if rows < 2 {
		return copySortedArray(ctx, input, nil, budget)
	}
	if len(input.Dimensions) > 1 && codec.missingArrayType {
		return model.Value{}, sqlError("42704", fmt.Sprintf("could not find array type for data type %d", codec.elementType))
	}
	// Slice construction checks inner bounds before comparing. The first bound
	// is restored after result construction and is not subject to this check.
	for _, dimension := range input.Dimensions[1:] {
		if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
			return model.Value{}, sqlError("54000", "array lower bound is too large")
		}
	}
	if codec.lookupErr != nil {
		if len(input.Dimensions) == 1 {
			return model.Value{}, sqlError("0A000", fmt.Sprintf("could not identify a comparison function for type %d", codec.elementType))
		}
		return model.Value{}, codec.lookupErr
	}
	permutationBytes := uint64(rows) * uint64(unsafe.Sizeof(int(0)))
	nested, err := budget.After(permutationBytes)
	if err != nil {
		return model.Value{}, err
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(permutationBytes)
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	permutation := make([]int, rows)
	for i := range permutation {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		permutation[i] = i
	}
	order := arraySortOrder{input: input.ArrayVal, stride: len(input.ArrayVal) / rows, rankOne: len(input.Dimensions) == 1, element: codec.element}
	if len(arguments) > 1 {
		order.descending = arguments[1].IntVal != 0
		order.nullsFirst = order.descending
	}
	if len(arguments) > 2 {
		order.nullsFirst = arguments[2].IntVal != 0
	}
	for root := rows/2 - 1; root >= 0; root-- {
		if err := order.sift(ctx, permutation, root, rows, nested); err != nil {
			return model.Value{}, err
		}
	}
	for end := rows - 1; end > 0; end-- {
		permutation[0], permutation[end] = permutation[end], permutation[0]
		if err := order.sift(ctx, permutation, 0, end, nested); err != nil {
			return model.Value{}, err
		}
	}
	return copySortedArray(ctx, input, permutation, nested)
}

type arraySortOrder struct {
	input                  []model.Value
	stride                 int
	rankOne                bool
	descending, nullsFirst bool
	element                Callable
}

func (order *arraySortOrder) compare(ctx context.Context, left, right int, budget CallBudget) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	leftStart, rightStart := left*order.stride, right*order.stride
	if order.rankOne {
		leftNull, rightNull := order.input[leftStart].IsNull, order.input[rightStart].IsNull
		if leftNull != rightNull {
			if leftNull == order.nullsFirst {
				return -1, nil
			}
			return 1, nil
		}
	}
	// First-axis slices share all dimensions and bounds, so only their elements
	// can distinguish them. Inner NULLs always follow non-NULLs before reversal.
	result, err := compareArrayElements(ctx, order.input[leftStart:leftStart+order.stride], order.input[rightStart:rightStart+order.stride], order.element, false, budget)
	if err != nil {
		return 0, err
	}
	if order.descending {
		result = -result
	}
	return result, nil
}

func (order *arraySortOrder) sift(ctx context.Context, permutation []int, root, end int, budget CallBudget) error {
	for root < end/2 {
		child := root*2 + 1
		if child+1 < end {
			comparison, err := order.compare(ctx, permutation[child], permutation[child+1], budget)
			if err != nil {
				return err
			}
			if comparison < 0 {
				child++
			}
		}
		comparison, err := order.compare(ctx, permutation[root], permutation[child], budget)
		if err != nil {
			return err
		}
		if comparison >= 0 {
			return nil
		}
		permutation[root], permutation[child] = permutation[child], permutation[root]
		root = child
	}
	return nil
}

func copySortedArray(ctx context.Context, input model.Value, permutation []int, budget CallBudget) (model.Value, error) {
	limit := uint64(min(budget.OutputBytes, arrayInputMaxBytes))
	limit = min(limit, budget.MemoryBytes)
	if budget.Allocator != nil {
		limit = min(limit, budget.Allocator.AvailableBytes())
	}
	view := model.Value{Type: model.TypeArray, Dimensions: input.Dimensions, ArrayVal: input.ArrayVal}
	bytes, err := model.ValueBytes(ctx, view, limit)
	if err != nil {
		return model.Value{}, arrayPartsCopyError(err)
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(bytes)
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	owned, err := copyArrayParts(ctx, input.Dimensions, input.ArrayVal, nil, int64(bytes))
	if err != nil {
		return model.Value{}, err
	}
	if len(permutation) == 0 {
		return owned, nil
	}
	stride := len(owned.ArrayVal) / len(permutation)
	// Consume each permutation cycle in place; no row-sized temporary is needed.
	for start := range permutation {
		if start%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		current := start
		for permutation[current] != start {
			next := permutation[current]
			if err := swapArraySortRows(ctx, owned.ArrayVal, current*stride, next*stride, stride); err != nil {
				return model.Value{}, err
			}
			permutation[current] = current
			current = next
		}
		permutation[current] = current
	}
	return owned, nil
}

func swapArraySortRows(ctx context.Context, values []model.Value, left, right, stride int) error {
	for start := 0; start < stride; start += 256 {
		if err := ctx.Err(); err != nil {
			return err
		}
		for offset := start; offset < min(start+256, stride); offset++ {
			values[left+offset], values[right+offset] = values[right+offset], values[left+offset]
		}
	}
	return nil
}
