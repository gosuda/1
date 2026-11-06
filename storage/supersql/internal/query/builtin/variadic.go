package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type variadicValues struct {
	values    []model.Value
	nullArray bool
}

// ResolveVariadic treats the final input as an explicit VARIADIC array. Ordinary
// array-valued arguments remain scalar inputs when resolved with ResolveTyped.
func ResolveVariadic(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maxMetadataBytes uint64) (Callable, error) {
	if err := ctx.Err(); err != nil {
		return Callable{}, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return Callable{}, &UnknownFunctionError{ID: id}
	}
	if signature.Variadic == 0 {
		return Callable{}, sqlError("42883", "function is not variadic")
	}
	if id == 3198 || id == 3200 || id == 3271 || id == 3273 {
		return resolveJSONBBuild(ctx, signature, arguments, result, options, maxMetadataBytes, true)
	}
	if primitiveOperation(id) == opNullCount {
		return resolveNullCount(ctx, signature, arguments, result, options, maxMetadataBytes, true)
	}
	if _, supported := variadicTextPrefix(id); !supported {
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	return resolveVariadicText(ctx, signature, arguments, result, options, maxMetadataBytes, true)
}

func validateVariadicLayouts(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, maximum uint64, prefix int, array bool, options ResolveOptions) (model.TypeLayout, error) {
	wrongArrayArity := array && len(arguments) != prefix+1
	if len(arguments) < int(signature.ArgumentCount) || len(arguments) > MaxCallArguments || wrongArrayArity {
		return model.TypeLayout{}, sqlError("42883", "builtin argument count does not match bound signature")
	}
	remaining := maximum
	for _, argument := range arguments {
		used, err := model.TypeLayoutBytes(ctx, argument, remaining)
		if err != nil {
			return model.TypeLayout{}, codecLayoutError(err)
		}
		remaining -= used
		if err := validateConcreteLayout(ctx, argument); err != nil {
			return model.TypeLayout{}, err
		}
	}
	if _, err := model.TypeLayoutBytes(ctx, result, remaining); err != nil {
		return model.TypeLayout{}, codecLayoutError(err)
	}
	if !array {
		return model.TypeLayout{}, nil
	}
	_, element, err := arrayCodecLayout(arguments[prefix], options)
	return element, err
}

func validateConcreteLayout(ctx context.Context, layout model.TypeLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch layout.Type.ID {
	case 2276:
		return sqlError("42P18", "could not determine variadic argument type")
	case 2277, 2283, 2776, 3500, 3831, 4537, 4538, 5077, 5078, 5079, 5080:
		return sqlError("42804", "variadic argument requires a concrete SQL type")
	}
	if expected := primitiveStorage(layout.Type.ID); expected != model.TypeAny && layout.Type.Storage != expected {
		return sqlError("42804", "datum storage differs from its SQL type")
	}
	// Array identities carry an element declaration; reject a layout whose
	// element contradicts the catalog identity before any kernel is selected.
	if layout.Type.Storage == model.TypeArray {
		if _, _, err := arrayCodecLayout(layout, ResolveOptions{}); err != nil {
			return err
		}
	}
	for _, child := range layout.Children {
		if err := validateConcreteLayout(ctx, child); err != nil {
			return err
		}
	}
	return nil
}

func decodeVariadicValues(arguments []model.Value, prefix int, array bool) (variadicValues, error) {
	if prefix < 0 || prefix > len(arguments) {
		return variadicValues{}, sqlError("XX000", "invalid variadic argument prefix")
	}
	if !array {
		return variadicValues{values: arguments[prefix:]}, nil
	}
	if prefix+1 != len(arguments) {
		return variadicValues{}, sqlError("XX000", "invalid variadic array argument plan")
	}
	value := arguments[prefix]
	if value.Type != model.TypeArray {
		return variadicValues{}, sqlError("42804", "variadic argument must be an array")
	}
	if value.IsNull {
		return variadicValues{nullArray: true}, nil
	}
	if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
		return variadicValues{}, sqlError("2202E", "invalid array dimensions")
	}
	return variadicValues{values: value.ArrayVal}, nil
}
