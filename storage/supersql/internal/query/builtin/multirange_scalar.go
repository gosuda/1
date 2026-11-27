package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveMultirangeScalar(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != int(signature.ArgumentCount) {
		return Callable{}, sqlError("42804", "multirange argument count mismatch")
	}
	family, err := rangeCallFamily(signature, arguments, result, options)
	if err != nil {
		return Callable{}, err
	}
	customConstructor := signature.ID >= 4280 && signature.ID <= 4298
	if _, builtinFamily := LookupRangeFamily(family.Range); builtinFamily {
		customConstructor = false
	}
	var constructorArray model.TypeLayout
	if customConstructor && len(arguments) != 0 && arguments[0].Type.Storage == model.TypeArray {
		var known bool
		constructorArray, known = ResolveRangeArrayLayout(options, family.Range)
		if !known {
			return Callable{}, sqlError("42804", "multirange constructor array layout mismatch")
		}
	}
	concrete := func(id model.TypeID) model.TypeID {
		switch id {
		case 3831:
			return family.Range
		case 4537:
			return family.Multirange
		case 2283:
			return family.Subtype
		}
		return id
	}
	expectedResultID := concrete(signature.Result)
	if customConstructor {
		expectedResultID = family.Multirange
	}
	expectedResult, known := resolvedTypeLayout(options, expectedResultID)
	if !known || result.Type.ID != expectedResult.Type.ID || result.Type.Storage != expectedResult.Type.Storage || len(result.Children) != 0 {
		return Callable{}, sqlError("42804", "multirange result layout mismatch")
	}
	for i, argument := range arguments {
		expectedID := concrete(signature.Arguments[i])
		if customConstructor {
			expectedID = family.Range
			if argument.Type.Storage == model.TypeArray {
				expectedID = constructorArray.Type.ID
			}
		}
		if argument.Type.ID != expectedID {
			return Callable{}, sqlError("42804", "multirange argument layout mismatch")
		}
		if argument.Type.Storage == model.TypeArray {
			_, element, err := arrayCodecLayout(argument, options)
			if err != nil {
				return Callable{}, err
			}
			if element.Type.ID != family.Range || element.Type.Storage != model.TypeBytes || len(element.Children) != 0 {
				return Callable{}, sqlError("42804", "multirange constructor element layout mismatch")
			}
			continue
		}
		expected, known := resolvedTypeLayout(options, expectedID)
		if !known || argument.Type.Storage != expected.Type.Storage || len(argument.Children) != 0 {
			return Callable{}, sqlError("42804", "multirange argument storage mismatch")
		}
	}
	metadata := uint64(unsafe.Sizeof(Callable{})) + uint64(unsafe.Sizeof(Signature{})) + 4*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "multirange metadata budget exceeded")
	}
	if signature.ID == 4388 {
		kernel := func(_ context.Context, _ []model.Value, _ CallBudget) (model.Value, error) {
			return model.Value{}, sqlError("XX000", "multirange_intersect_agg_transfn called in non-aggregate context")
		}
		return rangeTypedCallable(signature, arguments, result, kernel, metadata), nil
	}
	ops, owned, err := newRangeTypeOps(ctx, family, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	id := signature.ID
	var singles [2]bool
	for i := range min(2, len(arguments)) {
		singles[i] = arguments[i].Type.ID == family.Range
	}
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		// Fixed traversal state is admitted even when a subtype comparison needs
		// its own workspace. No invocation state is captured by the callable.
		stateBytes := 2*uint64(unsafe.Sizeof(multirangeSequence{})) + 10*uint64(unsafe.Sizeof(rangeDatum{}))
		nested, claim, err := reserveMultirangeWork(ctx, stateBytes, budget)
		if err != nil {
			return model.Value{}, err
		}
		if claim != nil {
			defer claim.Release()
		}
		if id >= 4280 && id <= 4298 {
			return constructMultirange(ctx, &ops, values, nested)
		}
		if id == 4249 || id == 4252 {
			multiIndex, elementIndex := 0, 1
			if id == 4252 {
				multiIndex, elementIndex = 1, 0
			}
			sequence, err := newMultirangeSequence(ctx, family, values[multiIndex], false)
			if err != nil {
				return model.Value{}, err
			}
			element, err := rangeBoundFromValue(family, values[elementIndex], rangeLower, true)
			if err != nil {
				return model.Value{}, err
			}
			for {
				value, more, err := sequence.next(ctx)
				if err != nil {
					return model.Value{}, err
				}
				if !more {
					return model.BoolVal(false), nil
				}
				contains, err := ops.containsElement(ctx, value, element, nested)
				if err != nil || contains {
					return model.BoolVal(contains), err
				}
			}
		}
		a, err := newMultirangeSequence(ctx, family, values[0], singles[0])
		if err != nil {
			return model.Value{}, err
		}
		if len(values) == 1 {
			return multirangeUnary(ctx, &ops, a, id, nested)
		}
		b, err := newMultirangeSequence(ctx, family, values[1], singles[1])
		if err != nil {
			return model.Value{}, err
		}
		switch id {
		case 4270, 4271, 4272:
			return multirangeAlgebra(ctx, &ops, a, b, id, nested)
		case 4244, 4245, 4273, 4274, 4275, 4276, 4277:
			order, err := compareMultiranges(ctx, &ops, a, b, nested)
			if err != nil {
				return model.Value{}, err
			}
			switch id {
			case 4273:
				return model.Int64Val(int64(order)), nil
			case 4244:
				return model.BoolVal(order == 0), nil
			case 4245:
				return model.BoolVal(order != 0), nil
			case 4274:
				return model.BoolVal(order < 0), nil
			case 4275:
				return model.BoolVal(order <= 0), nil
			case 4276:
				return model.BoolVal(order >= 0), nil
			default:
				return model.BoolVal(order > 0), nil
			}
		case 4246, 4247, 4248:
			value, err := multirangeOverlaps(ctx, &ops, a, b, nested)
			return model.BoolVal(value), err
		case 4250, 4251, 4541:
			value, err := multirangeContains(ctx, &ops, a, b, nested)
			return model.BoolVal(value), err
		case 4253, 4254, 4542:
			value, err := multirangeContains(ctx, &ops, b, a, nested)
			return model.BoolVal(value), err
		default:
			x, err := multirangeSpan(ctx, a)
			if err != nil {
				return model.Value{}, err
			}
			y, err := multirangeSpan(ctx, b)
			if err != nil {
				return model.Value{}, err
			}
			if x.empty || y.empty {
				return model.BoolVal(false), nil
			}
			if id >= 4255 && id <= 4257 {
				value, err := ops.adjacent(ctx, x, y, nested)
				return model.BoolVal(value), err
			}
			var order int
			switch {
			case id >= 4258 && id <= 4260:
				order, err = ops.compareBounds(ctx, x.upper, y.lower, nested)
				return model.BoolVal(order < 0), err
			case id >= 4261 && id <= 4263:
				order, err = ops.compareBounds(ctx, x.lower, y.upper, nested)
				return model.BoolVal(order > 0), err
			case id >= 4264 && id <= 4266:
				order, err = ops.compareBounds(ctx, x.upper, y.upper, nested)
				return model.BoolVal(order <= 0), err
			case id >= 4267 && id <= 4269:
				order, err = ops.compareBounds(ctx, x.lower, y.lower, nested)
				return model.BoolVal(order >= 0), err
			}
		}
		return model.Value{}, &MissingImplementationError{Signature: signature}
	}
	return rangeTypedCallable(signature, arguments, result, kernel, metadata+owned), nil
}

func multirangeUnary(ctx context.Context, ops *rangeTypeOps, sequence multirangeSequence, id model.FunctionID, budget CallBudget) (model.Value, error) {
	span, err := multirangeSpan(ctx, sequence)
	if err != nil {
		return model.Value{}, err
	}
	switch id {
	case 4228:
		return encodeRange(ctx, ops.family, span, budget)
	case 4235, 4236:
		bound := span.lower
		if id == 4236 {
			bound = span.upper
		}
		if span.empty {
			bound.infinite = true
		}
		return ownedRangeBoundValue(ctx, ops.family, bound, budget)
	case 4237:
		return model.BoolVal(span.empty), nil
	case 4238:
		return model.BoolVal(!span.empty && span.lower.inclusive), nil
	case 4239:
		return model.BoolVal(!span.empty && span.upper.inclusive), nil
	case 4240:
		return model.BoolVal(!span.empty && span.lower.infinite), nil
	case 4241:
		return model.BoolVal(!span.empty && span.upper.infinite), nil
	}
	return model.Value{}, sqlError("XX000", "invalid multirange unary operation")
}

func constructMultirange(ctx context.Context, ops *rangeTypeOps, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if len(arguments) == 0 {
		return encodeMultirange(ctx, ops.family, nil, budget)
	}
	input := arguments[0]
	if input.IsNull {
		return model.Value{}, sqlError("XX000", "multirange values cannot contain null members")
	}
	if input.Type != model.TypeArray {
		value, err := decodeRange(ctx, ops.family, input)
		if err != nil {
			return model.Value{}, err
		}
		one := [1]rangeDatum{value}
		return normalizeMultirange(ctx, ops, one[:], budget)
	}
	if len(input.Dimensions) > 1 {
		return model.Value{}, sqlError("21000", "multiranges cannot be constructed from multidimensional arrays")
	}
	count := uint64(len(input.ArrayVal))
	if err := model.ValidateArrayShape(input.Dimensions, count); err != nil {
		return model.Value{}, sqlError("2202E", "invalid multirange constructor array shape")
	}
	if count > math.MaxInt32 || count > math.MaxUint64/uint64(unsafe.Sizeof(rangeDatum{})) {
		return model.Value{}, sqlError("54000", "multirange count exceeds encoding limit")
	}
	nested, claim, err := reserveMultirangeWork(ctx, count*uint64(unsafe.Sizeof(rangeDatum{})), budget)
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	values := make([]rangeDatum, len(input.ArrayVal))
	for i, value := range input.ArrayVal {
		if value.IsNull {
			return model.Value{}, sqlError("22004", "multirange values cannot contain null members")
		}
		values[i], err = decodeRange(ctx, ops.family, value)
		if err != nil {
			return model.Value{}, err
		}
	}
	return normalizeMultirange(ctx, ops, values, nested)
}
