package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBPath(signature Signature) scalarKernel {
	if signature.ID != 3217 && signature.ID != 3940 {
		return nil
	}
	asText := signature.ID == 3940
	resultType := model.TypeJSONB
	if asText {
		resultType = model.TypeString
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		array := args[1]
		if err := model.ValidateArrayShape(array.Dimensions, uint64(len(array.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		invalidElement := false
		for _, element := range array.ArrayVal {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			if element.IsNull {
				return model.NullVal(resultType), nil
			}
			invalidElement = invalidElement || element.Type != model.TypeString
		}
		if invalidElement {
			return model.Value{}, sqlError("42804", "JSON path requires text elements")
		}
		text := args[0].StrVal
		span, err := selectJSONBPath(ctx, text, array.ArrayVal)
		if err != nil {
			return model.Value{}, err
		}
		return jsonbExtractResult(ctx, text, span, asText, resultType, budget)
	}
}

func selectJSONBPath(ctx context.Context, text string, path []model.Value) (jsonTextSpan, error) {
	// Validate before lookup or output admission, including values discarded by
	// duplicate keys. NULL path elements have already bypassed this conversion.
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return jsonTextSpan{}, err
	}
	span := jsonTextSpan{kind: shape.kind, end: len(text)}
	if len(path) == 0 {
		if shape.kind != jsonTextObject && shape.kind != jsonTextArray {
			scanner := jsonTextScanner{text: text}
			token, err := nextJSONExtractToken(ctx, &scanner, false, nil)
			return token.jsonTextSpan, err
		}
		return span, nil
	}
	// Resolve one prefix completely before its suffix: the last duplicate key
	// wins even when its value has no matching suffix. Rescanning each shrinking
	// subtree trades O(path depth * input bytes) work for bounded scanner state,
	// without an input-sized tree, copied path, or live temporary output.
	for _, element := range path {
		if err := ctx.Err(); err != nil {
			return jsonTextSpan{}, err
		}
		selector := jsonTextSelector{shape: span.kind, field: element.StrVal}
		switch span.kind {
		case jsonTextObject:
		case jsonTextArray:
			index, err := jsonPathIndex(ctx, element.StrVal)
			if err != nil {
				return jsonTextSpan{}, err
			}
			if index == math.MinInt32 {
				return jsonTextSpan{}, nil
			}
			selector.index = index
		default:
			// Path extraction never treats a root scalar as a one-element array.
			return jsonTextSpan{}, nil
		}
		child, err := selectJSONBExtract(ctx, text[span.start:span.end], selector)
		if err != nil || child.kind == 0 {
			return jsonTextSpan{}, err
		}
		child.start += span.start
		child.end += span.start
		span = child
	}
	return span, ctx.Err()
}
