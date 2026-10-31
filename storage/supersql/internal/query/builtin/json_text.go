package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONText(signature Signature) scalarKernel {
	switch signature.ID {
	case 321:
		return jsonTextInput
	case 322:
		return copyCString
	case 324:
		return jsonTextSend
	case 3968:
		return jsonTextType
	case 3956:
		return jsonTextArrayLength
	default:
		return nil
	}
}

func jsonTextInput(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	value, err := copyCString(ctx, args, callBudget)
	if err != nil {
		return model.Value{}, err
	}
	if _, _, err := scanJSONText(ctx, value.StrVal); err != nil {
		return model.Value{}, err
	}
	value.Type = model.TypeJSONB
	return value, nil
}

func jsonTextSend(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input := args[0].StrVal
	if err := outputBudget(int64(len(input)), budget); err != nil {
		return model.Value{}, err
	}
	output := make([]byte, len(input))
	for offset := 0; offset < len(input); {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		end := offset + min(cancellationChunk, len(input)-offset)
		copy(output[offset:end], input[offset:end])
		offset = end
	}
	return model.BytesVal(output), ctx.Err()
}

func jsonTextType(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	scanner := jsonTextScanner{text: args[0].StrVal}
	scanner.space(ctx)
	kind, err := scanner.value(ctx)
	if err != nil {
		return model.Value{}, err
	}
	var name string
	switch kind {
	case jsonTextObject:
		name = "object"
	case jsonTextArray:
		name = "array"
	case jsonTextString:
		name = "string"
	case jsonTextNumber:
		name = "number"
	case jsonTextBoolean:
		name = "boolean"
	case jsonTextNull:
		name = "null"
	default:
		return model.Value{}, sqlError("XX000", "unexpected JSON token")
	}
	return ownedText(ctx, budget, name)
}

func jsonTextArrayLength(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	scanner := jsonTextScanner{text: args[0].StrVal}
	scanner.space(ctx)
	kind, err := scanner.value(ctx)
	if err != nil {
		return model.Value{}, err
	}
	if kind != jsonTextArray {
		return model.Value{}, sqlError("22023", "cannot get array length of a non-array")
	}
	_, count, err := scanJSONText(ctx, args[0].StrVal)
	if err != nil {
		return model.Value{}, err
	}
	if count > math.MaxInt32 {
		return model.Value{}, sqlError("54000", "JSON array length exceeds int4")
	}
	return model.Int64Val(count), nil
}
