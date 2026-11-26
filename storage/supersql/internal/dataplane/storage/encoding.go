package storage

import (
	"errors"
	"hash/crc32"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const MutationEncodingVersion uint32 = 1
const maxValueDepth = 64

var ErrInvalidEncoding = errors.New("invalid canonical mutation encoding")
var ErrDecodeBudget = errors.New("mutation decoding exceeds allocation budget")

// The canonical representation is little endian, with uint64 lengths/counts.
// CRC fields are excluded from their own payloads. Strings retain their exact
// bytes; floats retain every IEEE bit, including signed zero and NaN payloads.
// One encoder is reused for the batch; no per-value hashing buffers are built.
type mutationEncoder struct {
	writer io.Writer
	buffer [4096]byte
	used   int
	size   uint64
	err    error
}

func (e *mutationEncoder) flush() {
	if e.err != nil || e.used == 0 {
		return
	}
	n, err := e.writer.Write(e.buffer[:e.used])
	if err == nil && n != e.used {
		err = io.ErrShortWrite
	}
	e.err = err
	e.used = 0
}

func (e *mutationEncoder) number(v uint64, width int) {
	if e.writer == nil {
		e.addSize(uint64(width))
		return
	}
	if len(e.buffer)-e.used < width {
		e.flush()
	}
	if e.err != nil {
		return
	}
	for i := range width {
		e.buffer[e.used+i] = byte(v >> (8 * i))
	}
	e.used += width
}

func (e *mutationEncoder) bytes(v []byte) {
	e.number(uint64(len(v)), 8)
	if e.writer == nil {
		e.addSize(uint64(len(v)))
		return
	}
	for len(v) != 0 && e.err == nil {
		if e.used == len(e.buffer) {
			e.flush()
		}
		n := copy(e.buffer[e.used:], v)
		e.used += n
		v = v[n:]
	}
}

func (e *mutationEncoder) text(v string) {
	e.number(uint64(len(v)), 8)
	if e.writer == nil {
		e.addSize(uint64(len(v)))
		return
	}
	for len(v) != 0 && e.err == nil {
		if e.used == len(e.buffer) {
			e.flush()
		}
		n := copy(e.buffer[e.used:], v)
		e.used += n
		v = v[n:]
	}
}

func (e *mutationEncoder) value(v model.Value, depth int) {
	if e.err != nil {
		return
	}
	if depth > maxValueDepth || !v.Type.Valid() {
		e.err = ErrInvalidEncoding
		return
	}
	e.number(uint64(v.Type), 1)
	if v.IsNull {
		e.number(1, 1)
		return
	}
	e.number(0, 1)
	switch v.Type {
	case model.TypeInt64, model.TypeTimestamp:
		e.number(uint64(v.IntVal), 8)
	case model.TypeBoolean:
		if v.IntVal != 0 && v.IntVal != 1 {
			e.err = ErrInvalidEncoding
			return
		}
		e.number(uint64(v.IntVal), 1)
	case model.TypeFloat64:
		e.number(math.Float64bits(v.FloatVal), 8)
	case model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		e.text(v.StrVal)
	case model.TypeBytes:
		e.bytes(v.BytesVal)
	case model.TypeVectorF32:
		e.number(uint64(len(v.VectorVal)), 8)
		if e.writer == nil {
			e.addSize(uint64(len(v.VectorVal)) * 4)
			return
		}
		for _, value := range v.VectorVal {
			e.number(uint64(math.Float32bits(value)), 4)
			if e.err != nil {
				return
			}
		}
	case model.TypeInterval:
		e.number(uint64(uint32(v.IntervalVal.Months)), 4)
		e.number(uint64(uint32(v.IntervalVal.Days)), 4)
		e.number(uint64(v.IntervalVal.Microseconds), 8)
	case model.TypeArray, model.TypeComposite:
		if err := validateDimensions(v); err != nil {
			e.err = err
			return
		}
		e.number(uint64(len(v.Dimensions)), 1)
		for _, dim := range v.Dimensions {
			e.number(uint64(uint32(dim.Length)), 4)
			e.number(uint64(uint32(dim.LowerBound)), 4)
		}
		e.number(uint64(len(v.ArrayVal)), 8)
		for _, element := range v.ArrayVal {
			e.value(element, depth+1)
			if e.err != nil {
				return
			}
		}
	default:
		e.err = ErrInvalidEncoding
	}
}

func validateDimensions(v model.Value) error {
	if v.Type == model.TypeComposite {
		if len(v.Dimensions) != 0 {
			return ErrInvalidEncoding
		}
		return nil
	}
	if err := model.ValidateArrayShape(v.Dimensions, uint64(len(v.ArrayVal))); err != nil {
		return errors.Join(ErrInvalidEncoding, err)
	}
	return nil
}

func (e *mutationEncoder) mutation(m Mutation) {
	if err := m.ValidateShape(); err != nil {
		e.err = err
		return
	}
	e.number(uint64(MutationEncodingVersion), 4)
	e.number(uint64(m.Op), 1)
	e.number(uint64(m.Target.ObjectID), 8)
	e.number(m.Target.Generation, 8)
	e.number(m.Target.DescriptorVersion, 8)
	e.number(uint64(m.Target.EncodingVersion), 4)
	e.number(m.Ordinal, 8)
	e.bytes(m.Key)
	e.bytes(m.EndKey)
	e.number(uint64(len(m.ColumnIDs)), 8)
	for i, id := range m.ColumnIDs {
		e.number(uint64(id), 4)
		e.value(m.Values[i], 0)
		if e.err != nil {
			return
		}
	}
}

// EncodeMutation may write a prefix on failure. Its caller must discard that
// frame; durability requires a separately validated whole-transaction commit.
func EncodeMutation(w io.Writer, m Mutation) error {
	e := mutationEncoder{writer: w}
	e.mutation(m)
	e.flush()
	return e.err
}

func MutationChecksum(m Mutation) (uint32, error) {
	h := crc32.NewIEEE()
	if err := EncodeMutation(h, m); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// EncodedMutationSize checks lengths and representation without materializing
// the payload. WAL framing can then stream arbitrarily large values in chunks.
func EncodedMutationSize(m Mutation) (uint64, error) {
	e := mutationEncoder{}
	e.mutation(m)
	return e.size, e.err
}

func (e *mutationEncoder) addSize(n uint64) {
	if n > math.MaxUint64-e.size {
		e.err = ErrInvalidEncoding
		return
	}
	e.size += n
}
