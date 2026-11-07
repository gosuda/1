package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveNativeTyped(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	switch signature.ID {
	case 515, 516:
		return resolveArrayComparison(ctx, signature, arguments, result, options, maximum)
	case 2401, 2503, 5091, 2403:
		if len(arguments) != 1 || result.Type.ID != 17 { return Callable{}, sqlError("42804", "binary send signature mismatch") }
		codec, err := ResolveTypeBinaryCodec(ctx, arguments[0], options, maximum)
		if err != nil { return Callable{}, err }
		return nativeTypedCallable(signature, arguments, result, scalarKernel(codec.Output), codec.OwnedBytes), nil
	case 1619:
		if len(arguments) != 1 || result.Type.ID != 2206 { return Callable{}, sqlError("42804", "pg_typeof signature mismatch") }
		typeID := arguments[0].Type.ID
		return nativeTypedCallable(signature, arguments, result, func(context.Context, []model.Value, CallBudget) (model.Value, error) { return model.Int64Val(int64(typeID)), nil }, 0), nil
	case 3162:
		if len(arguments) != 1 || result.Type.ID != 25 { return Callable{}, sqlError("42804", "pg_collation_for signature mismatch") }
		collation := arguments[0].Type.Collation
		return nativeTypedCallable(signature, arguments, result, func(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) { if collation == 0 { return model.NullVal(model.TypeString), nil }; info, ok := LookupCollation(collation); if !ok { return model.Value{}, sqlError("XX000", "bound collation is unavailable") }; return ownedText(ctx, budget.OutputBytes, info.Name) }, 0), nil
	case 2003, 2004:
		return resolveNativeTextConcat(ctx, signature, arguments, result, options, maximum)
	case 3181, 3182, 3183, 3184, 3185, 3186, 3187:
		return resolveNativeRecordImage(ctx, signature, arguments, result, options, maximum)
	case 6375, 6376:
		return resolveNativeRecordExtremum(ctx, signature, arguments, result, options, maximum)
	case 3414:
		resolved, ok := resolvedType(options, arguments[0].Type.ID, model.TypeKindEnum)
		if !ok { return Callable{}, sqlError("42804", "hashenumextended requires a resolved enum") }
		kernel := func(ctx context.Context, values []model.Value, _ CallBudget) (model.Value, error) { if _, err := resolved.enumOrder(values[0]); err != nil { return model.Value{}, err }; return model.Int64Val(int64(pgHashUint32Extended(uint32(values[0].IntVal), uint64(values[1].IntVal)))), ctx.Err() }
		return nativeTypedCallable(signature, arguments, result, kernel, uint64(unsafe.Sizeof((*ResolvedType)(nil)))), nil
	case 5028:
		return resolveNativeHashPartition(ctx, signature, arguments, result, options, maximum)
	case 6215, 6216:
		return resolveNativeArrayRandom(signature, arguments, result)
	case 2804, 3547:
		if len(arguments) != 2 || result.Type.ID != 20 { return Callable{}, sqlError("42804", "polymorphic counter signature mismatch") }
		kernel := func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) {
			value := values[0].IntVal
			if signature.ID == 2804 { if value == math.MaxInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }; value++ } else { if value == math.MinInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }; value-- }
			return model.Int64Val(value), nil
		}
		return nativeTypedCallable(signature, arguments, result, kernel, 0), nil
	case 6292:
		if len(arguments) != 2 || result.Type != arguments[0].Type || result.Type != arguments[1].Type { return Callable{}, sqlError("42804", "any_value transition signature mismatch") }
		kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
			selected := values[0]; if selected.IsNull { selected = values[1] }
			owned, _, err := model.CopyValue(ctx, selected, uint64(max(budget.OutputBytes, 0)))
			if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "any_value result exceeds output budget", Cause: err} }
			return owned, nil
		}
		return nativeTypedCallable(signature, arguments, result, kernel, 0), nil
	case 2333, 2334, 3173, 3180, 3265, 3268, 4051, 4052, 6275, 6277, 6278, 6279, 6283, 6285, 6286, 6287:
		return resolveCollectionAggregateTyped(ctx, signature, arguments, result, options, maximum)
	default:
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
}

func nativeTypedCallable(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, kernel scalarKernel, metadata uint64) Callable {
	callable := Callable{signature: signature, kernel: kernel, resultStorage: result.Type.Storage, metadataBytes: metadata}
	callable.signature.Result = result.Type.ID
	if len(arguments) <= len(callable.argumentStorage) {
		for i, argument := range arguments { callable.signature.Arguments[i] = argument.Type.ID; callable.argumentStorage[i] = argument.Type.Storage }
		callable.signature.ArgumentCount = uint8(len(arguments))
	} else {
		callable.argumentTypes = make([]model.Type, len(arguments))
		for i, argument := range arguments { callable.argumentTypes[i] = argument.Type }
	}
	return callable
}

func resolveNativeTextConcat(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != 2 || result.Type.ID != 25 { return Callable{}, sqlError("42804", "polymorphic text concatenation signature mismatch") }
	index := 1; if signature.ID == 2004 { index = 0 }
	codec, err := ResolveTypeTextCodec(ctx, arguments[index], options, maximum)
	if err != nil { return Callable{}, err }
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		formatted, err := codec.Output(ctx, values[index:index+1], budget)
		if err != nil { return model.Value{}, err }
		if signature.ID == 2003 { return ownedText(ctx, budget.OutputBytes, values[0].StrVal, formatted.StrVal) }
		return ownedText(ctx, budget.OutputBytes, formatted.StrVal, values[1].StrVal)
	}
	return nativeTypedCallable(signature, arguments, result, kernel, codec.OwnedBytes), nil
}

func resolveNativeRecordImage(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != 2 || result.Type.ID != signature.Result { return Callable{}, sqlError("42804", "record image comparison signature mismatch") }
	left, err := ResolveTypeBinaryCodec(ctx, arguments[0], options, maximum); if err != nil { return Callable{}, err }
	if left.OwnedBytes > maximum { return Callable{}, sqlError("54000", "record image metadata budget exceeded") }
	right, err := ResolveTypeBinaryCodec(ctx, arguments[1], options, maximum-left.OwnedBytes); if err != nil { return Callable{}, err }
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		a, err := left.Output(ctx, values[:1], budget); if err != nil { return model.Value{}, err }
		nested, err := budget.After(uint64(len(a.BytesVal))); if err != nil { return model.Value{}, err }
		b, err := right.Output(ctx, values[1:], nested); if err != nil { return model.Value{}, err }
		order, err := compareBytes(ctx, a.BytesVal, b.BytesVal); if err != nil { return model.Value{}, err }
		switch signature.ID { case 3181: return model.BoolVal(order == 0), nil; case 3182: return model.BoolVal(order != 0), nil; case 3183: return model.BoolVal(order < 0), nil; case 3184: return model.BoolVal(order > 0), nil; case 3185: return model.BoolVal(order <= 0), nil; case 3186: return model.BoolVal(order >= 0), nil; default: return model.Int64Val(int64(order)), nil }
	}
	return nativeTypedCallable(signature, arguments, result, kernel, left.OwnedBytes+right.OwnedBytes), nil
}

func resolveNativeRecordExtremum(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	comparison, _ := resolveSignature(2987, options)
	inner, err := resolveRecordComparison(ctx, comparison, arguments, model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}, options, maximum)
	if err != nil { return Callable{}, err }
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) { order, err := inner.Call(ctx, values, budget.WithOutput(0)); if err != nil { return model.Value{}, err }; selected := values[0]; if signature.ID == 6375 && order.IntVal < 0 || signature.ID == 6376 && order.IntVal > 0 { selected = values[1] }; owned, _, err := model.CopyValue(ctx, selected, uint64(max(budget.OutputBytes, 0))); if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "record extremum exceeds output budget", Cause: err} }; return owned, nil }
	return nativeTypedCallable(signature, arguments, result, kernel, inner.OwnedBytes()), nil
}

func resolveNativeArrayRandom(signature Signature, arguments []model.TypeLayout, result model.TypeLayout) (Callable, error) {
	if len(arguments) < 1 || len(arguments) > 2 || arguments[0].Type.Storage != model.TypeArray || result.Type.ID != arguments[0].Type.ID { return Callable{}, sqlError("42804", "array random signature mismatch") }
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		if budget.Native.Random == nil { return model.Value{}, sqlError("55000", "session random runtime is unavailable") }
		input := values[0]
		if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil { return model.Value{}, sqlError("2202E", "invalid array dimensions") }
		count := 0; if len(input.Dimensions) != 0 { count = int(input.Dimensions[0].Length) }
		selected := count
		if signature.ID == 6216 { selected = int(values[1].IntVal); if selected < 0 || selected > count { return model.Value{}, sqlError("22023", "sample size must be between zero and the array length") } }
		owned, _, err := model.CopyValue(ctx, input, uint64(max(budget.OutputBytes, 0))); if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "random array exceeds output budget", Cause: err} }
		block := 1; for _, dimension := range owned.Dimensions[1:] { block *= int(dimension.Length) }
		for i := count-1; i > 0; i-- { bits, err := budget.Native.Random.RandomUint64(ctx); if err != nil { return model.Value{}, err }; j := int(bits % uint64(i+1)); for k := 0; k < block; k++ { owned.ArrayVal[i*block+k], owned.ArrayVal[j*block+k] = owned.ArrayVal[j*block+k], owned.ArrayVal[i*block+k] } }
		if signature.ID == 6216 && len(owned.Dimensions) != 0 { owned.Dimensions[0].Length = int32(selected); owned.ArrayVal = owned.ArrayVal[:selected*block] }
		return owned, nil
	}
	return nativeTypedCallable(signature, arguments, result, kernel, 0), nil
}

type nativeHashPartitionField struct { storage model.DataType; callable Callable }

func resolveNativeHashPartition(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) < 4 || result.Type.ID != 16 { return Callable{}, sqlError("42804", "hash partition signature mismatch") }
	fields := make([]nativeHashPartitionField, len(arguments)-3)
	metadata := uint64(len(arguments))*uint64(unsafe.Sizeof(model.Type{})) + uint64(len(fields))*uint64(unsafe.Sizeof(nativeHashPartitionField{}))
	if metadata > maximum { return Callable{}, sqlError("54000", "hash partition metadata budget exceeded") }
	for i := range fields { callback, err := resolveNestedHashField(ctx, arguments[i+3], true, false, options, maximum-metadata); if err != nil { return Callable{}, err }; fields[i] = nativeHashPartitionField{storage: arguments[i+3].Type.Storage, callable: callback}; metadata += callback.OwnedBytes() }
	kernel := func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		modulus, remainder := values[1].IntVal, values[2].IntVal
		if modulus <= 0 || remainder < 0 || remainder >= modulus { return model.Value{}, sqlError("22023", "invalid hash partition modulus or remainder") }
		hash := uint64(0x7A5B22367996DCFD)
		for i, field := range fields { value := values[i+3]; if value.IsNull { continue }; if value.Type != field.storage { return model.Value{}, sqlError("42804", "hash partition argument differs from bound type") }; result, err := field.callable.Call(ctx, []model.Value{value, model.Int64Val(0)}, budget.WithOutput(0)); if err != nil { return model.Value{}, err }; hash ^= uint64(result.IntVal) + 0x49a0f4dd15e5a8e3 + (hash << 54) + (hash >> 7) }
		return model.BoolVal(hash%uint64(modulus) == uint64(remainder)), nil
	}
	callable := nativeTypedCallable(signature, arguments, result, kernel, metadata)
	if len(arguments) <= 8 { callable.argumentTypes = make([]model.Type, len(arguments)); for i, argument := range arguments { callable.argumentTypes[i] = argument.Type } }
	return callable, nil
}

