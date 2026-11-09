package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonPathStep struct {
	field                 string
	index, current        int64
	matched, fieldMatches bool
}

type jsonExtraction struct {
	selector      *jsonTextSelector
	path          []jsonPathStep
	pathLength    int
	pathMode      bool
	fieldMatches  bool
	selectedDepth int
}

func compileJSONPath(signature Signature) scalarKernel {
	if signature.ID != 3951 && signature.ID != 3953 {
		return nil
	}
	asText := signature.ID == 3953
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
		for _, value := range array.ArrayVal {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			if value.IsNull {
				return model.NullVal(resultType), nil
			}
		}
		count := min(len(array.ArrayVal), jsonTextMaxDepth)
		metadata := int64(count) * int64(unsafe.Sizeof(jsonPathStep{}))
		if err := outputBudget(metadata, budget); err != nil {
			return model.Value{}, err
		}
		extraction := jsonExtraction{pathMode: true, pathLength: len(array.ArrayVal), path: make([]jsonPathStep, count)}
		for i := range extraction.path {
			value := array.ArrayVal[i]
			if value.Type != model.TypeString {
				return model.Value{}, sqlError("42804", "JSON path requires text elements")
			}
			index, err := jsonPathIndex(ctx, value.StrVal)
			if err != nil {
				return model.Value{}, err
			}
			extraction.path[i] = jsonPathStep{field: value.StrVal, index: index}
		}
		span, _, err := scanJSONExtraction(ctx, args[0].StrVal, &extraction)
		if err != nil {
			return model.Value{}, err
		}
		return jsonExtractResult(ctx, args[0].StrVal, span, asText, resultType, budget-metadata)
	}
}

func jsonPathIndex(ctx context.Context, text string) (int64, error) {
	index, valid, err := parseJSONPathIndex(ctx, text)
	if !valid {
		return math.MinInt32, err
	}
	return index, err
}

func parseJSONPathIndex(ctx context.Context, text string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	position := 0
	for position < len(text) && jsonPathSpace(text[position]) {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		position++
	}
	negative := false
	if position < len(text) && (text[position] == '-' || text[position] == '+') {
		negative = text[position] == '-'
		position++
	}
	start := position
	value := int64(0)
	for position < len(text) && text[position] >= '0' && text[position] <= '9' {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		value = value*10 + int64(text[position]-'0')
		if value > int64(math.MaxInt32)+1 {
			return 0, false, nil
		}
		position++
	}
	if position == start || position < len(text) && text[position] != 0 {
		return 0, false, nil
	}
	if negative {
		value = -value
	}
	if value > math.MaxInt32 {
		return 0, false, nil
	}
	return value, true, nil
}

func jsonPathSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

func (extraction *jsonExtraction) field(depth int, state uint8) *string {
	if depth == 0 || state != jsonObjectFirst && state != jsonObjectKey {
		return nil
	}
	if extraction.pathMode {
		if depth <= len(extraction.path) {
			return &extraction.path[depth-1].field
		}
		return nil
	}
	if depth == 1 && extraction.selector != nil && extraction.selector.shape == jsonTextObject {
		return &extraction.selector.field
	}
	return nil
}

func (extraction *jsonExtraction) key(depth int, matches bool) {
	if extraction.pathMode {
		if depth <= len(extraction.path) {
			extraction.path[depth-1].fieldMatches = matches
		}
	} else if depth == 1 {
		extraction.fieldMatches = matches
	}
}

func (extraction *jsonExtraction) value(depth int, object bool, elements int64) bool {
	if extraction.pathMode {
		if depth == 0 {
			return extraction.pathLength == 0
		}
		if depth > len(extraction.path) {
			return false
		}
		step := &extraction.path[depth-1]
		match := step.fieldMatches
		if !object {
			match = step.index == step.current
			step.current++
		}
		return step.matched && match
	}
	if depth != 1 || extraction.selector == nil {
		return false
	}
	if object {
		return extraction.fieldMatches
	}
	return extraction.selector.shape == jsonTextArray && extraction.selector.index == elements
}

func (extraction *jsonExtraction) enter(ctx context.Context, text string, depth int, token jsonExtractToken, matched bool) error {
	if !extraction.pathMode || depth >= len(extraction.path) {
		return nil
	}
	step := &extraction.path[depth]
	step.matched = depth == 0 || matched
	step.current = 0
	if token.kind != jsonTextArray || step.index >= 0 || step.index == math.MinInt32 {
		return nil
	}
	// PostgreSQL mutates each depth's negative index after its first fitting array,
	// even when that array is outside the matching object path.
	_, count, err := scanJSONTextExtract(ctx, text[token.start:], nil)
	if err != nil {
		return err
	}
	if -step.index <= count {
		step.index += count
	}
	return nil
}

func jsonExtractResult(ctx context.Context, text string, span jsonTextSpan, asText bool, resultType model.DataType, budget int64) (model.Value, error) {
	if span.kind == 0 || asText && span.kind == jsonTextNull {
		return model.NullVal(resultType), nil
	}
	selected := text[span.start:span.end]
	if asText && span.kind == jsonTextString {
		decoded, err := decodeJSONString(ctx, selected, budget)
		if err != nil {
			return model.Value{}, err
		}
		return model.StringVal(decoded), nil
	}
	result, err := ownedText(ctx, budget, selected)
	if err != nil {
		return model.Value{}, err
	}
	result.Type = resultType
	return result, nil
}
