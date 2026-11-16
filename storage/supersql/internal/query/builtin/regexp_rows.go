package builtin

import (
	"context"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type regexpRowsPlan struct {
	id       model.FunctionID
	metadata uint64
}

func ResolveRegexpRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	if id != 2763 && id != 2764 && id != 2765 && id != 2766 {
		return nil, &MissingImplementationError{Signature: signature}
	}
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	if collation != 950 && collation != 951 {
		return nil, sqlError("42P22", "regular expression requires a resolved C or POSIX collation")
	}
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "regular expression argument count does not match signature")
	}
	for _, argument := range arguments {
		if argument.Type.ID != 25 || argument.Type.Storage != model.TypeString || len(argument.Children) != 0 {
			return nil, sqlError("42804", "regular expression requires text arguments")
		}
	}
	if id == 2763 || id == 2764 {
		valid := result.Type.ID == 1009 && result.Type.Storage == model.TypeArray && len(result.Children) == 1 && result.Children[0].Type.ID == 25 && result.Children[0].Type.Storage == model.TypeString && len(result.Children[0].Children) == 0
		if !valid {
			return nil, sqlError("42804", "regexp_matches result must be text[]")
		}
	} else if result.Type.ID != 25 || result.Type.Storage != model.TypeString || len(result.Children) != 0 {
		return nil, sqlError("42804", "regexp_split_to_table result must be text")
	}
	metadata := uint64(unsafe.Sizeof(regexpRowsPlan{}))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	return &regexpRowsPlan{id: id, metadata: metadata}, nil
}

func (plan *regexpRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type regexpRowsIterator struct {
	id       model.FunctionID
	options  RowSourceOptions
	runtime  *regexpRuntime
	text     string
	offsets  []int
	matches  []regexpMatch
	pieces   []regexpSpan
	position int
	row      [1]model.Value
	closed   bool
}

func (plan *regexpRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (_ RowSourceIterator, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, _ := LookupSignature(plan.id)
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "regular expression argument count does not match signature")
	}
	var inputBytes uint64
	for _, argument := range arguments {
		if argument.Type != model.TypeString {
			return nil, sqlError("42804", "regular expression requires text arguments")
		}
		bytes, valueErr := model.ValueBytes(ctx, argument, options.MaxInputBytes)
		if valueErr != nil || bytes > options.MaxInputBytes-inputBytes {
			return nil, sqlError("54000", "regular expression input exceeded byte budget")
		}
		inputBytes += bytes
	}
	if inputBytes > options.MaxInputBytes || inputBytes > options.MaxMemoryBytes {
		return nil, sqlError("54000", "regular expression input exceeded memory budget")
	}
	iterator := &regexpRowsIterator{id: plan.id, options: options}
	fail := func(failure error) (RowSourceIterator, error) {
		_ = iterator.Close()
		return nil, failure
	}
	for _, argument := range arguments {
		if argument.IsNull {
			return iterator, nil
		}
	}
	if err := regexpMemoryFits(options.MaxMemoryBytes, arguments[0].StrVal, arguments[1].StrVal); err != nil {
		return fail(err)
	}
	iterator.runtime, err = newRegexpRuntime(ctx, options.MaxMemoryBytes, options.Allocator)
	if err != nil {
		return fail(err)
	}
	if err := iterator.runtime.bindInput(arguments[0].StrVal, arguments[1].StrVal); err != nil {
		return fail(err)
	}
	flags, err := regexpFlagsArgument(arguments, 2)
	if err != nil {
		return fail(err)
	}
	if plan.id == 2765 || plan.id == 2766 {
		if err := rejectRegexpGlobal("regexp_split_to_table", flags); err != nil {
			return fail(err)
		}
	}
	program, err := compileRegexp(ctx, arguments[1].StrVal, flags, options.MaxMemoryBytes)
	if err != nil {
		return fail(err)
	}
	iterator.runtime.bindGroups(program.groups)
	iterator.text = arguments[0].StrVal
	characters, offsets, err := decodeRegexpRunes(ctx, iterator.text)
	if err != nil {
		return fail(err)
	}
	iterator.offsets = offsets
	matches := make([]regexpMatch, 0, 8)
	search := 0
	for search <= len(characters) {
		match, found, findErr := program.find(iterator.runtime, characters, search)
		if findErr != nil {
			return fail(findErr)
		}
		if !found {
			break
		}
		if len(matches) >= iterator.runtime.maxStates {
			return fail(sqlError("54000", "regular expression exceeded memory budget"))
		}
		matches = append(matches, match)
		whole := match.spans[0]
		if !flags.global && (plan.id == 2763 || plan.id == 2764) {
			break
		}
		if whole.end > whole.start {
			search = whole.end
		} else if whole.end < len(characters) {
			search = whole.end + 1
		} else {
			break
		}
	}
	if plan.id == 2763 || plan.id == 2764 {
		iterator.matches = matches
		return iterator, nil
	}
	iterator.pieces = make([]regexpSpan, 0, len(matches)+1)
	previous := 0
	for _, match := range matches {
		span := match.spans[0]
		if span.start >= len(characters) || span.end <= previous {
			continue
		}
		iterator.pieces = append(iterator.pieces, regexpSpan{start: previous, end: span.start, valid: true})
		previous = span.end
	}
	iterator.pieces = append(iterator.pieces, regexpSpan{start: previous, end: len(characters), valid: true})
	return iterator, nil
}

func (iterator *regexpRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	iterator.row = [1]model.Value{}
	if iterator.closed {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		_ = iterator.Close()
		return nil, false, err
	}
	if iterator.id == 2763 || iterator.id == 2764 {
		if iterator.position == len(iterator.matches) {
			_ = iterator.Close()
			return nil, false, nil
		}
		limit := int64(min(iterator.options.MaxRowBytes, uint64(math.MaxInt64)))
		value, err := regexpMatchValue(ctx, iterator.text, iterator.offsets, iterator.matches[iterator.position], limit)
		if err != nil {
			_ = iterator.Close()
			return nil, false, err
		}
		iterator.row[0] = value
	} else {
		if iterator.position == len(iterator.pieces) {
			_ = iterator.Close()
			return nil, false, nil
		}
		span := iterator.pieces[iterator.position]
		piece := iterator.text[iterator.offsets[span.start]:iterator.offsets[span.end]]
		if uint64(len(piece)) > iterator.options.MaxRowBytes {
			_ = iterator.Close()
			return nil, false, sqlError("54000", "regular expression row exceeded byte budget")
		}
		iterator.row[0] = model.StringVal(strings.Clone(piece))
	}
	iterator.position++
	return iterator.row[:], true, nil
}

func (iterator *regexpRowsIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.closed = true
	iterator.row = [1]model.Value{}
	iterator.text = ""
	iterator.offsets = nil
	iterator.matches = nil
	iterator.pieces = nil
	iterator.options = RowSourceOptions{}
	if iterator.runtime != nil {
		iterator.runtime.close()
		iterator.runtime = nil
	}
	return nil
}
