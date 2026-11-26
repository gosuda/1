package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeIOWriter struct {
	data  []byte
	size  uint64
	limit int64
}

func (writer *rangeIOWriter) append(ctx context.Context, data []byte) error {
	size := uint64(len(data))
	if size > math.MaxUint64-writer.size || writer.size+size > math.MaxInt64 {
		return sqlError("54000", "range output exceeds memory limit")
	}
	if err := outputBudget(int64(writer.size+size), writer.limit); err != nil {
		return err
	}
	if writer.data != nil {
		for start := 0; start < len(data); {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(start+cancellationChunk, len(data))
			copy(writer.data[int(writer.size)+start:], data[start:end])
			start = end
		}
	}
	writer.size += size
	return ctx.Err()
}
func (writer *rangeIOWriter) byte(ctx context.Context, value byte) error {
	return writer.append(ctx, []byte{value})
}
func (writer *rangeIOWriter) uint32(ctx context.Context, value uint32) error {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	return writer.append(ctx, data[:])
}
func (writer *rangeIOWriter) text(ctx context.Context, value string) error {
	return writer.append(ctx, unsafe.Slice(unsafe.StringData(value), len(value)))
}
func (writer *rangeIOWriter) boundText(ctx context.Context, text string) error {
	quoted := len(text) == 0
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		switch text[i] {
		case '"', '\\', '(', ')', '[', ']', ',':
			quoted = true
		default:
			if rangeIOSpace(text[i]) {
				quoted = true
			}
		}
	}
	if !quoted {
		return writer.text(ctx, text)
	}
	if err := writer.byte(ctx, '"'); err != nil {
		return err
	}
	start := 0
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if text[i] == '"' || text[i] == '\\' {
			if err := writer.text(ctx, text[start:i]); err != nil {
				return err
			}
			if err := writer.byte(ctx, text[i]); err != nil {
				return err
			}
			start = i
		}
	}
	if err := writer.text(ctx, text[start:]); err != nil {
		return err
	}
	return writer.byte(ctx, '"')
}
func (codec *rangeIOCodec) writeRange(ctx context.Context, datum rangeDatum, writer *rangeIOWriter, element *arrayElementCall, budget CallBudget) error {
	if datum.empty {
		if codec.binary {
			return writer.byte(ctx, rangeEmptyFlag)
		}
		return writer.text(ctx, "empty")
	}
	if codec.binary {
		flags := byte(0)
		if datum.lower.infinite {
			flags |= rangeLowerInfiniteFlag
		} else if datum.lower.inclusive {
			flags |= rangeLowerInclusiveFlag
		}
		if datum.upper.infinite {
			flags |= rangeUpperInfiniteFlag
		} else if datum.upper.inclusive {
			flags |= rangeUpperInclusiveFlag
		}
		if err := writer.byte(ctx, flags); err != nil {
			return err
		}
	} else {
		ch := byte('(')
		if datum.lower.inclusive {
			ch = '['
		}
		if err := writer.byte(ctx, ch); err != nil {
			return err
		}
	}
	for i, bound := range [2]rangeBound{datum.lower, datum.upper} {
		if i == 1 && !codec.binary {
			if err := writer.byte(ctx, ','); err != nil {
				return err
			}
		}
		if bound.infinite {
			continue
		}
		value, err := element.invoke(ctx, borrowRangeBoundValue(codec.ops.family, bound), budget)
		if err != nil {
			return err
		}
		if codec.binary {
			if value.Type != model.TypeBytes || value.IsNull {
				return sqlError("XX000", "range subtype send did not return bytea")
			}
			if uint64(len(value.BytesVal)) > math.MaxInt32 {
				return sqlError("54000", "range bound wire payload too large")
			}
			if err := writer.uint32(ctx, uint32(len(value.BytesVal))); err != nil {
				return err
			}
			if err := writer.append(ctx, value.BytesVal); err != nil {
				return err
			}
		} else {
			if value.Type != model.TypeString || value.IsNull {
				return sqlError("XX000", "range subtype output did not return cstring")
			}
			if err := writer.boundText(ctx, value.StrVal); err != nil {
				return err
			}
		}
	}
	if !codec.binary {
		ch := byte(')')
		if datum.upper.inclusive {
			ch = ']'
		}
		return writer.byte(ctx, ch)
	}
	return nil
}
func (codec *rangeIOCodec) write(ctx context.Context, value model.Value, writer *rangeIOWriter, budget CallBudget) error {
	element := arrayElementCall{function: codec.element}
	if !codec.multi {
		datum, err := decodeRange(ctx, codec.ops.family, value)
		if err != nil {
			return err
		}
		return codec.writeRange(ctx, datum, writer, &element, budget)
	}
	cursor, err := newMultirangeCursor(codec.ops.family, value)
	if err != nil {
		return err
	}
	if codec.binary {
		count := binary.BigEndian.Uint32(value.BytesVal)
		if count > math.MaxInt32 {
			return invalidRangeValue()
		}
		if err := writer.uint32(ctx, count); err != nil {
			return err
		}
	} else {
		if err := writer.byte(ctx, '{'); err != nil {
			return err
		}
	}
	first := true
	for {
		datum, ok, err := cursor.next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		prefix := writer.size
		if codec.binary {
			if err := writer.uint32(ctx, 0); err != nil {
				return err
			}
		} else if !first {
			if err := writer.byte(ctx, ','); err != nil {
				return err
			}
		}
		if err := codec.writeRange(ctx, datum, writer, &element, budget); err != nil {
			return err
		}
		if codec.binary {
			size := writer.size - prefix - 4
			if size > math.MaxInt32 {
				return sqlError("54000", "range wire payload too large")
			}
			if writer.data != nil {
				binary.BigEndian.PutUint32(writer.data[int(prefix):], uint32(size))
			}
		}
		first = false
	}
	if !codec.binary {
		return writer.byte(ctx, '}')
	}
	return nil
}
func (codec *rangeIOCodec) output(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	counter := rangeIOWriter{limit: budget.OutputBytes}
	if err := codec.write(ctx, arguments[0], &counter, budget); err != nil {
		return model.Value{}, err
	}
	buffer, err := newByteResult(ctx, counter.size, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	nested, err := budget.After(counter.size)
	if err != nil {
		return model.Value{}, err
	}
	writer := rangeIOWriter{data: buffer.data, limit: budget.OutputBytes}
	if err := codec.write(ctx, arguments[0], &writer, nested); err != nil {
		return model.Value{}, err
	}
	if codec.binary {
		return buffer.value(), nil
	}
	return model.StringVal(unsafe.String(unsafe.SliceData(buffer.data), len(buffer.data))), nil
}
