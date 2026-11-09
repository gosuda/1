package builtin

import (
	"context"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type fullTextRowsPlan struct {
	id       model.FunctionID
	metadata uint64
}

func ResolveFullTextRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, _ ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	switch id {
	case 3322, 3689, 3690, 3713, 3714, 3715, 3716, 6183, 6184:
	default:
		return nil, &MissingImplementationError{Signature: signature}
	}
	if len(arguments) != int(signature.ArgumentCount) || result.Type.Storage != model.TypeComposite {
		return nil, sqlError("42804", "text search row source does not match its declared record type")
	}
	if err := validateFullTextRowArguments(id, arguments); err != nil {
		return nil, err
	}
	if err := validateFullTextRowResult(id, result); err != nil {
		return nil, err
	}
	metadata := uint64(unsafe.Sizeof(fullTextRowsPlan{}))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	return &fullTextRowsPlan{id: id, metadata: metadata}, nil
}

func validateFullTextRowArguments(id model.FunctionID, arguments []model.TypeLayout) error {
	want := func(index int, typeID model.TypeID, storage model.DataType) bool {
		return index < len(arguments) && arguments[index].Type.ID == typeID && arguments[index].Type.Storage == storage && len(arguments[index].Children) == 0
	}
	valid := false
	switch id {
	case 3322:
		valid = want(0, 3614, model.TypeBytes)
	case 3689:
		valid = want(0, 25, model.TypeString)
	case 3690:
		valid = want(0, 25, model.TypeString) && want(1, 25, model.TypeString)
	case 3713:
		valid = want(0, 26, model.TypeInt64)
	case 3714:
		valid = want(0, 25, model.TypeString)
	case 3715:
		valid = want(0, 26, model.TypeInt64) && want(1, 25, model.TypeString)
	case 3716:
		valid = want(0, 25, model.TypeString) && want(1, 25, model.TypeString)
	case 6183:
		valid = want(0, 3734, model.TypeInt64) && want(1, 25, model.TypeString)
	case 6184:
		valid = want(0, 25, model.TypeString)
	}
	if !valid {
		return sqlError("42804", "text search row source requires its declared argument types")
	}
	return nil
}

func validateFullTextRowResult(id model.FunctionID, result model.TypeLayout) error {
	column := func(index int, typeID model.TypeID, storage model.DataType) bool {
		if index >= len(result.Children) {
			return false
		}
		layout := result.Children[index]
		return layout.Type.ID == typeID && layout.Type.Storage == storage
	}
	valid := false
	switch id {
	case 3322:
		valid = len(result.Children) == 3 && column(0, 25, model.TypeString) && column(1, 1005, model.TypeArray) && column(2, 1009, model.TypeArray)
	case 3689, 3690:
		valid = len(result.Children) == 3 && column(0, 25, model.TypeString) && column(1, 23, model.TypeInt64) && column(2, 23, model.TypeInt64)
	case 3713, 3714:
		valid = len(result.Children) == 3 && column(0, 23, model.TypeInt64) && column(1, 25, model.TypeString) && column(2, 25, model.TypeString)
	case 3715, 3716:
		valid = len(result.Children) == 2 && column(0, 23, model.TypeInt64) && column(1, 25, model.TypeString)
	case 6183, 6184:
		valid = len(result.Children) == 6 && column(0, 25, model.TypeString) && column(1, 25, model.TypeString) && column(2, 25, model.TypeString) && column(3, 3770, model.TypeArray) && column(4, 3769, model.TypeInt64) && column(5, 1009, model.TypeArray)
	}
	if !valid {
		return sqlError("42804", "text search row source result record has the wrong layout")
	}
	return nil
}

func (plan *fullTextRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type fullTextRowsIterator struct {
	rows     [][]model.Value
	position int
	claim    columnar.Allocation
	closed   bool
}

func (plan *fullTextRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (RowSourceIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, _ := LookupSignature(plan.id)
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "text search row source argument count does not match signature")
	}
	expected := []model.DataType{model.TypeString}
	switch plan.id {
	case 3322:
		expected = []model.DataType{model.TypeBytes}
	case 3690:
		expected = []model.DataType{model.TypeString, model.TypeString}
	case 3713:
		expected = []model.DataType{model.TypeInt64}
	case 3715:
		expected = []model.DataType{model.TypeInt64, model.TypeString}
	case 3716:
		expected = []model.DataType{model.TypeString, model.TypeString}
	case 6183:
		expected = []model.DataType{model.TypeInt64, model.TypeString}
	}
	for i, argument := range arguments {
		if argument.Type != expected[i] {
			return nil, sqlError("42804", "text search row source requires its declared argument types")
		}
	}
	var inputBytes uint64
	for _, argument := range arguments {
		bytes, err := model.ValueBytes(ctx, argument, options.MaxInputBytes)
		if err != nil || bytes > options.MaxInputBytes-inputBytes {
			return nil, sqlError("54000", "text search row source input exceeded byte budget")
		}
		inputBytes += bytes
		if argument.IsNull {
			return &fullTextRowsIterator{}, nil
		}
	}
	rows, err := prepareFullTextRows(ctx, plan.id, arguments, options)
	if err != nil {
		return nil, err
	}
	iterator := &fullTextRowsIterator{rows: rows}
	retained := uint64(unsafe.Sizeof(*iterator)) + uint64(len(rows))*uint64(unsafe.Sizeof([]model.Value{}))
	if retained > options.MaxMemoryBytes {
		return nil, sqlError("54000", "text search row source exceeded memory budget")
	}
	for _, row := range rows {
		rowBytes, valueErr := model.ValueBytes(ctx, model.Value{Type: model.TypeArray, ArrayVal: row}, options.MaxRowBytes)
		if valueErr != nil || rowBytes > options.MaxRowBytes {
			return nil, sqlError("54000", "text search row exceeded byte budget")
		}
		if rowBytes > options.MaxMemoryBytes-retained {
			return nil, sqlError("54000", "text search row source exceeded memory budget")
		}
		retained += rowBytes
	}
	if options.Allocator != nil {
		iterator.claim, err = options.Allocator.Reserve(retained)
		if err != nil {
			return nil, err
		}
	}
	return iterator, nil
}

func prepareFullTextRows(ctx context.Context, id model.FunctionID, arguments []model.Value, options RowSourceOptions) ([][]model.Value, error) {
	switch id {
	case 3322:
		return prepareTSVectorRows(ctx, arguments[0].BytesVal)
	case 3689, 3690:
		return prepareTSStatRows(ctx, arguments, options)
	case 3713, 3714:
		if err := validateTextSearchParser(ctx, arguments[0]); err != nil {
			return nil, err
		}
		rows := make([][]model.Value, tsTokenTypeCount)
		for token := uint16(1); token <= tsTokenTypeCount; token++ {
			alias, description := tsTokenInfo(token)
			rows[token-1] = []model.Value{model.Int64Val(int64(token)), model.StringVal(alias), model.StringVal(description)}
		}
		return rows, nil
	case 3715, 3716:
		if err := validateTextSearchParser(ctx, arguments[0]); err != nil {
			return nil, err
		}
		return prepareTSParseRows(ctx, arguments[1].StrVal, options)
	case 6183, 6184:
		return prepareTSDebugRows(ctx, id, arguments, options)
	default:
		return nil, &UnknownFunctionError{ID: id}
	}
}

func prepareTSVectorRows(ctx context.Context, data []byte) ([][]model.Value, error) {
	vector, err := decodeTSVector(ctx, data)
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, len(vector))
	const letters = "DABC"
	for i, lexeme := range vector {
		positions := make([]model.Value, len(lexeme.positions))
		weights := make([]model.Value, len(lexeme.positions))
		for j, position := range lexeme.positions {
			positions[j] = model.Int64Val(int64(tsPositionNumber(position)))
			weights[j] = model.StringVal(letters[tsPositionWeight(position) : tsPositionWeight(position)+1])
		}
		rows[i] = []model.Value{model.StringVal(lexeme.text), fullTextArray(positions), fullTextArray(weights)}
	}
	return rows, nil
}

func prepareTSStatRows(ctx context.Context, arguments []model.Value, options RowSourceOptions) ([][]model.Value, error) {
	if options.TextSearch == nil {
		return nil, sqlError("55000", "text search statistics runtime is unavailable")
	}
	weights := ""
	if len(arguments) == 2 {
		weights = strings.ToUpper(arguments[1].StrVal)
		for _, weight := range weights {
			if weight < 'A' || weight > 'D' {
				return nil, sqlError("22023", "unrecognized weight")
			}
		}
	}
	statistics, err := options.TextSearch.TextSearchStatistics(ctx, arguments[0].StrVal, weights, options.MaxMemoryBytes)
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, len(statistics))
	for i, statistic := range statistics {
		if statistic.Documents < 0 || statistic.Documents > math.MaxInt32 || statistic.Entries < 0 || statistic.Entries > math.MaxInt32 {
			return nil, sqlError("22003", "ts_stat count is out of range")
		}
		rows[i] = []model.Value{model.StringVal(statistic.Word), model.Int64Val(statistic.Documents), model.Int64Val(statistic.Entries)}
	}
	return rows, nil
}

func validateTextSearchParser(ctx context.Context, value model.Value) error {
	if value.Type == model.TypeInt64 {
		if model.OID(value.IntVal) == tsDefaultParserOID {
			return nil
		}
	} else if value.Type == model.TypeString {
		parts, err := ParseQualifiedName(ctx, value.StrVal, 1<<20)
		if err != nil {
			return err
		}
		if len(parts) == 1 && parts[0] == "default" || len(parts) == 2 && parts[0] == "pg_catalog" && parts[1] == "default" {
			return nil
		}
	}
	return sqlError("42704", "text search parser does not exist")
}

func prepareTSParseRows(ctx context.Context, text string, options RowSourceOptions) ([][]model.Value, error) {
	limit := max(1, min(len(text)+1, tsMaxDataBytes))
	tokens, err := parseDefaultText(ctx, text, limit)
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, len(tokens))
	for i, token := range tokens {
		rows[i] = []model.Value{model.Int64Val(int64(token.typ)), model.StringVal(token.text)}
	}
	return rows, nil
}

func prepareTSDebugRows(ctx context.Context, id model.FunctionID, arguments []model.Value, options RowSourceOptions) ([][]model.Value, error) {
	budget := CallBudget{OutputBytes: int64(min(options.MaxMemoryBytes, uint64(math.MaxInt64))), MemoryBytes: options.MaxMemoryBytes, Settings: options.Settings, TextSearch: options.TextSearch}
	var explicit *model.Value
	textIndex := 0
	if id == 6183 {
		explicit = &arguments[0]
		textIndex = 1
	}
	configuration, err := resolveTextSearchConfiguration(ctx, explicit, budget)
	if err != nil {
		return nil, err
	}
	tokens, err := parseDefaultText(ctx, arguments[textIndex].StrVal, max(1, min(len(arguments[textIndex].StrVal)+1, tsMaxDataBytes)))
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, len(tokens))
	for i, token := range tokens {
		dictionaries := textSearchMapping(configuration, token.typ)
		dictionaryValues := make([]model.Value, len(dictionaries))
		for j, dictionary := range dictionaries {
			dictionaryValues[j] = model.Int64Val(int64(dictionary.OID))
		}
		selected := model.NullVal(model.TypeInt64)
		lexemeValue := model.NullVal(model.TypeArray)
		for _, dictionary := range dictionaries {
			lexemes, lexizeErr := lexizeTextSearch(ctx, dictionary, token.text)
			if lexizeErr != nil {
				return nil, lexizeErr
			}
			if lexemes == nil {
				continue
			}
			selected = model.Int64Val(int64(dictionary.OID))
			values := make([]model.Value, len(lexemes))
			for j, lexeme := range lexemes {
				values[j] = model.StringVal(lexeme)
			}
			lexemeValue = fullTextArray(values)
			break
		}
		alias, description := tsTokenInfo(token.typ)
		rows[i] = []model.Value{model.StringVal(alias), model.StringVal(description), model.StringVal(token.text), fullTextArray(dictionaryValues), selected, lexemeValue}
	}
	return rows, nil
}

func fullTextArray(values []model.Value) model.Value {
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		value.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	return value
}

func (iterator *fullTextRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator.closed || iterator.position == len(iterator.rows) {
		_ = iterator.Close()
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		_ = iterator.Close()
		return nil, false, err
	}
	row := iterator.rows[iterator.position]
	iterator.position++
	return row, true, nil
}

func (iterator *fullTextRowsIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.closed = true
	iterator.rows = nil
	if iterator.claim != nil {
		iterator.claim.Release()
		iterator.claim = nil
	}
	return nil
}
