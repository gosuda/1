package builtin

import (
	"cmp"
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type recordComparison struct {
	operation primitiveOp
	counts    [2]int
	fields    []recordFieldComparison
}

type recordFieldComparison struct {
	storage  [2]model.DataType
	callable Callable
	err      error
}

func resolveRecordComparison(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	extremum := signature.ID == 6375 || signature.ID == 6376
	if len(arguments) != 2 || (!extremum && (result.Type.ID != signature.Result || result.Type.Storage != primitiveStorage(signature.Result))) || (extremum && (result.Type.ID != arguments[0].Type.ID || result.Type.Storage != model.TypeComposite)) {
		return Callable{}, sqlError("42804", "record comparison signature mismatch")
	}
	var logical [2]model.TypeLayout
	for i, argument := range arguments {
		if argument.Type.Storage != model.TypeComposite {
			return Callable{}, sqlError("42804", "record comparison requires composite arguments")
		}
		if err := validateConcreteLayout(ctx, argument); err != nil {
			return Callable{}, err
		}
		resolved, err := resolvedStorageLayout(argument, model.TypeComposite, options)
		if err != nil {
			return Callable{}, err
		}
		logical[i] = resolved
	}
	var operation primitiveOp
	switch signature.ID {
	case 2981, 3181:
		operation = opEq
	case 2982, 3182:
		operation = opNe
	case 2983, 3183:
		operation = opLt
	case 2984, 3184:
		operation = opGt
	case 2985, 3185:
		operation = opLe
	case 2986, 3186:
		operation = opGe
	case 2987, 3187, 6375, 6376:
		operation = opCmp
	default:
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	count := min(len(logical[0].Children), len(logical[1].Children))
	metadata := uint64(unsafe.Sizeof(recordComparison{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	// A selected kernel can be absent even when an earlier field decides the
	// result. Admit its deferred error before asking the shared selector.
	errorReserve := max(uint64(unsafe.Sizeof(model.Error{})), uint64(unsafe.Sizeof(MissingImplementationError{}))) + 96
	fieldBytes := uint64(unsafe.Sizeof(recordFieldComparison{})) + errorReserve
	if metadata > maximum || uint64(count) > (maximum-metadata)/fieldBytes {
		return Callable{}, sqlError("54000", "record comparison metadata budget exceeded")
	}
	metadata += uint64(count) * fieldBytes
	codec := &recordComparison{
		operation: operation,
		counts:    [2]int{len(logical[0].Children), len(logical[1].Children)},
		fields:    make([]recordFieldComparison, count),
	}
	equality := operation == opEq || operation == opNe
	for i := range codec.fields {
		if err := ctx.Err(); err != nil {
			return Callable{}, err
		}
		left, right := logical[0].Children[i], logical[1].Children[i]
		field := &codec.fields[i]
		field.storage = [2]model.DataType{left.Type.Storage, right.Type.Storage}
		if left.Type.ID != right.Type.ID {
			field.err = sqlError("42804", "cannot compare dissimilar column types in a record")
			continue
		}
		fieldOptions := options
		fieldOptions.Collation = left.Type.Collation
		if left.Type.Collation != right.Type.Collation {
			fieldOptions.Collation = 0
		}
		selection, err := resolveArrayElementComparison(ctx, left, right, equality, fieldOptions, maximum-metadata+errorReserve)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Callable{}, err
			}
			var missing *MissingImplementationError
			var state *model.Error
			semanticError := errors.As(err, &state) && (state.Code == "42883" || state.Code == "42804")
			if errors.As(err, &missing) || semanticError {
				field.err = err
				continue
			}
			return Callable{}, err
		}
		if selection.metadataBytes > errorReserve {
			additional := selection.metadataBytes - errorReserve
			if additional > maximum-metadata {
				return Callable{}, sqlError("54000", "record comparison metadata budget exceeded")
			}
			metadata += additional
		}
		field.callable, field.err = selection.callable, selection.lookupErr
	}
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, kernel: codec.compare, metadataBytes: metadata}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = model.TypeComposite
	}
	return callable, nil
}

func (codec *recordComparison) compare(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	for i, argument := range arguments {
		if len(argument.Dimensions) != 0 || len(argument.ArrayVal) != codec.counts[i] {
			return model.Value{}, sqlError("42804", "record value does not match its bound field layout")
		}
	}
	left, right := arguments[0].ArrayVal, arguments[1].ArrayVal
	equality := codec.operation == opEq || codec.operation == opNe
	var operands [2]model.Value
	for i := range codec.fields {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		field := &codec.fields[i]
		// rowtypes.c checks field identity and operator availability even for
		// NULL pairs, but only after all preceding fields compared equal.
		if field.err != nil {
			return model.Value{}, field.err
		}
		if left[i].Type != field.storage[0] || right[i].Type != field.storage[1] {
			return model.Value{}, sqlError("42804", "record field storage differs from its bound type")
		}
		if left[i].IsNull && right[i].IsNull {
			continue
		}
		if left[i].IsNull {
			return comparisonResult(1, codec.operation), nil
		}
		if right[i].IsNull {
			return comparisonResult(-1, codec.operation), nil
		}
		operands[0], operands[1] = left[i], right[i]
		if equality {
			matched, err := arrayElementEqual(ctx, field.callable, &operands, callBudget)
			if err != nil {
				return model.Value{}, err
			}
			if !matched {
				return comparisonResult(1, codec.operation), nil
			}
			continue
		}
		result, err := field.callable.Call(ctx, operands[:], callBudget.WithOutput(0))
		clear(operands[:])
		if err != nil {
			return model.Value{}, err
		}
		if result.IsNull || result.Type != model.TypeInt64 || result.IntVal < math.MinInt32 || result.IntVal > math.MaxInt32 {
			return model.Value{}, sqlError("XX000", "record field comparison function did not return nonnull int4")
		}
		if result.IntVal != 0 {
			return comparisonResult(cmp.Compare(result.IntVal, 0), codec.operation), nil
		}
	}
	if codec.counts[0] != codec.counts[1] {
		return model.Value{}, sqlError("42804", "cannot compare record types with different numbers of columns")
	}
	return comparisonResult(0, codec.operation), nil
}
