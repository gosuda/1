package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBExists(signature Signature) scalarKernel {
	var multiple, all bool
	switch signature.ID {
	case 4047:
	case 4048:
		multiple = true
	case 4049:
		multiple, all = true, true
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		if multiple {
			array := &args[1]
			if err := model.ValidateArrayShape(array.Dimensions, uint64(len(array.ArrayVal))); err != nil {
				return model.Value{}, sqlError("2202E", "invalid array dimensions")
			}
			for i := range array.ArrayVal {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				if value := &array.ArrayVal[i]; !value.IsNull && value.Type != model.TypeString {
					return model.Value{}, sqlError("42804", "JSONB existence requires text elements")
				}
			}
		}
		text := args[0].StrVal
		// Validate even when the keys are empty or membership can short-circuit.
		shape, err := walkJSONB(ctx, text, nil)
		if err != nil {
			return model.Value{}, err
		}
		if !multiple {
			matched, err := jsonbExistsKey(ctx, text, shape.kind, args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			return model.BoolVal(matched), nil
		}
		for i := range args[1].ArrayVal {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			key := &args[1].ArrayVal[i]
			if key.IsNull {
				continue
			}
			matched, err := jsonbExistsKey(ctx, text, shape.kind, key.StrVal)
			if err != nil {
				return model.Value{}, err
			}
			if matched != all {
				return model.BoolVal(matched), nil
			}
		}
		return model.BoolVal(all), nil
	}
}

// The document has already passed walkJSONB. Rescanning each key avoids a
// document-sized index or per-key state for a fixed-width result.
func jsonbExistsKey(ctx context.Context, text string, root jsonTextKind, key string) (bool, error) {
	if root != jsonTextObject && root != jsonTextArray && root != jsonTextString {
		return false, ctx.Err()
	}
	// JSONB cannot contain NUL; exclude it before using the C-string matcher.
	for start := 0; start < len(key); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if strings.IndexByte(key[start:min(start+cancellationChunk, len(key))], 0) >= 0 {
			return false, nil
		}
	}
	scanner := jsonTextScanner{text: text}
	depth := 0
	for {
		token, err := nextJSONExtractToken(ctx, &scanner, false, nil)
		if err != nil {
			return false, err
		}
		if token.start == token.end {
			return false, ctx.Err()
		}
		switch token.kind {
		case jsonTextObject, jsonTextArray:
			depth++
		case jsonTextString:
			candidate := depth == 0 || depth == 1 && root == jsonTextArray
			if depth == 1 && root == jsonTextObject {
				scanner.space(ctx)
				candidate = scanner.peek() == ':'
			}
			if candidate {
				matched, err := jsonStringEquals(ctx, text[token.start:token.end], key)
				if err != nil || matched {
					return matched, err
				}
			}
		}
		if token.punctuation == '}' || token.punctuation == ']' {
			depth--
		}
	}
}
