package builtin

import (
	"context"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileRegexpFunction(signature Signature, options ResolveOptions) scalarKernel {
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	indeterminate := collation != 950 && collation != 951
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if signature.ID != 1623 && signature.ID != 1986 && signature.ID != 1987 && indeterminate {
			return model.Value{}, sqlError("42P22", "regular expression requires a resolved C or POSIX collation")
		}
		switch signature.ID {
		case 79, 1238, 1239, 1240, 1241, 1252, 1254, 1256, 1656, 1657, 1658, 1659:
			return regexpOperator(ctx, signature.ID, arguments, budget)
		case 1623, 1986, 1987:
			return regexpSimilarEscape(ctx, signature.ID, arguments, budget)
		case 2073, 2074:
			return regexpSubstring(ctx, signature.ID, arguments, budget)
		case 2284, 2285, 6251, 6252, 6253:
			return regexpReplace(ctx, signature.ID, arguments, budget)
		case 2767, 2768:
			return regexpSplitArray(ctx, arguments, budget)
		case 3396, 3397:
			return regexpMatchArray(ctx, arguments, budget)
		case 6254, 6255, 6256:
			return regexpCount(ctx, arguments, budget)
		case 6257, 6258, 6259, 6260, 6261, 6262:
			return regexpInstr(ctx, arguments, budget)
		case 6263, 6264:
			return regexpLike(ctx, arguments, budget)
		case 6265, 6266, 6267, 6268, 6269:
			return regexpSubstr(ctx, arguments, budget)
		default:
			return model.Value{}, &MissingImplementationError{Signature: signature}
		}
	}
}

func regexpOperator(ctx context.Context, id model.FunctionID, arguments []model.Value, budget CallBudget) (model.Value, error) {
	flags := regexpFlags{syntax: regexpARE}
	switch id {
	case 1238, 1239, 1240, 1241, 1656, 1657:
		flags.ignoreCase = true
	}
	_, found, _, release, err := regexpFirst(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, 0, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	switch id {
	case 1239, 1241, 1252, 1256, 1657, 1659:
		found = !found
	}
	return model.BoolVal(found), nil
}

func regexpFirst(ctx context.Context, text, pattern string, flags regexpFlags, start int, budget CallBudget) (regexpMatch, bool, []int, func(), error) {
	if err := regexpMemoryFits(budget.MemoryBytes, text, pattern); err != nil {
		return regexpMatch{}, false, nil, nil, err
	}
	runtime, err := newRegexpRuntime(ctx, budget.MemoryBytes, budget.Allocator)
	if err != nil {
		return regexpMatch{}, false, nil, nil, err
	}
	fail := func(err error) (regexpMatch, bool, []int, func(), error) {
		runtime.close()
		return regexpMatch{}, false, nil, nil, err
	}
	if err := runtime.bindInput(text, pattern); err != nil {
		return fail(err)
	}
	program, err := compileRegexp(ctx, pattern, flags, budget.MemoryBytes)
	if err != nil {
		return fail(err)
	}
	runtime.bindGroups(program.groups)
	characters, offsets, err := decodeRegexpRunes(ctx, text)
	if err != nil {
		return fail(err)
	}
	match, found, err := program.find(runtime, characters, start)
	if err != nil {
		return fail(err)
	}
	return match, found, offsets, runtime.close, nil
}

func regexpAll(ctx context.Context, text, pattern string, flags regexpFlags, start int, budget CallBudget) ([]regexpMatch, []int, func(), error) {
	if err := regexpMemoryFits(budget.MemoryBytes, text, pattern); err != nil {
		return nil, nil, nil, err
	}
	runtime, err := newRegexpRuntime(ctx, budget.MemoryBytes, budget.Allocator)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) ([]regexpMatch, []int, func(), error) {
		runtime.close()
		return nil, nil, nil, err
	}
	if err := runtime.bindInput(text, pattern); err != nil {
		return fail(err)
	}
	program, err := compileRegexp(ctx, pattern, flags, budget.MemoryBytes)
	if err != nil {
		return fail(err)
	}
	runtime.bindGroups(program.groups)
	characters, offsets, err := decodeRegexpRunes(ctx, text)
	if err != nil {
		return fail(err)
	}
	matches := make([]regexpMatch, 0, 8)
	search := start
	for search <= len(characters) {
		match, found, err := program.find(runtime, characters, search)
		if err != nil {
			return fail(err)
		}
		if !found {
			break
		}
		if len(matches) >= runtime.maxStates {
			return fail(sqlError("54000", "regular expression exceeded memory budget"))
		}
		matches = append(matches, match)
		if !flags.global {
			break
		}
		whole := match.spans[0]
		if whole.end > whole.start {
			search = whole.end
		} else if whole.end < len(characters) {
			search = whole.end + 1
		} else {
			break
		}
	}
	return matches, offsets, runtime.close, nil
}

func regexpFlagsArgument(arguments []model.Value, index int) (regexpFlags, error) {
	if index >= len(arguments) {
		return parseRegexpFlags("")
	}
	return parseRegexpFlags(arguments[index].StrVal)
}

func regexpStart(arguments []model.Value, index int) (int, error) {
	if index >= len(arguments) {
		return 0, nil
	}
	if arguments[index].IntVal <= 0 {
		return 0, sqlError("22023", "invalid value for parameter \"start\": "+strconv.FormatInt(arguments[index].IntVal, 10))
	}
	if arguments[index].IntVal > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(arguments[index].IntVal - 1), nil
}

func regexpOccurrence(arguments []model.Value, index int) (int, error) {
	if index >= len(arguments) {
		return 1, nil
	}
	if arguments[index].IntVal <= 0 {
		return 0, sqlError("22023", "invalid value for parameter \"n\": "+strconv.FormatInt(arguments[index].IntVal, 10))
	}
	if arguments[index].IntVal > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(arguments[index].IntVal), nil
}

func regexpSimilarEscape(ctx context.Context, id model.FunctionID, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if arguments[0].IsNull {
		return model.NullVal(model.TypeString), nil
	}
	var escape *string
	if id == 1987 {
		value := "\\"
		escape = &value
	} else if len(arguments) > 1 && !arguments[1].IsNull {
		escape = &arguments[1].StrVal
	}
	translated, err := translateSimilarPattern(ctx, arguments[0].StrVal, escape, budget.OutputBytes, budget.MemoryBytes)
	if err != nil {
		return model.Value{}, err
	}
	return model.StringVal(translated), nil
}

func translateSimilarPattern(ctx context.Context, pattern string, escape *string, outputLimit int64, memoryLimit uint64) (string, error) {
	if outputLimit < 0 {
		return "", sqlError("22023", "negative builtin output byte budget")
	}
	values := []string{pattern}
	if escape != nil {
		values = append(values, *escape)
	}
	if err := regexpMemoryFits(memoryLimit, values...); err != nil {
		return "", err
	}
	characters, _, err := decodeRegexpRunes(ctx, pattern)
	if err != nil {
		return "", err
	}
	escapeCharacter := rune('\\')
	escapingEnabled := true
	if escape != nil {
		escapeRunes, _, err := decodeRegexpRunes(ctx, *escape)
		if err != nil {
			return "", err
		}
		switch len(escapeRunes) {
		case 0:
			escapingEnabled = false
		case 1:
			escapeCharacter = escapeRunes[0]
		default:
			return "", sqlError("22025", "invalid escape string")
		}
	}
	var result strings.Builder
	result.WriteString("^(?:")
	afterEscape, bracketDepth, classPosition, separators := false, 0, 0, 0
	for position, character := range characters {
		if position&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		switch {
		case afterEscape:
			if character == '"' && bracketDepth == 0 {
				switch separators {
				case 0:
					result.WriteString("){1,1}?(")
				case 1:
					result.WriteString("){1,1}(?:")
				default:
					return "", sqlError("2200C", "SQL regular expression may not contain more than two escape-double-quote separators")
				}
				separators++
			} else {
				result.WriteRune('\\')
				result.WriteRune(character)
				classPosition = 3
			}
			afterEscape = false
		case escapingEnabled && character == escapeCharacter:
			afterEscape = true
		case bracketDepth > 0:
			if character == '\\' {
				result.WriteRune('\\')
			}
			result.WriteRune(character)
			switch {
			case character == ']' && classPosition > 2:
				bracketDepth--
			case character == '[':
				bracketDepth++
				classPosition = 3
			case character == '^':
				classPosition++
			default:
				classPosition = 3
			}
		case character == '[':
			result.WriteRune(character)
			bracketDepth = 1
			classPosition = 1
		case character == '%':
			result.WriteString(".*")
		case character == '_':
			result.WriteRune('.')
		case character == '(':
			result.WriteString("(?:")
		case character == '\\' || character == '.' || character == '^' || character == '$':
			result.WriteRune('\\')
			result.WriteRune(character)
		default:
			result.WriteRune(character)
		}
		if int64(result.Len()) > outputLimit {
			return "", sqlError("54000", "builtin output byte budget exceeded")
		}
	}
	result.WriteString(")$")
	if err := outputBudget(int64(result.Len()), outputLimit); err != nil {
		return "", err
	}
	return result.String(), nil
}

func regexpSubstring(ctx context.Context, id model.FunctionID, arguments []model.Value, budget CallBudget) (model.Value, error) {
	pattern := arguments[1].StrVal
	if id == 2074 {
		translated, err := translateSimilarPattern(ctx, pattern, &arguments[2].StrVal, budget.OutputBytes, budget.MemoryBytes)
		if err != nil {
			return model.Value{}, err
		}
		pattern = translated
	}
	match, found, offsets, release, err := regexpFirst(ctx, arguments[0].StrVal, pattern, regexpFlags{syntax: regexpARE}, 0, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	if !found {
		return model.NullVal(model.TypeString), nil
	}
	span := match.spans[0]
	if len(match.spans) > 1 {
		span = match.spans[1]
		if !span.valid {
			return model.NullVal(model.TypeString), nil
		}
	}
	return regexpOwnedSliceOffsets(ctx, arguments[0].StrVal, offsets, span, budget.OutputBytes)
}

func regexpCount(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	start, err := regexpStart(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	flags, err := regexpFlagsArgument(arguments, 3)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_count", flags); err != nil {
		return model.Value{}, err
	}
	flags.global = true
	matches, _, release, err := regexpAll(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, start, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	return model.Int64Val(int64(len(matches))), nil
}

func regexpLike(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	flags, err := regexpFlagsArgument(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_like", flags); err != nil {
		return model.Value{}, err
	}
	_, found, _, release, err := regexpFirst(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, 0, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	return model.BoolVal(found), nil
}

func regexpMatchArray(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	flags, err := regexpFlagsArgument(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_match", flags); err != nil {
		return model.Value{}, err
	}
	match, found, offsets, release, err := regexpFirst(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, 0, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	if !found {
		return model.NullVal(model.TypeArray), nil
	}
	return regexpMatchValue(ctx, arguments[0].StrVal, offsets, match, budget.OutputBytes)
}

func regexpMatchValue(ctx context.Context, text string, offsets []int, match regexpMatch, outputLimit int64) (model.Value, error) {
	spans := match.spans
	if len(spans) > 1 {
		spans = spans[1:]
	}
	size := int64(len(spans))*arrayValueBytes + arrayDimensionBytes
	for _, span := range spans {
		if span.valid {
			size += int64(offsets[span.end] - offsets[span.start])
		}
	}
	if err := outputBudget(size, outputLimit); err != nil {
		return model.Value{}, err
	}
	values := make([]model.Value, len(spans))
	for index, span := range spans {
		if !span.valid {
			values[index] = model.NullVal(model.TypeString)
			continue
		}
		value := text[offsets[span.start]:offsets[span.end]]
		values[index] = model.StringVal(strings.Clone(value))
	}
	result := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		result.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	if err := regexpOutputValue(ctx, result, outputLimit); err != nil {
		return model.Value{}, err
	}
	return result, nil
}

func regexpSplitArray(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	flags, err := regexpFlagsArgument(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_split_to_array", flags); err != nil {
		return model.Value{}, err
	}
	flags.global = true
	pieces, release, err := regexpSplitPieces(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	size := int64(len(pieces))*arrayValueBytes + arrayDimensionBytes
	for _, piece := range pieces {
		size += int64(len(piece))
	}
	if err := outputBudget(size, budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	values := make([]model.Value, len(pieces))
	for index, piece := range pieces {
		values[index] = model.StringVal(strings.Clone(piece))
	}
	result := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		result.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	if err := regexpOutputValue(ctx, result, budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	return result, nil
}

func regexpSplitPieces(ctx context.Context, text, pattern string, flags regexpFlags, budget CallBudget) ([]string, func(), error) {
	matches, offsets, release, err := regexpAll(ctx, text, pattern, flags, 0, budget)
	if err != nil {
		return nil, nil, err
	}
	characters := len(offsets) - 1
	pieces := make([]string, 0, len(matches)+1)
	previous := 0
	for _, match := range matches {
		span := match.spans[0]
		if span.start >= characters || span.end <= previous {
			continue
		}
		pieces = append(pieces, text[offsets[previous]:offsets[span.start]])
		previous = span.end
	}
	pieces = append(pieces, text[offsets[previous]:])
	return pieces, release, nil
}

func regexpInstr(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	start, err := regexpStart(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	occurrence, err := regexpOccurrence(arguments, 3)
	if err != nil {
		return model.Value{}, err
	}
	endOption := int64(0)
	if len(arguments) > 4 {
		endOption = arguments[4].IntVal
		if endOption != 0 && endOption != 1 {
			return model.Value{}, sqlError("22023", "invalid value for parameter \"endoption\": "+strconv.FormatInt(endOption, 10))
		}
	}
	flags, err := regexpFlagsArgument(arguments, 5)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_instr", flags); err != nil {
		return model.Value{}, err
	}
	flags.global = true
	subexpression := int64(0)
	if len(arguments) > 6 {
		subexpression = arguments[6].IntVal
		if subexpression < 0 {
			return model.Value{}, sqlError("22023", "invalid value for parameter \"subexpr\": "+strconv.FormatInt(subexpression, 10))
		}
	}
	matches, _, release, err := regexpAll(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, start, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	if occurrence > len(matches) || subexpression > math.MaxInt || int(subexpression) >= len(matches[occurrence-1].spans) {
		return model.Int64Val(0), nil
	}
	span := matches[occurrence-1].spans[int(subexpression)]
	if !span.valid {
		return model.Int64Val(0), nil
	}
	position := span.start + 1
	if endOption == 1 {
		position = span.end + 1
	}
	return model.Int64Val(int64(position)), nil
}

func regexpSubstr(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	start, err := regexpStart(arguments, 2)
	if err != nil {
		return model.Value{}, err
	}
	occurrence, err := regexpOccurrence(arguments, 3)
	if err != nil {
		return model.Value{}, err
	}
	flags, err := regexpFlagsArgument(arguments, 4)
	if err != nil {
		return model.Value{}, err
	}
	if err := rejectRegexpGlobal("regexp_substr", flags); err != nil {
		return model.Value{}, err
	}
	flags.global = true
	subexpression := int64(0)
	if len(arguments) > 5 {
		subexpression = arguments[5].IntVal
		if subexpression < 0 {
			return model.Value{}, sqlError("22023", "invalid value for parameter \"subexpr\": "+strconv.FormatInt(subexpression, 10))
		}
	}
	matches, offsets, release, err := regexpAll(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, start, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	if occurrence > len(matches) || subexpression > math.MaxInt || int(subexpression) >= len(matches[occurrence-1].spans) {
		return model.NullVal(model.TypeString), nil
	}
	span := matches[occurrence-1].spans[int(subexpression)]
	if !span.valid {
		return model.NullVal(model.TypeString), nil
	}
	return regexpOwnedSliceOffsets(ctx, arguments[0].StrVal, offsets, span, budget.OutputBytes)
}

func regexpReplace(ctx context.Context, id model.FunctionID, arguments []model.Value, budget CallBudget) (model.Value, error) {
	start := 0
	occurrence := 1
	flags := regexpFlags{syntax: regexpARE}
	var err error
	switch id {
	case 2285:
		flags, err = parseRegexpFlags(arguments[3].StrVal)
		if err == nil && flags.global {
			occurrence = 0
		}
	case 6251, 6252, 6253:
		start, err = regexpStart(arguments, 3)
		if err == nil && len(arguments) > 4 {
			if arguments[4].IntVal < 0 {
				err = sqlError("22023", "invalid value for parameter \"n\": "+strconv.FormatInt(arguments[4].IntVal, 10))
			} else if arguments[4].IntVal > math.MaxInt {
				occurrence = math.MaxInt
			} else {
				occurrence = int(arguments[4].IntVal)
			}
		}
		if err == nil && len(arguments) > 5 {
			flags, err = parseRegexpFlags(arguments[5].StrVal)
		}
	}
	if err != nil {
		return model.Value{}, err
	}
	flags.global = occurrence != 1
	matches, offsets, release, err := regexpAll(ctx, arguments[0].StrVal, arguments[1].StrVal, flags, start, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer release()
	var result strings.Builder
	cursor := 0
	for index, match := range matches {
		replace := occurrence == 0 || index+1 == occurrence
		if !replace {
			continue
		}
		whole := match.spans[0]
		if err := appendRegexpText(ctx, &result, arguments[0].StrVal[offsets[cursor]:offsets[whole.start]], budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		if err := appendRegexpReplacement(ctx, &result, arguments[2].StrVal, arguments[0].StrVal, offsets, match, budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		cursor = whole.end
		if occurrence != 0 {
			break
		}
	}
	if err := appendRegexpText(ctx, &result, arguments[0].StrVal[offsets[cursor]:], budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if err := outputBudget(int64(result.Len()), budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(result.String()), nil
}

func appendRegexpReplacement(ctx context.Context, output *strings.Builder, replacement, text string, offsets []int, match regexpMatch, outputLimit int64) error {
	if _, err := textCharacters(ctx, replacement); err != nil {
		return err
	}
	for position := 0; position < len(replacement); {
		if position&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if replacement[position] != '\\' || position+1 == len(replacement) {
			_, width := utf8.DecodeRuneInString(replacement[position:])
			if err := appendRegexpText(ctx, output, replacement[position:position+width], outputLimit); err != nil {
				return err
			}
			position += width
			continue
		}
		start := position
		next, width := utf8.DecodeRuneInString(replacement[position+1:])
		position += 1 + width
		if next == '\\' {
			if err := appendRegexpText(ctx, output, `\`, outputLimit); err != nil {
				return err
			}
			continue
		}
		group := -1
		if next == '&' {
			group = 0
		} else if next >= '1' && next <= '9' {
			group = int(next - '0')
		}
		if group < 0 {
			if err := appendRegexpText(ctx, output, replacement[start:position], outputLimit); err != nil {
				return err
			}
			continue
		}
		if group < len(match.spans) && match.spans[group].valid {
			span := match.spans[group]
			if err := appendRegexpText(ctx, output, text[offsets[span.start]:offsets[span.end]], outputLimit); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendRegexpText(ctx context.Context, output *strings.Builder, value string, outputLimit int64) error {
	if outputLimit < int64(output.Len()) || int64(len(value)) > outputLimit-int64(output.Len()) {
		return sqlError("54000", "builtin output byte budget exceeded")
	}
	return writeText(ctx, output, value)
}


func regexpOwnedSliceOffsets(ctx context.Context, text string, offsets []int, span regexpSpan, outputLimit int64) (model.Value, error) {
	piece := text[offsets[span.start]:offsets[span.end]]
	return ownedText(ctx, outputLimit, piece)
}

func regexpOutputValue(ctx context.Context, value model.Value, outputLimit int64) error {
	if outputLimit < 0 {
		return sqlError("22023", "negative builtin output byte budget")
	}
	bytes, err := model.ValueBytes(ctx, value, uint64(outputLimit))
	if err != nil || bytes > uint64(outputLimit) {
		return sqlError("54000", "builtin output byte budget exceeded")
	}
	return nil
}
