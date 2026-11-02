package builtin

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeScalarPlan struct {
	ops rangeTypeOps
	id  model.FunctionID
}

func rangeScalarConstructor(id model.FunctionID) bool {
	switch id {
	case 3840, 3841, 3844, 3845, 3933, 3934, 3937, 3938, 3941, 3942, 3945, 3946:
		return true
	}
	return false
}

func rangeScalarDifference(id model.FunctionID) bool {
	return id >= 3922 && id <= 3925 || id == 3929 || id == 3930
}

func resolveRangeScalar(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != int(signature.ArgumentCount) {
		return Callable{}, sqlError("42883", "range argument count does not match signature")
	}
	family, err := rangeCallFamily(signature, arguments, result, options)
	if err != nil {
		return Callable{}, err
	}
	customConstructor := rangeScalarConstructor(signature.ID)
	if _, builtinFamily := LookupRangeFamily(family.Range); builtinFamily {
		customConstructor = false
	}
	specialize := func(id model.TypeID) model.TypeID {
		switch id {
		case 3831:
			return family.Range
		case 2283:
			return family.Subtype
		default:
			return id
		}
	}
	for i, argument := range arguments {
		if err := validateConcreteLayout(ctx, argument); err != nil {
			return Callable{}, err
		}
		expected := specialize(signature.Arguments[i])
		if customConstructor && i < 2 {
			expected = family.Subtype
		}
		if argument.Type.ID != expected || len(argument.Children) != 0 {
			return Callable{}, sqlError("42804", "range argument type does not match signature")
		}
	}
	if err := validateConcreteLayout(ctx, result); err != nil {
		return Callable{}, err
	}
	expectedResult := specialize(signature.Result)
	if customConstructor {
		expectedResult = family.Range
	}
	if result.Type.ID != expectedResult || len(result.Children) != 0 {
		return Callable{}, sqlError("42804", "range result type does not match signature")
	}
	id := signature.ID
	constructor, difference := rangeScalarConstructor(id), rangeScalarDifference(id)
	known := constructor || difference || id >= 3848 && id <= 3874 || id == 3914 || id == 3915 || id == 3928 || id == 4057 || id == 4401
	if !known {
		return Callable{}, sqlError("42883", "unsupported range scalar function")
	}
	metadata := uint64(unsafe.Sizeof(rangeScalarPlan{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "range scalar metadata budget exceeded")
	}
	plan := rangeScalarPlan{ops: rangeTypeOps{family: family}, id: id}
	needsComparison := !difference && id != 4401 && !(id >= 3848 && id <= 3854)
	if needsComparison {
		ops, retained, err := newRangeTypeOps(ctx, family, options, maximum-metadata+uint64(unsafe.Sizeof(rangeTypeOps{})))
		if err != nil {
			return Callable{}, err
		}
		plan.ops = ops
		metadata += retained - uint64(unsafe.Sizeof(rangeTypeOps{}))
	}
	return rangeTypedCallable(signature, arguments, result, plan.call, metadata), nil
}

func (plan *rangeScalarPlan) call(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	ops, id := &plan.ops, plan.id
	if rangeScalarConstructor(id) {
		return plan.construct(ctx, args, budget)
	}
	if rangeScalarDifference(id) {
		if id == 3924 {
			return rangeNumericDifference(ctx, args, budget)
		}
		value := float64(args[0].IntVal) - float64(args[1].IntVal)
		if id == 3929 || id == 3930 {
			value /= 1000000
		}
		return model.Float64Val(value), nil
	}
	if id == 4401 {
		return model.Value{}, sqlError("XX000", "range_intersect_agg_transfn called in non-aggregate context")
	}
	first := 0
	if id == 3860 {
		first = 1
	}
	a, err := decodeRange(ctx, ops.family, args[first])
	if err != nil {
		return model.Value{}, err
	}
	switch id {
	case 3848, 3849:
		bound := a.lower
		if id == 3849 {
			bound = a.upper
		}
		if a.empty {
			bound.infinite = true
		}
		return ownedRangeBoundValue(ctx, ops.family, bound, budget)
	case 3850:
		return model.BoolVal(a.empty), nil
	case 3851:
		return model.BoolVal(!a.empty && a.lower.inclusive), nil
	case 3852:
		return model.BoolVal(!a.empty && a.upper.inclusive), nil
	case 3853:
		return model.BoolVal(!a.empty && a.lower.infinite), nil
	case 3854:
		return model.BoolVal(!a.empty && a.upper.infinite), nil
	case 3914, 3915, 3928:
		value, err := ops.canonical(ctx, a, budget)
		if err != nil {
			return model.Value{}, err
		}
		return encodeRange(ctx, ops.family, value, budget)
	case 3858, 3860:
		element, err := rangeBoundFromValue(ops.family, args[1-first], rangeLower, true)
		if err != nil {
			return model.Value{}, err
		}
		contains, err := ops.containsElement(ctx, a, element, budget)
		return model.BoolVal(contains), err
	}
	b, err := decodeRange(ctx, ops.family, args[1])
	if err != nil {
		return model.Value{}, err
	}
	var value rangeDatum
	switch id {
	case 3867, 4057:
		value, err = ops.union(ctx, a, b, id == 3867, budget)
	case 3868:
		value, err = ops.intersection(ctx, a, b, budget)
	case 3869:
		var pieces [2]rangeDatum
		var count int
		pieces, count, err = ops.difference(ctx, a, b, budget)
		if err == nil && count == 2 {
			return model.Value{}, sqlError("22000", "result of range difference would not be contiguous")
		}
		value = rangeDatum{empty: true}
		if count == 1 {
			value = pieces[0]
		}
	default:
		return ops.scalarPredicate(ctx, id, a, b, budget)
	}
	if err != nil {
		return model.Value{}, err
	}
	return encodeRange(ctx, ops.family, value, budget)
}

func (plan *rangeScalarPlan) construct(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
	lowerInclusive, upperInclusive := true, false
	if len(args) == 3 {
		if args[2].IsNull {
			return model.Value{}, sqlError("22000", "range constructor flags argument must not be null")
		}
		flags := args[2].StrVal
		if len(flags) != 2 {
			return model.Value{}, sqlError("42601", "invalid range bound flags")
		}
		validLower := flags[0] == '[' || flags[0] == '('
		validUpper := flags[1] == ']' || flags[1] == ')'
		if !validLower || !validUpper {
			return model.Value{}, sqlError("42601", "invalid range bound flags")
		}
		lowerInclusive, upperInclusive = flags[0] == '[', flags[1] == ']'
	}
	lower, err := rangeBoundFromValue(plan.ops.family, args[0], rangeLower, lowerInclusive)
	if err != nil {
		return model.Value{}, err
	}
	upper, err := rangeBoundFromValue(plan.ops.family, args[1], rangeUpper, upperInclusive)
	if err != nil {
		return model.Value{}, err
	}
	value, err := plan.ops.canonical(ctx, rangeDatum{lower: lower, upper: upper}, budget)
	if err != nil {
		return model.Value{}, err
	}
	return encodeRange(ctx, plan.ops.family, value, budget)
}

func rangeNumericDifference(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	left, err := parseNumericRangeValue(ctx, work, args[0])
	if err != nil {
		return model.Value{}, err
	}
	right, err := parseNumericRangeValue(ctx, work, args[1])
	if err != nil {
		return model.Value{}, err
	}
	difference, err := numericArithmetic(ctx, work, left, right, opMi)
	if err != nil {
		return model.Value{}, err
	}
	return numericToPrimitive(ctx, work, difference, 701, budget)
}
