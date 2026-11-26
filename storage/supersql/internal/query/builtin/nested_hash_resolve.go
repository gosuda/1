package builtin

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type nestedHash struct {
	array    bool
	extended bool
	fields   []nestedHashField
}

type nestedHashField struct {
	storage  model.DataType
	callable Callable
	err      error
}

func resolveNestedHash(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	array := signature.ID == 626 || signature.ID == 782
	extended := signature.ID == 782 || signature.ID == 6193
	if !array && signature.ID != 6192 && signature.ID != 6193 {
		return Callable{}, sqlError("42883", "unknown nested hash function")
	}
	count := 1
	resultID := model.TypeID(23)
	if extended {
		count, resultID = 2, 20
	}
	if len(arguments) != count || result.Type.ID != resultID || result.Type.Storage != model.TypeInt64 || len(result.Children) != 0 {
		return Callable{}, sqlError("42804", "nested hash signature mismatch")
	}
	if extended {
		seed := arguments[1]
		if seed.Type.ID != 20 || seed.Type.Storage != model.TypeInt64 || len(seed.Children) != 0 {
			return Callable{}, sqlError("42804", "extended hash requires an int8 seed")
		}
	}
	// Validate depth before any recursive resolution; layouts remain borrowed.
	if _, err := model.TypeLayoutBytes(ctx, arguments[0], maximum); err != nil {
		return Callable{}, codecLayoutError(err)
	}
	if err := validateConcreteLayout(ctx, arguments[0]); err != nil {
		return Callable{}, err
	}
	storage := model.TypeComposite
	if array {
		storage = model.TypeArray
	}
	if arguments[0].Type.Storage != storage {
		return Callable{}, sqlError("42804", "nested hash argument does not match its bound layout")
	}
	logical, err := resolvedStorageLayout(arguments[0], storage, options)
	if err != nil {
		return Callable{}, err
	}
	children := logical.Children
	metadata := uint64(unsafe.Sizeof(nestedHash{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	errorReserve := max(uint64(unsafe.Sizeof(model.Error{})), uint64(unsafe.Sizeof(MissingImplementationError{}))) + 128
	fieldBytes := uint64(unsafe.Sizeof(nestedHashField{})) + errorReserve
	if metadata > maximum || uint64(len(children)) > (maximum-metadata)/fieldBytes {
		return Callable{}, sqlError("54000", "nested hash metadata budget exceeded")
	}
	metadata += uint64(len(children)) * fieldBytes
	codec := &nestedHash{array: array, extended: extended, fields: make([]nestedHashField, len(children))}
	for i, child := range children {
		if err := ctx.Err(); err != nil {
			return Callable{}, err
		}
		fieldOptions := options
		if !array {
			fieldOptions.Collation = child.Type.Collation
		}
		field := &codec.fields[i]
		field.storage = child.Type.Storage
		callback, err := resolveNestedHashField(ctx, child, extended, array && !extended && child.Type.ID == 2249, fieldOptions, maximum-metadata+errorReserve)
		if err != nil {
			owned, deferred := nestedHashErrorBytes(err)
			if !deferred {
				return Callable{}, err
			}
			if owned > errorReserve {
				if owned-errorReserve > maximum-metadata {
					return Callable{}, sqlError("54000", "nested hash error metadata budget exceeded")
				}
				metadata += owned - errorReserve
			}
			field.err = err
			continue
		}
		if callback.OwnedBytes() > errorReserve {
			additional := callback.OwnedBytes() - errorReserve
			if additional > maximum-metadata {
				return Callable{}, sqlError("54000", "nested hash metadata budget exceeded")
			}
			metadata += additional
		}
		field.callable = callback
	}
	callable := Callable{signature: signature, resultStorage: model.TypeInt64, kernel: codec.hash, metadataBytes: metadata}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	return callable, nil
}

func resolveNestedHashField(ctx context.Context, layout model.TypeLayout, extended, anonymousArrayRecord bool, options ResolveOptions, maximum uint64) (Callable, error) {
	logical, err := resolvedDomainBaseLayout(layout, options)
	if err != nil {
		return Callable{}, err
	}
	class, supported, err := nestedHashClass(ctx, logical, extended, anonymousArrayRecord, options, 0)
	if err != nil {
		return Callable{}, err
	}
	if !supported {
		if uint64(unsafe.Sizeof(model.Error{}))+128 > maximum {
			return Callable{}, sqlError("54000", "nested hash error metadata budget exceeded")
		}
		kind := "a hash"
		if extended {
			kind = "an extended hash"
		}
		return Callable{}, sqlError("42883", fmt.Sprintf("could not identify %s function for type %d", kind, layout.Type.ID))
	}
	function := class.HashFunction
	output := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	operands := [2]model.TypeLayout{logical, {Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}}
	count := 1
	if extended {
		function, output.Type.ID, count = class.HashExtendedFunction, 20, 2
	}
	if class.InputType != 2277 && class.InputType != 2249 && class.InputType != 3500 && class.InputType != 3831 && class.InputType != 4537 {
		operands[0].Type.ID = class.InputType
		operands[0].Type.Modifier = -1
	}
	return ResolveTyped(ctx, function, operands[:count], output, options, maximum)
}

// PostgreSQL's type cache withholds generic hash support when any declared
// component lacks it. hash_array alone overrides this for anonymous records.
func nestedHashClass(ctx context.Context, layout model.TypeLayout, extended, anonymousArrayRecord bool, options ResolveOptions, depth int) (OperatorClassInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return OperatorClassInfo{}, false, err
	}
	if depth > 64 {
		return OperatorClassInfo{}, false, sqlError("54000", "nested hash type depth exceeds limit")
	}
	class, found, err := defaultComparisonClass(ctx, "hash", layout.Type, options)
	if err != nil || !found {
		return class, false, err
	}
	function := class.HashFunction
	if extended {
		function = class.HashExtendedFunction
	}
	if function == 0 {
		return class, false, nil
	}
	switch function {
	case 6192, 6193:
		if layout.Type.ID == 2249 {
			return class, anonymousArrayRecord, nil
		}
	case 626, 782:
	default:
		return class, true, nil
	}
	for _, child := range layout.Children {
		_, supported, err := nestedHashClass(ctx, child, extended, false, options, depth+1)
		if err != nil || !supported {
			return class, false, err
		}
	}
	return class, true, nil
}

func nestedHashErrorBytes(err error) (uint64, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false
	}
	switch failure := err.(type) {
	case *MissingImplementationError:
		return uint64(unsafe.Sizeof(*failure)) + uint64(len(failure.Signature.Name)), true
	case *model.Error:
		deferred := failure.Code == "42883" || failure.Code == "42804" || failure.Code == "42P22"
		if failure.Cause != nil || !deferred {
			return 0, false
		}
		return uint64(unsafe.Sizeof(*failure)) + uint64(len(failure.Code)) + uint64(len(failure.Message)) + uint64(len(failure.Detail)) + uint64(len(failure.FeatureID)), true
	default:
		return 0, false
	}
}
