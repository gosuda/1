package builtin

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ResolveTyped borrows layouts only during resolution. Retained type metadata
// is bounded by maxMetadataBytes and reported by Callable.OwnedBytes.
func ResolveTyped(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maxMetadataBytes uint64) (Callable, error) {
	if err := ctx.Err(); err != nil {
		return Callable{}, err
	}
	signature, known := resolveSignature(id, options)
	if !known {
		return Callable{}, &UnknownFunctionError{ID: id}
	}
	if id == 3198 || id == 3200 || id == 3271 || id == 3273 {
		return resolveJSONBBuild(ctx, signature, arguments, result, options, maxMetadataBytes, false)
	}
	if primitiveOperation(id) == opJSONConversion {
		return resolveJSONConversion(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if primitiveOperation(id) == opNullCount {
		return resolveNullCount(ctx, signature, arguments, result, options, maxMetadataBytes, false)
	}
	if _, supported := variadicTextPrefix(id); supported {
		return resolveVariadicText(ctx, signature, arguments, result, options, maxMetadataBytes, false)
	}
	if nativeScalarTyped(id) {
		return resolveNativeTyped(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if primitiveOperation(id) == opContextOnly {
		if len(arguments) != int(signature.ArgumentCount) || len(arguments) > 8 {
			return Callable{}, sqlError("42883", "builtin argument count does not match bound signature")
		}
		remaining := maxMetadataBytes
		for index, layout := range arguments {
			expected := signature.Arguments[index]
			info, pseudo := LookupType(expected)
			if layout.Type.ID != expected && (!pseudo || info.Kind != 'p' || expected == 2281 || expected == 2275) { return Callable{}, sqlError("42804", "builtin argument type does not match bound signature") }
			if expected == 2281 && layout.Type.Storage != model.TypeBytes { return Callable{}, sqlError("42804", "internal aggregate state requires executor-owned bytes storage") }
			used, err := model.TypeLayoutBytes(ctx, layout, remaining); if err != nil { return Callable{}, codecLayoutError(err) }
			remaining -= used
			if err := validateCatalogVectorLayout(layout); err != nil { return Callable{}, err }
		}
		expected, info, pseudo := signature.Result, TypeInfo{}, false
		if expected != result.Type.ID { info, pseudo = LookupType(expected) }
		if expected != result.Type.ID && (!pseudo || info.Kind != 'p' || expected == 2281 || expected == 2275) { return Callable{}, sqlError("42804", "builtin result type does not match bound signature") }
		if expected == 2281 && result.Type.Storage != model.TypeBytes { return Callable{}, sqlError("42804", "internal aggregate state requires executor-owned bytes storage") }
		if _, err := model.TypeLayoutBytes(ctx, result, remaining); err != nil { return Callable{}, codecLayoutError(err) }
		if err := validateCatalogVectorLayout(result); err != nil { return Callable{}, err }
		return nativeTypedCallable(signature, arguments, result, compileContextOnly(signature), 0), nil
	}
	if len(arguments) != int(signature.ArgumentCount) || len(arguments) > 8 {
		return Callable{}, sqlError("42883", "builtin argument count does not match bound signature")
	}
	remaining := maxMetadataBytes
	var types [8]model.Type
	for i, layout := range arguments {
		used, err := model.TypeLayoutBytes(ctx, layout, remaining)
		if err != nil {
			return Callable{}, codecLayoutError(err)
		}
		remaining -= used
		if err := validateCatalogVectorLayout(layout); err != nil {
			return Callable{}, err
		}
		types[i] = layout.Type
	}
	if _, err := model.TypeLayoutBytes(ctx, result, remaining); err != nil {
		return Callable{}, codecLayoutError(err)
	}
	if err := validateCatalogVectorLayout(result); err != nil {
		return Callable{}, err
	}
	if introspectionScalarFunction(id) {
		return resolveIntrospectionScalar(signature, arguments, result, options)
	}
	if id >= 3504 && id <= 3533 {
		if callable, resolved, err := resolveEnumCall(ctx, signature, arguments, result, options, maxMetadataBytes); resolved {
			return callable, err
		}
	}
	if id == 2597 {
		if callable, resolved, err := resolveDomainInput(ctx, signature, arguments, result, options, maxMetadataBytes); resolved {
			return callable, err
		}
	}
	if callable, resolved, err := resolveDomainOutput(ctx, signature, arguments, result, options, maxMetadataBytes); resolved {
		return callable, err
	}
	switch primitiveOperation(id) {
	case opJSONRecord:
		return resolveJSONRecordScalar(ctx, signature, arguments, result, options, maxMetadataBytes)
	case opRangeScalar:
		return resolveRangeScalar(ctx, signature, arguments, result, options, maxMetadataBytes)
	case opMultirangeScalar:
		return resolveMultirangeScalar(ctx, signature, arguments, result, options, maxMetadataBytes)
	case opRangeIO:
		return resolveRangeIO(ctx, signature, arguments, result, options, maxMetadataBytes)
	case opRangeHash:
		return resolveRangeHash(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if primitiveOperation(id) == opNestedHash {
		return resolveNestedHash(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if primitiveOperation(id) == opArraySort {
		return resolveArraySort(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 2290 {
		return resolveRecordInput(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 2291 {
		return resolveRecordOutput(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if primitiveOperation(id) == opRecordComparison {
		return resolveRecordComparison(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 1285 || id == 1290 {
		return resolveQuoteTyped(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 3218 {
		return resolveWidthBucketArray(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 750 || id == 751 {
		return resolveArrayIO(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if op := primitiveOperation(id); op == opArrayComparison || op == opArrayMembership {
		return resolveArrayComparison(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 1193 || id == 1286 {
		return resolveArrayFill(signature, arguments, result, options)
	}
	if id == 384 || id == 395 {
		return resolveArrayToString(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 6172 {
		return resolveArrayTrim(signature, arguments, result, options)
	}
	if id == 6381 {
		return resolveArrayReverse(signature, arguments, result, options)
	}
	if op := primitiveOperation(id); op == opArraySearch || op == opArrayReplace {
		return resolveArraySearch(ctx, signature, arguments, result, options, maxMetadataBytes)
	}
	if id == 378 || id == 379 || id == 383 {
		return resolveArrayConstruction(signature, arguments, result, options)
	}
	if id == 376 || id == 394 {
		_, element, err := arrayCodecLayout(result, options)
		if err != nil {
			return Callable{}, err
		}
		if element.Type.ID != 25 || element.Type.Storage != model.TypeString {
			return Callable{}, sqlError("42804", "string splitting requires a text array result")
		}
	}
	callable, err := Resolve(id, options)
	if err != nil {
		return Callable{}, err
	}
	if err := callable.ValidateTypes(types[:len(arguments)], result.Type); err != nil {
		return Callable{}, err
	}
	return callable, nil
}

func validateCatalogVectorLayout(layout model.TypeLayout) error {
	if layout.Type.ID != 22 && layout.Type.ID != 30 {
		return nil
	}
	_, element, err := arrayCodecLayout(layout, ResolveOptions{})
	if err != nil {
		return err
	}
	if element.Type.Storage != model.TypeInt64 || len(element.Children) != 0 {
		return sqlError("42804", "catalog vector requires its declared integer element layout")
	}
	return nil
}

func codecLayoutError(err error) error {
	if errors.Is(err, model.ErrValueBudget) {
		return &model.Error{Code: "54000", Message: "builtin type metadata budget exceeded", Cause: err}
	}
	if errors.Is(err, model.ErrInvalidTypeLayout) {
		return &model.Error{Code: "42804", Message: "invalid builtin type layout", Cause: err}
	}
	return err
}

type arrayIOCodec struct {
	element     Callable
	elementType model.Type
	ioParameter model.TypeID
	delimiter   byte
	nulls       bool
}

func resolveArrayIO(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	array := result
	if signature.ID == 751 {
		array = arguments[0]
	}
	info, elementLayout, err := arrayCodecLayout(array, options)
	if err != nil {
		return Callable{}, err
	}
	if signature.ID == 750 && info.Input != 750 {
		return Callable{}, sqlError("42804", "array input result has incompatible array representation")
	}
	for i, argument := range arguments {
		if signature.Arguments[i] == 2277 {
			continue
		}
		if argument.Type.ID != signature.Arguments[i] || argument.Type.Storage != primitiveStorage(signature.Arguments[i]) {
			return Callable{}, sqlError("42804", "array I/O metadata argument mismatch")
		}
	}
	if signature.ID == 751 && (result.Type.ID != 2275 || result.Type.Storage != model.TypeString) {
		return Callable{}, sqlError("42804", "array output requires cstring result")
	}
	elementInfo, known := lookupResolvedTypeIO(options, elementLayout)
	if !known || elementInfo.Delimiter == 0 {
		return Callable{}, sqlError("42804", "array element has no resolved I/O metadata")
	}
	metadata := uint64(unsafe.Sizeof(arrayIOCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "array codec metadata budget exceeded")
	}
	codec := &arrayIOCodec{elementType: elementLayout.Type, ioParameter: elementInfo.Parameter, delimiter: elementInfo.Delimiter, nulls: !options.ArrayNullsDisabled}
	var element Callable
	if signature.ID == 750 {
		element, err = resolveElementInput(ctx, elementInfo.Input, elementLayout, options, maximum-metadata)
	} else {
		element, err = ResolveTyped(ctx, elementInfo.Output, []model.TypeLayout{elementLayout}, model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}, options, maximum-metadata)
	}
	if err != nil {
		return Callable{}, err
	}
	codec.element = element
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, metadataBytes: metadata + element.OwnedBytes()}
	callable.signature.Result = result.Type.ID
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	if signature.ID == 750 {
		callable.kernel = codec.input
	} else {
		callable.kernel = codec.output
	}
	return callable, nil
}

func resolveElementInput(ctx context.Context, id model.FunctionID, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	signature, known := LookupSignature(id)
	if !known {
		return Callable{}, &UnknownFunctionError{ID: id}
	}
	if signature.Arguments[0] != 2275 || signature.ArgumentCount < 1 || signature.ArgumentCount > 3 {
		return Callable{}, sqlError("42804", "unsupported array element input signature")
	}
	var arguments [3]model.TypeLayout
	for i := range signature.ArgumentCount {
		arguments[i].Type = model.Type{ID: signature.Arguments[i], Modifier: -1, Storage: primitiveStorage(signature.Arguments[i])}
	}
	return ResolveTyped(ctx, id, arguments[:signature.ArgumentCount], result, options, maximum)
}

func (codec *arrayIOCodec) input(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	if arguments[1].IntVal != int64(codec.elementType.ID) {
		return model.Value{}, sqlError("42804", "array input element OID differs from bound type")
	}
	if codec.elementType.Modifier != -1 && arguments[2].IntVal != int64(codec.elementType.Modifier) {
		return model.Value{}, sqlError("42804", "array input typmod differs from bound element layout")
	}
	element := arrayElementCall{function: codec.element}
	element.arguments[1] = model.Int64Val(int64(codec.ioParameter))
	element.arguments[2] = arguments[2]
	return parseArrayText(ctx, arguments[0].StrVal, codec.delimiter, codec.nulls, &element, callBudget)
}

func (codec *arrayIOCodec) output(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	element := arrayElementCall{function: codec.element}
	return formatArrayText(ctx, arguments[0], codec.delimiter, &element, callBudget)
}
