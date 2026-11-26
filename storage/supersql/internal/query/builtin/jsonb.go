package builtin

import (
	"context"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONB(signature Signature) scalarKernel {
	switch signature.ID {
	case 3806:
		return jsonbInput
	case 3804:
		return jsonbOutput
	case 3803:
		return jsonbSend
	case 3210:
		return jsonbType
	case 3207:
		return jsonbArrayLength
	default:
		return nil
	}
}

func jsonbInput(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	text := args[0].StrVal
	for start := 0; start < len(text); {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		end := start + min(cancellationChunk, len(text)-start)
		if offset := strings.IndexByte(text[start:end], 0); offset >= 0 {
			text = text[:start+offset]
			break
		}
		start = end
	}
	canonical, err := canonicalJSONB(ctx, text, budget, false)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeJSONB, StrVal: canonical}, nil
}

func jsonbOutput(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	canonical, err := canonicalJSONB(ctx, args[0].StrVal, budget, false)
	if err != nil {
		return model.Value{}, err
	}
	return model.StringVal(canonical), nil
}

func jsonbSend(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	canonical, err := canonicalJSONB(ctx, args[0].StrVal, budget, true)
	if err != nil {
		return model.Value{}, err
	}
	output := make([]byte, len(canonical)+1)
	output[0] = 1
	for start := 0; start < len(canonical); {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		end := start + min(cancellationChunk, len(canonical)-start)
		copy(output[start+1:end+1], canonical[start:end])
		start = end
	}
	return model.BytesVal(output), ctx.Err()
}

func jsonbType(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	shape, err := walkJSONB(ctx, args[0].StrVal, nil)
	if err != nil {
		return model.Value{}, err
	}
	var name string
	switch shape.kind {
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
	}
	return ownedText(ctx, budget, name)
}

func jsonbArrayLength(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	shape, err := walkJSONB(ctx, args[0].StrVal, nil)
	if err != nil {
		return model.Value{}, err
	}
	if shape.kind == jsonTextObject {
		return model.Value{}, sqlError("22023", "cannot get array length of a non-array")
	}
	if shape.kind != jsonTextArray {
		return model.Value{}, sqlError("22023", "cannot get array length of a scalar")
	}
	if shape.elements > math.MaxInt32 {
		return model.Value{}, sqlError("54000", "JSONB array length exceeds int4")
	}
	return model.Int64Val(shape.elements), nil
}
