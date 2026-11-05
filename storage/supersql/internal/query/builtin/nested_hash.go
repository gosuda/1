package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (codec *nestedHash) hash(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	value := arguments[0]
	var hash uint64
	if codec.array {
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		// Lookup precedes iteration, including empty arrays and NULL elements.
		if codec.fields[0].err != nil {
			return model.Value{}, codec.fields[0].err
		}
		hash = 1
	} else if len(value.Dimensions) != 0 || len(value.ArrayVal) != len(codec.fields) {
		return model.Value{}, sqlError("42804", "record value does not match its bound field layout")
	}
	var operands [2]model.Value
	count := 1
	if codec.extended {
		operands[1] = arguments[1]
		count = 2
	}
	for i, value := range value.ArrayVal {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		index := i
		if codec.array {
			index = 0
		}
		field := &codec.fields[index]
		// Record hash support is checked in field order, before NULL handling.
		if field.err != nil {
			return model.Value{}, field.err
		}
		if value.Type != field.storage {
			return model.Value{}, sqlError("42804", "nested hash value storage differs from its bound type")
		}
		var elementHash uint64
		if !value.IsNull {
			operands[0] = value
			result, err := field.callable.Call(ctx, operands[:count], callBudget.WithOutput(0))
			operands[0] = model.Value{}
			if err != nil {
				return model.Value{}, err
			}
			wrongWidth := !codec.extended && (result.IntVal < math.MinInt32 || result.IntVal > math.MaxInt32)
			if result.IsNull || result.Type != model.TypeInt64 || wrongWidth {
				return model.Value{}, sqlError("XX000", "hash support function did not return a nonnull integer of the declared width")
			}
			elementHash = uint64(result.IntVal)
		}
		hash = hash*31 + elementHash
	}
	if codec.extended {
		return model.Int64Val(int64(hash)), nil
	}
	return model.Int64Val(int64(int32(hash))), nil
}
