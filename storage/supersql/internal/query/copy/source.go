package copy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

var binarySignature = [...]byte{'P', 'G', 'C', 'O', 'P', 'Y', '\n', 0xff, '\r', '\n', 0}

const codecOverheadBytes = 64

type inputCodec struct {
	text   builtin.TypeTextCodec
	binary builtin.TypeBinaryCodec
}

type sourcePlan struct {
	plan    *Plan
	streams *Streams
	codecs  []inputCodec
	owned   uint64
}

// NewSource resolves all type codecs before the stream can be read. The returned
// plan borrows streams for one portal lifetime and never closes its Reader.
func NewSource(ctx context.Context, plan *Plan, streams *Streams, options builtin.ResolveOptions, maximumMetadata uint64) (builtin.RowSourcePlan, error) {
	if err := ValidateStreams(plan, streams); err != nil {
		return nil, err
	}
	if plan.direction != From {
		return nil, sqlError("22023", "COPY TO cannot be opened as a row source")
	}
	streamSnapshot := *streams
	streams = &streamSnapshot
	codecs := make([]inputCodec, len(plan.columns))
	owned := plan.OwnedBytes() + uint64(len(codecs))*codecOverheadBytes
	if owned > maximumMetadata {
		return nil, sqlError("54000", "COPY codec metadata budget exceeded")
	}
	for i, column := range plan.columns {
		var used uint64
		var err error
		if plan.options.Format == Binary {
			codecs[i].binary, err = builtin.ResolveTypeBinaryCodec(ctx, column.Layout, options, maximumMetadata-owned)
			used = codecs[i].binary.OwnedBytes
		} else {
			codecs[i].text, err = builtin.ResolveTypeTextCodec(ctx, column.Layout, options, maximumMetadata-owned)
			used = codecs[i].text.OwnedBytes
		}
		if err != nil {
			return nil, err
		}
		if used > maximumMetadata-owned {
			return nil, sqlError("54000", "COPY codec metadata budget exceeded")
		}
		owned += used
	}
	return &sourcePlan{plan: plan, streams: streams, codecs: codecs, owned: owned}, ctx.Err()
}

func (plan *sourcePlan) OwnedBytes() uint64 { return plan.owned }

func (plan *sourcePlan) Open(ctx context.Context, arguments []model.Value, options builtin.RowSourceOptions) (builtin.RowSourceIterator, error) {
	if len(arguments) != 0 || options.MaxRowBytes == 0 || options.MaxMemoryBytes == 0 {
		return nil, sqlError("22023", "invalid COPY row source invocation")
	}
	rowBytes := uint64(len(plan.plan.columns)) * 2 * uint64(unsafe.Sizeof(model.Value{}))
	if rowBytes > options.MaxMemoryBytes {
		return nil, sqlError("54000", "COPY row metadata exceeds invocation memory budget")
	}
	iterator := &sourceIterator{
		plan: plan, input: bufio.NewReader(plan.streams.Reader), maximum: min(plan.streams.MaxRecordBytes, options.MaxRowBytes, options.MaxMemoryBytes),
		memory: options.MaxMemoryBytes, temporal: options.Temporal, settings: options.Settings, relations: options.Relations, textSearch: options.TextSearch,
		row: make([]model.Value, 2*len(plan.plan.columns)),
	}
	if iterator.maximum == 0 {
		return nil, sqlError("22023", "invalid COPY record limit")
	}
	return iterator, ctx.Err()
}


type sourceIterator struct {
	plan        *sourcePlan
	input       *bufio.Reader
	maximum     uint64
	memory      uint64
	temporal    model.TemporalRuntime
	settings    model.SettingsRuntime
	relations   model.RelationRuntime
	textSearch  model.TextSearchRuntime
	row         []model.Value
	record      []copyField
	rowNumber   uint64
	rejected    uint64
	initialized bool
	done        bool
	closed      bool
}

type copyField struct {
	text, source string
	quoted      bool
}

func (iterator *sourceIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator.closed {
		return nil, false, io.ErrClosedPipe
	}
	if iterator.done {
		return nil, false, nil
	}
	if !iterator.initialized {
		if err := iterator.initialize(ctx); err != nil {
			return nil, false, err
		}
	}
	for {
		clear(iterator.row)
		var ok bool
		var err error
		if iterator.plan.plan.options.Format == Binary {
			ok, err = iterator.binaryRow(ctx)
		} else {
			ok, err = iterator.textRow(ctx)
		}
		if err == nil && !ok {
			iterator.done = true
			return nil, false, nil
		}
		if err == nil {
			return iterator.row, true, nil
		}
		var conversion *conversionError
		if iterator.plan.plan.options.ErrorMode != IgnoreConversionErrors || !errors.As(err, &conversion) || !ignorableConversion(conversion.err) {
			return nil, false, err
		}
		iterator.rejected++
		if report := iterator.plan.streams.Report; report != nil && iterator.plan.plan.options.LogVerbosity != LogSilent {
			report(ctx, Diagnostic{Row: iterator.rowNumber, Column: conversion.column, Err: conversion.err})
		}
		limit := iterator.plan.plan.options.RejectLimit
		if limit != 0 && iterator.rejected > limit {
			return nil, false, sqlError("22000", "COPY reject limit exceeded")
		}
	}
}

func (iterator *sourceIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.closed = true
	clear(iterator.row)
	iterator.row = nil
	iterator.record = nil
	iterator.input = nil
	iterator.plan = nil
	return nil
}

func (iterator *sourceIterator) initialize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	iterator.initialized = true
	if iterator.plan.plan.options.Format == Binary {
		return iterator.binaryHeader(ctx)
	}
	if iterator.plan.plan.options.Header == NoHeader {
		return nil
	}
	fields, ok, end, err := iterator.readRecord(ctx)
	if err != nil {
		return err
	}
	iterator.rowNumber++
	if !ok || end {
		return sqlError("22P04", "COPY header is missing")
	}
	if iterator.plan.plan.options.Header == HeaderMatch {
		if len(fields) != len(iterator.plan.plan.columns) {
			return sqlError("22P04", "COPY header does not match table columns")
		}
		for i, field := range fields {
			if field.text != iterator.plan.plan.columns[i].Name {
				return sqlError("22P04", "COPY header does not match table columns")
			}
		}
	}
	return nil
}

func (iterator *sourceIterator) textRow(ctx context.Context) (bool, error) {
	fields, ok, end, err := iterator.readRecord(ctx)
	if err != nil || !ok || end {
		return false, err
	}
	iterator.rowNumber++
	if len(fields) != len(iterator.plan.plan.columns) {
		return false, sqlError("22P04", "COPY row has the wrong number of columns")
	}
	options := iterator.plan.plan.options
	for i, field := range fields {
		column := iterator.plan.plan.columns[i]
		isNull := field.source == options.Null && (!field.quoted || column.ForceNull) && !(column.ForceNotNull && !field.quoted)
		isDefault := options.HasDefault && field.source == options.Default && !field.quoted
		if isNull {
			iterator.row[i] = model.NullVal(column.Layout.Type.Storage)
		} else if isDefault {
			iterator.row[i] = model.NullVal(column.Layout.Type.Storage)
		} else {
			budget := iterator.callBudget()
			value, err := iterator.plan.codecs[i].text.Input(ctx, []model.Value{model.StringVal(field.text)}, budget)
			if err != nil {
				return false, &conversionError{column: column.Name, err: err}
			}
			iterator.row[i] = value
		}
		iterator.row[len(fields)+i] = model.BoolVal(isDefault)
	}
	return true, nil
}

func (iterator *sourceIterator) callBudget() builtin.CallBudget {
	limit := min(iterator.maximum, iterator.memory)
	return builtin.CallBudget{
		OutputBytes: int64(min(limit, math.MaxInt64)), MemoryBytes: iterator.memory,
		Temporal: iterator.temporal, Settings: iterator.settings, Relations: iterator.relations, TextSearch: iterator.textSearch,
	}
}

type conversionError struct {
	column string
	err    error
}

func (err *conversionError) Error() string { return err.err.Error() }
func (err *conversionError) Unwrap() error { return err.err }

func ignorableConversion(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var sqlErr *model.Error
	return errors.As(err, &sqlErr) && strings.HasPrefix(sqlErr.Code, "22")
}

func (iterator *sourceIterator) readRecord(ctx context.Context) ([]copyField, bool, bool, error) {
	if iterator.plan.plan.options.Format == CSV {
		return iterator.readCSVRecord(ctx)
	}
	line, ok, err := iterator.readLine(ctx)
	if err != nil || !ok {
		return nil, ok, false, err
	}
	if slices.Equal(line, []byte{'\\', '.'}) {
		return nil, true, true, nil
	}
	if len(iterator.plan.plan.columns) == 0 && len(line) == 0 {
		return nil, true, false, nil
	}
	fields := make([]copyField, 0, len(iterator.plan.plan.columns))
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' {
			if i+1 < len(line) {
				i++
			}
			continue
		}
		if line[i] != iterator.plan.plan.options.Delimiter {
			continue
		}
		raw := line[start:i]
		text, err := decodeTextField(ctx, raw, iterator.maximum)
		if err != nil {
			return nil, false, false, err
		}
		fields = append(fields, copyField{text: text, source: string(raw)})
		start = i + 1
	}
	raw := line[start:]
	text, err := decodeTextField(ctx, raw, iterator.maximum)
	if err != nil {
		return nil, false, false, err
	}
	fields = append(fields, copyField{text: text, source: string(raw)})
	return fields, true, false, nil
}

func (iterator *sourceIterator) readLine(ctx context.Context) ([]byte, bool, error) {
	line := make([]byte, 0, min(int(iterator.maximum), 256))
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		value, err := iterator.input.ReadByte()
		if err != nil {
			if err != io.EOF {
				return nil, false, err
			}
			// End of input without the \. terminator ends the stream; a
			// partial final line without a newline is still a record.
			return line, len(line) != 0, nil
		}
		if value == '\n' {
			if len(line) != 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return line, true, nil
		}
		if uint64(len(line)) == iterator.maximum {
			return nil, false, sqlError("54000", "COPY record byte limit exceeded")
		}
		line = append(line, value)
	}
}

func decodeTextField(ctx context.Context, raw []byte, maximum uint64) (string, error) {
	output := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil { return "", err }
		}
		if raw[i] != '\\' {
			output = append(output, raw[i])
			continue
		}
		i++
		if i == len(raw) {
			return "", sqlError("22P04", "COPY text row ends with a backslash")
		}
		switch raw[i] {
		case 'b': output = append(output, '\b')
		case 'f': output = append(output, '\f')
		case 'n': output = append(output, '\n')
		case 'r': output = append(output, '\r')
		case 't': output = append(output, '\t')
		case 'v': output = append(output, '\v')
		case 'x':
			value, digits := byte(0), 0
			for i+1 < len(raw) && digits < 2 {
				digit, ok := hexDigit(raw[i+1]); if !ok { break }
				i++; digits++; value = value*16 + digit
			}
			if digits == 0 { output = append(output, 'x') } else { output = append(output, value) }
		default:
			if raw[i] >= '0' && raw[i] <= '7' {
				value := raw[i]-'0'
				for digits := 1; i+1 < len(raw) && digits < 3 && raw[i+1] >= '0' && raw[i+1] <= '7'; digits++ {
					i++; value = value*8 + raw[i]-'0'
				}
				output = append(output, value)
			} else {
				output = append(output, raw[i])
			}
		}
		if uint64(len(output)) > maximum { return "", sqlError("54000", "COPY field byte limit exceeded") }
	}
	return string(output), nil
}

func hexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9': return value-'0', true
	case value >= 'a' && value <= 'f': return value-'a'+10, true
	case value >= 'A' && value <= 'F': return value-'A'+10, true
	default: return 0, false
	}
}

func (iterator *sourceIterator) readCSVRecord(ctx context.Context) ([]copyField, bool, bool, error) {
	options := iterator.plan.plan.options
	fields := make([]copyField, 0, len(iterator.plan.plan.columns))
	field := make([]byte, 0, 64)
	quoted, inQuote, afterQuote, any := false, false, false, false
	var recordBytes uint64
	for {
		if err := ctx.Err(); err != nil { return nil, false, false, err }
		value, err := iterator.input.ReadByte()
		if err != nil {
			if err != io.EOF { return nil, false, false, err }
			if !any && len(field) == 0 && len(fields) == 0 { return nil, false, false, nil }
			if inQuote { return nil, false, false, sqlError("22P04", "unterminated CSV quoted field") }
			fields = append(fields, copyField{text: string(field), source: string(field), quoted: quoted})
			return fields, true, false, nil
		}
		recordBytes++
		if recordBytes > iterator.maximum && (inQuote || value != '\n' && value != '\r') {
			return nil, false, false, sqlError("54000", "COPY record byte limit exceeded")
		}
		any = true
		if inQuote {
			if value == options.Quote && options.Escape == options.Quote {
				next, readErr := iterator.input.Peek(1)
				if readErr == nil && next[0] == options.Quote {
					_, _ = iterator.input.ReadByte()
					recordBytes++
					if recordBytes > iterator.maximum {
						return nil, false, false, sqlError("54000", "COPY record byte limit exceeded")
					}
					field = append(field, value)
					continue
				}
				inQuote, afterQuote = false, true
				continue
			}
			if value == options.Escape {
				next, readErr := iterator.input.ReadByte()
				if readErr != nil { return nil, false, false, sqlError("22P04", "unterminated CSV escape") }
				recordBytes++
				if recordBytes > iterator.maximum {
					return nil, false, false, sqlError("54000", "COPY record byte limit exceeded")
				}
				if next == options.Quote || next == options.Escape {
					field = append(field, next)
					continue
				}
				field = append(field, value, next)
				continue
			}
			if value == options.Quote {
				inQuote, afterQuote = false, true
				continue
			}
			field = append(field, value)
			if uint64(len(field)) > iterator.maximum { return nil, false, false, sqlError("54000", "COPY record byte limit exceeded") }
			continue
		}
		if afterQuote && value != options.Delimiter && value != '\n' && value != '\r' {
			return nil, false, false, sqlError("22P04", "unexpected data after closing CSV quote")
		}
		if value == options.Quote && len(field) == 0 && !afterQuote {
			quoted, inQuote = true, true
			continue
		}
		if value == options.Delimiter {
			fields = append(fields, copyField{text: string(field), source: string(field), quoted: quoted})
			field, quoted, afterQuote = field[:0], false, false
			continue
		}
		if value == '\r' {
			next, readErr := iterator.input.Peek(1)
			if readErr == nil && next[0] == '\n' { _, _ = iterator.input.ReadByte() }
			value = '\n'
		}
		if value == '\n' {
			if len(iterator.plan.plan.columns) == 0 && len(fields) == 0 && len(field) == 0 && !quoted {
				return nil, true, false, nil
			}
			fields = append(fields, copyField{text: string(field), source: string(field), quoted: quoted})
			if len(fields) == 1 && !fields[0].quoted && fields[0].text == `\.` { return nil, true, true, nil }
			return fields, true, false, nil
		}
		field = append(field, value)
		if uint64(len(field)) > iterator.maximum { return nil, false, false, sqlError("54000", "COPY record byte limit exceeded") }
	}
}

func (iterator *sourceIterator) binaryHeader(ctx context.Context) error {
	var header [19]byte
	if err := readFullContext(ctx, iterator.input, header[:]); err != nil { return err }
	if !slices.Equal(header[:11], binarySignature[:]) || binary.BigEndian.Uint32(header[11:]) != 0 {
		return sqlError("22P04", "invalid COPY binary header")
	}
	extension := uint64(binary.BigEndian.Uint32(header[15:]))
	if extension > iterator.maximum || extension > uint64(int(^uint(0)>>1)) { return sqlError("54000", "COPY binary header extension exceeds limit") }
	if extension != 0 {
		buffer := make([]byte, extension)
		if err := readFullContext(ctx, iterator.input, buffer); err != nil { return err }
	}
	return nil
}

func (iterator *sourceIterator) binaryRow(ctx context.Context) (bool, error) {
	var countBytes [2]byte
	if err := readFullContext(ctx, iterator.input, countBytes[:]); err != nil {
		if err == io.EOF { return false, sqlError("22P04", "COPY binary trailer is missing") }
		return false, err
	}
	count := int16(binary.BigEndian.Uint16(countBytes[:]))
	if count == -1 {
		if _, err := iterator.input.ReadByte(); err != nil {
			if err == io.EOF { return false, nil }
			return false, err
		}
		return false, sqlError("22P04", "trailing data after COPY binary trailer")
	}
	if count < 0 || int(count) != len(iterator.plan.plan.columns) {
		return false, sqlError("22P04", "COPY binary row has the wrong number of columns")
	}
	iterator.rowNumber++
	var rowBytes uint64 = 2
	if rowBytes > iterator.maximum {
		return false, sqlError("54000", "COPY binary record exceeds limit")
	}
	for i, column := range iterator.plan.plan.columns {
		var sizeBytes [4]byte
		if err := readFullContext(ctx, iterator.input, sizeBytes[:]); err != nil { return false, err }
		size := int32(binary.BigEndian.Uint32(sizeBytes[:]))
		rowBytes += 4
		if size == -1 {
			iterator.row[i] = model.NullVal(column.Layout.Type.Storage)
			iterator.row[len(iterator.plan.plan.columns)+i] = model.BoolVal(false)
			continue
		}
		if size < 0 || uint64(size) > iterator.maximum || rowBytes+uint64(size) > iterator.maximum {
			return false, sqlError("54000", "COPY binary record exceeds limit")
		}
		payload := make([]byte, int(size))
		if err := readFullContext(ctx, iterator.input, payload); err != nil { return false, err }
		rowBytes += uint64(size)
		value, err := iterator.plan.codecs[i].binary.Input(ctx, []model.Value{model.BytesVal(payload)}, iterator.callBudget())
		if err != nil { return false, &conversionError{column: column.Name, err: err} }
		iterator.row[i] = value
		iterator.row[len(iterator.plan.plan.columns)+i] = model.BoolVal(false)
	}
	return true, nil
}

func readFullContext(ctx context.Context, reader io.Reader, buffer []byte) error {
	read := false
	for len(buffer) != 0 {
		if err := ctx.Err(); err != nil { return err }
		count, err := reader.Read(buffer)
		if count != 0 {
			read = true
		}
		buffer = buffer[count:]
		if err != nil {
			if err == io.EOF && len(buffer) != 0 {
				if !read {
					return io.EOF
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if count == 0 { return io.ErrNoProgress }
	}
	return nil
}
