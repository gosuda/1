package copy

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type RowReader interface {
	Next(context.Context, *columnar.Batch) error
}

type Runtime struct {
	Temporal   model.TemporalRuntime
	Relations  model.RelationRuntime
	Settings   model.SettingsRuntime
	TextSearch model.TextSearchRuntime
	Advisory  model.AdvisoryLockRuntime
	Native     model.NativeScalarRuntime
}

type Writer struct {
	plan    *Plan
	streams *Streams
	codecs  []inputCodec
	maximum uint64
	runtime Runtime
}

// NewWriter resolves all type output codecs before emitting a header. Writer
// borrows the caller stream and never flushes or closes it.
func NewWriter(ctx context.Context, plan *Plan, streams *Streams, options builtin.ResolveOptions, runtime Runtime, maximumMetadata, maximumRecord uint64) (*Writer, error) {
	if err := ValidateStreams(plan, streams); err != nil {
		return nil, err
	}
	if plan.direction != To {
		return nil, sqlError("22023", "COPY FROM cannot be opened as a stream writer")
	}
	codecs := make([]inputCodec, len(plan.columns))
	streamSnapshot := *streams
	streams = &streamSnapshot
	used := plan.OwnedBytes() + uint64(len(codecs))*codecOverheadBytes
	if used > maximumMetadata {
		return nil, sqlError("54000", "COPY codec metadata budget exceeded")
	}
	for i, column := range plan.columns {
		var size uint64
		var err error
		if plan.options.Format == Binary {
			codecs[i].binary, err = builtin.ResolveTypeBinaryCodec(ctx, column.Layout, options, maximumMetadata-used)
			size = codecs[i].binary.OwnedBytes
		} else {
			codecs[i].text, err = builtin.ResolveTypeTextCodec(ctx, column.Layout, options, maximumMetadata-used)
			size = codecs[i].text.OwnedBytes
		}
		if err != nil {
			return nil, err
		}
		if size > maximumMetadata-used {
			return nil, sqlError("54000", "COPY codec metadata budget exceeded")
		}
		used += size
	}
	maximum := min(streams.MaxRecordBytes, maximumRecord)
	if maximum == 0 {
		return nil, sqlError("22023", "invalid COPY record limit")
	}
	return &Writer{plan: plan, streams: streams, codecs: codecs, maximum: maximum, runtime: runtime}, ctx.Err()
}

// Write drains rows through the COPY encoder. The count includes only complete
// records accepted by the caller's Writer before an error.
func (writer *Writer) Write(ctx context.Context, rows RowReader) (uint64, error) {
	if writer == nil || rows == nil || writer.maximum == 0 {
		return 0, sqlError("22023", "invalid COPY writer invocation")
	}
	if err := writer.writeHeader(ctx); err != nil {
		return 0, err
	}
	batch := columnar.Batch{MaxRows: columnar.DefaultBatchSize}
	var count uint64
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		err := rows.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if err := batch.Validate(columnar.DefaultBatchSize); err != nil {
			return count, err
		}
		if len(batch.Columns) != len(writer.plan.columns) {
			return count, sqlError("42804", "COPY output row layout differs from prepared columns")
		}
		for row := range batch.Rows() {
			if err := writer.writeRow(ctx, &batch, row); err != nil {
				return count, err
			}
			count++
		}
	}
	if writer.plan.options.Format == Binary {
		var trailer [2]byte
		binary.BigEndian.PutUint16(trailer[:], math.MaxUint16)
		if err := writeAll(ctx, writer.streams.Writer, trailer[:]); err != nil {
			return count, err
		}
	}
	return count, nil
}

func (writer *Writer) writeHeader(ctx context.Context) error {
	if writer.plan.options.Format == Binary {
		header := make([]byte, 0, 19)
		header = append(header, binarySignature[:]...)
		header = binary.BigEndian.AppendUint32(header, 0)
		header = binary.BigEndian.AppendUint32(header, 0)
		return writeAll(ctx, writer.streams.Writer, header)
	}
	if writer.plan.options.Header == NoHeader {
		return nil
	}
	fields := make([]string, len(writer.plan.columns))
	for i := range writer.plan.columns {
		fields[i] = writer.plan.columns[i].Name
	}
	record, err := writer.encodeStrings(ctx, fields, nil)
	if err != nil {
		return err
	}
	return writeAll(ctx, writer.streams.Writer, record)
}

func (writer *Writer) writeRow(ctx context.Context, batch *columnar.Batch, row int) error {
	lane := batch.Lane(row)
	values := make([]model.Value, len(batch.Columns))
	for i := range batch.Columns {
		value, _, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, writer.maximum)
		if err != nil {
			return err
		}
		if value.Type != writer.plan.columns[i].Layout.Type.Storage {
			return sqlError("42804", "COPY output value differs from prepared column type")
		}
		values[i] = value
	}
	if writer.plan.options.Format == Binary {
		return writer.writeBinaryRow(ctx, values)
	}
	fields := make([]string, len(values))
	nulls := make([]bool, len(values))
	for i, value := range values {
		if value.IsNull {
			nulls[i] = true
			continue
		}
		result, err := writer.codecs[i].text.Output(ctx, []model.Value{value}, writer.callBudget())
		if err != nil {
			return err
		}
		if result.Type != model.TypeString || result.IsNull {
			return sqlError("42804", "COPY text codec returned an invalid value")
		}
		fields[i] = result.StrVal
	}
	record, err := writer.encodeStrings(ctx, fields, nulls)
	if err != nil {
		return err
	}
	return writeAll(ctx, writer.streams.Writer, record)
}

func (writer *Writer) callBudget() builtin.CallBudget {
	return builtin.CallBudget{
		OutputBytes: int64(min(writer.maximum, math.MaxInt64)), MemoryBytes: writer.maximum,
		Temporal: writer.runtime.Temporal, Relations: writer.runtime.Relations, Settings: writer.runtime.Settings, TextSearch: writer.runtime.TextSearch, Advisory: writer.runtime.Advisory, Native: writer.runtime.Native,
	}
}

func (writer *Writer) encodeStrings(ctx context.Context, fields []string, nulls []bool) ([]byte, error) {
	output := make([]byte, 0, min(int(writer.maximum), 256))
	for i, field := range fields {
		if i != 0 {
			output = append(output, writer.plan.options.Delimiter)
		}
		isNull := nulls != nil && nulls[i]
		if writer.plan.options.Format == CSV {
			output = writer.appendCSVField(output, field, isNull, i)
		} else if isNull {
			output = append(output, writer.plan.options.Null...)
		} else {
			start := len(output)
			output = appendTextField(output, field, writer.plan.options.Delimiter)
			encoded := string(output[start:])
			if len(field) != 0 && (encoded == writer.plan.options.Null || writer.plan.options.HasDefault && encoded == writer.plan.options.Default) {
				output = output[:start]
				output = append(output, '\\', '0'+field[0]/64, '0'+field[0]/8%8, '0'+field[0]%8)
				output = appendTextField(output, field[1:], writer.plan.options.Delimiter)
			}
		}
		if uint64(len(output))+1 > writer.maximum {
			return nil, sqlError("54000", "COPY output record exceeds limit")
		}
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil { return nil, err }
		}
	}
	output = append(output, '\n')
	return output, nil
}

func appendTextField(output []byte, field string, delimiter byte) []byte {
	for i := range len(field) {
		value := field[i]
		switch value {
		case '\b': output = append(output, '\\', 'b')
		case '\f': output = append(output, '\\', 'f')
		case '\n': output = append(output, '\\', 'n')
		case '\r': output = append(output, '\\', 'r')
		case '\t': output = append(output, '\\', 't')
		case '\v': output = append(output, '\\', 'v')
		case '\\': output = append(output, '\\', '\\')
		default:
			if value == delimiter {
				output = append(output, '\\', value)
			} else {
				output = append(output, value)
			}
		}
	}
	return output
}


func (writer *Writer) appendCSVField(output []byte, field string, null bool, column int) []byte {
	if null {
		return append(output, writer.plan.options.Null...)
	}
	force := writer.plan.columns[column].ForceQuote
	quote := force || field == writer.plan.options.Null || writer.plan.options.HasDefault && field == writer.plan.options.Default ||
		strings.IndexByte(field, writer.plan.options.Delimiter) >= 0 || strings.IndexByte(field, writer.plan.options.Quote) >= 0 ||
		strings.IndexByte(field, '\r') >= 0 || strings.IndexByte(field, '\n') >= 0 || len(writer.plan.columns) == 1 && field == `\.`
	if !quote {
		return append(output, field...)
	}
	output = append(output, writer.plan.options.Quote)
	for i := range len(field) {
		value := field[i]
		if value == writer.plan.options.Quote || value == writer.plan.options.Escape {
			output = append(output, writer.plan.options.Escape)
		}
		output = append(output, value)
	}
	return append(output, writer.plan.options.Quote)
}

func (writer *Writer) writeBinaryRow(ctx context.Context, values []model.Value) error {
	if len(values) > math.MaxInt16 {
		return sqlError("54000", "COPY binary row has too many columns")
	}
	if writer.maximum < 2 {
		return sqlError("54000", "COPY binary record exceeds limit")
	}
	output := make([]byte, 0, min(int(writer.maximum), 256))
	output = binary.BigEndian.AppendUint16(output, uint16(len(values)))
	for i, value := range values {
		if uint64(len(output))+4 > writer.maximum {
			return sqlError("54000", "COPY binary record exceeds limit")
		}
		if value.IsNull {
			output = binary.BigEndian.AppendUint32(output, math.MaxUint32)
			continue
		}
		budget := writer.callBudget()
		budget.OutputBytes -= int64(len(output))
		budget.MemoryBytes -= min(budget.MemoryBytes, uint64(len(output)))
		encoded, err := writer.codecs[i].binary.Output(ctx, []model.Value{value}, budget)
		if err != nil {
			return err
		}
		if encoded.Type != model.TypeBytes || encoded.IsNull || len(encoded.BytesVal) > math.MaxInt32 {
			return sqlError("54000", "COPY binary field exceeds protocol limit")
		}
		if uint64(len(output))+4+uint64(len(encoded.BytesVal)) > writer.maximum {
			return sqlError("54000", "COPY binary record exceeds limit")
		}
		output = binary.BigEndian.AppendUint32(output, uint32(len(encoded.BytesVal)))
		output = append(output, encoded.BytesVal...)
	}
	return writeAll(ctx, writer.streams.Writer, output)
}

func writeAll(ctx context.Context, target io.Writer, data []byte) error {
	if target == nil {
		return sqlError("22023", "COPY writer is missing")
	}
	for len(data) != 0 {
		if err := ctx.Err(); err != nil { return err }
		written, err := target.Write(data)
		if written < 0 || written > len(data) { return io.ErrShortWrite }
		data = data[written:]
		if err != nil { return err }
		if written == 0 { return io.ErrShortWrite }
	}
	return nil
}
