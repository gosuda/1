package explain

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var ErrInvalid = errors.New("invalid plan explanation request")
var ErrLimit = errors.New("plan explanation resource limit exceeded")

type jsonWriter struct {
	ctx       context.Context
	output    io.Writer
	remaining uint64
	depth     uint32
	err       error
}

func (w *jsonWriter) Write(data []byte) (int, error) {
	if err := w.admit(len(data)); err != nil {
		return 0, err
	}
	n, err := w.output.Write(data)
	return w.written(n, len(data), err)
}

func (w *jsonWriter) WriteString(text string) (int, error) {
	if err := w.admit(len(text)); err != nil {
		return 0, err
	}
	n, err := io.WriteString(w.output, text)
	return w.written(n, len(text), err)
}

func (w *jsonWriter) admit(size int) error {
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return err
	}
	if uint64(size) > w.remaining {
		w.err = ErrLimit
	}
	return w.err
}

func (w *jsonWriter) written(n, size int, err error) (int, error) {
	if n < 0 || n > size {
		n, err = 0, io.ErrShortWrite
	}
	w.remaining -= uint64(n)
	if err == nil && n != size {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func (w *jsonWriter) raw(text string) {
	if w.err == nil {
		_, w.err = w.WriteString(text)
	}
}

// fixed is used only for fixed-size metadata, never strings, slices or datums.
func (w *jsonWriter) fixed(value any) {
	if w.err != nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		w.err = err
		return
	}
	_, w.err = w.Write(data)
}

func (w *jsonWriter) text(value string) {
	w.raw(`"`)
	for len(value) != 0 && w.err == nil {
		end := min(128, len(value))
		if end < len(value) {
			for end > 0 && !utf8.RuneStart(value[end]) {
				end--
			}
			end = cmp.Or(end, min(128, len(value)))
		}
		encoded, err := json.Marshal(value[:end])
		if err != nil {
			w.err = err
			return
		}
		_, w.err = w.Write(encoded[1 : len(encoded)-1])
		value = value[end:]
	}
	w.raw(`"`)
}

func (w *jsonWriter) list(count int, emit func(int)) {
	w.raw("[")
	for i := 0; i < count && w.err == nil; i++ {
		if i != 0 {
			w.raw(",")
		}
		emit(i)
	}
	w.raw("]")
}

func (w *jsonWriter) number(value float64) {
	switch {
	case math.IsNaN(value):
		w.text("NaN")
	case math.IsInf(value, 1):
		w.text("Infinity")
	case math.IsInf(value, -1):
		w.text("-Infinity")
	default:
		w.fixed(value)
	}
}

func (w *jsonWriter) datum(value model.Value, depth uint32) {
	if w.err != nil {
		return
	}
	if depth >= w.depth {
		w.err = ErrLimit
		return
	}
	w.raw(`{"Storage":`)
	w.fixed(value.Type)
	w.raw(`,"Null":`)
	w.fixed(value.IsNull)
	if value.IsNull {
		w.raw("}")
		return
	}
	w.raw(`,"Value":`)
	switch value.Type {
	case model.TypeInt64, model.TypeTimestamp:
		w.fixed(value.IntVal)
	case model.TypeBoolean:
		w.fixed(value.IntVal != 0)
	case model.TypeFloat64:
		w.number(value.FloatVal)
	case model.TypeString, model.TypeJSONB, model.TypeNumeric:
		w.text(value.StrVal)
	case model.TypeBytes, model.TypeUUID:
		w.raw(`"`)
		encoder := base64.NewEncoder(base64.StdEncoding, w)
		_, err := encoder.Write(value.BytesVal)
		w.err = errors.Join(w.err, err, encoder.Close())
		w.raw(`"`)
	case model.TypeVectorF32:
		w.list(len(value.VectorVal), func(i int) { w.number(float64(value.VectorVal[i])) })
	case model.TypeArray, model.TypeComposite:
		w.list(len(value.ArrayVal), func(i int) { w.datum(value.ArrayVal[i], depth+1) })
		if value.Type == model.TypeArray {
			w.raw(`,"Dimensions":`)
			w.list(len(value.Dimensions), func(i int) { w.fixed(value.Dimensions[i]) })
		}
	case model.TypeInterval:
		w.fixed(value.IntervalVal)
	default:
		w.err = ErrInvalid
	}
	w.raw("}")
}
