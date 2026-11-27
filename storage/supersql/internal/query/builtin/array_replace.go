package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// featureMultidimArrayRemove names the embedded_pg18 profile exclusion the
// array_replace kernel raises for element removal on multidimensional
// arrays. The bind package owns the canonical FeatureID
// (bind.MultidimArrayRemove) but cannot be imported here: bind imports
// builtin. gates_test.go's manifest-driven exclusion table fails if this id
// drifts from testdata/postgres/embedded_features.json.
const featureMultidimArrayRemove = "multidim_array_remove"

func compileArrayReplace(signature Signature, element Callable, lookupErr error) scalarKernel {
	remove := signature.ID == 3167
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		input := arguments[0]
		if input.IsNull {
			return model.NullVal(model.TypeArray), nil
		}
		if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if len(input.ArrayVal) == 0 {
			return model.Value{Type: model.TypeArray}, nil
		}
		if remove && len(input.Dimensions) > 1 {
		return model.Value{}, &model.Error{Code: "0A000", Message: "removing elements from multidimensional arrays is not supported", FeatureID: featureMultidimArrayRemove}
		}
		if lookupErr != nil {
			return model.Value{}, lookupErr
		}
		search := arguments[1]
		buffer := newArrayValueBuffer(len(input.Dimensions), budget)
		var operands [2]model.Value
		for i := range len(input.ArrayVal) {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			value := input.ArrayVal[i]
			match := value.IsNull && search.IsNull
			if !value.IsNull && !search.IsNull {
				operands[0], operands[1] = value, search
				nested, err := callBudget.After(uint64(cap(buffer.values))*uint64(arrayValueBytes) + buffer.payload)
				if err != nil {
					return model.Value{}, err
				}
				match, err = arrayElementEqual(ctx, element, &operands, nested)
				if err != nil {
					return model.Value{}, err
				}
			}
			if match {
				if remove {
					continue
				}
				value = arguments[2]
			}
			if err := buffer.append(ctx, value); err != nil {
				return model.Value{}, err
			}
		}
		if remove {
			dimensions := [1]model.ArrayDimension{{LowerBound: input.Dimensions[0].LowerBound, Length: int32(len(buffer.values))}}
			return buffer.finish(ctx, dimensions[:])
		}
		return buffer.finish(ctx, input.Dimensions)
	}
}
