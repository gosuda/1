package storage

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// DecodeMutation owns every returned payload. Allocation is charged before each
// allocation; the caller separately accounts for its input buffer's lifetime.
func DecodeMutation(data []byte, maxAllocation uint64) (Mutation, error) {
	d := mutationDecoder{data: data, budget: maxAllocation}
	if d.number(4) != uint64(MutationEncodingVersion) {
		return Mutation{}, ErrInvalidEncoding
	}
	m := Mutation{Op: MutationOp(d.number(1))}
	m.Target.ObjectID = model.ObjectID(d.number(8))
	m.Target.Generation = d.number(8)
	m.Target.DescriptorVersion = d.number(8)
	m.Target.EncodingVersion = uint32(d.number(4))
	m.Ordinal = d.number(8)
	m.Key = d.ownedBytes()
	m.EndKey = d.ownedBytes()
	count := d.count(uint64(unsafe.Sizeof(model.Value{}))+4, 6)
	if d.err != nil {
		return Mutation{}, d.err
	}
	m.ColumnIDs = make([]model.ColumnID, count)
	m.Values = make([]model.Value, count)
	for i := range count {
		m.ColumnIDs[i] = model.ColumnID(d.number(4))
		m.Values[i] = d.value(0)
		if d.err != nil {
			return Mutation{}, d.err
		}
	}
	if len(d.data) != 0 {
		return Mutation{}, ErrInvalidEncoding
	}
	if err := m.ValidateShape(); err != nil {
		return Mutation{}, err
	}
	m.ChecksumCRC = crc32.ChecksumIEEE(data)
	return m, nil
}

type mutationDecoder struct {
	data   []byte
	budget uint64
	err    error
	region *decodeRegion
}

func (d *mutationDecoder) take(n uint64) []byte {
	if d.err != nil {
		return nil
	}
	if d.region != nil {
		if n > uint64(len(d.region.scratch)) {
			d.err = ErrInvalidEncoding
			return nil
		}
		value := d.region.scratch[:n]
		d.err = d.region.read(value)
		return value
	}
	if n > uint64(len(d.data)) {
		d.err = ErrInvalidEncoding
		return nil
	}
	value := d.data[:n]
	d.data = d.data[n:]
	return value
}

func (d *mutationDecoder) number(width int) uint64 {
	value := d.take(uint64(width))
	if d.err != nil {
		return 0
	}
	switch width {
	case 1:
		return uint64(value[0])
	case 4:
		return uint64(binary.LittleEndian.Uint32(value))
	case 8:
		return binary.LittleEndian.Uint64(value)
	}
	panic("invalid canonical integer width")
}

func (d *mutationDecoder) charge(n uint64) bool {
	if d.err != nil {
		return false
	}
	if n > d.budget {
		d.err = ErrDecodeBudget
		return false
	}
	d.budget -= n
	return true
}

func (d *mutationDecoder) count(elementBytes, minEncodedBytes uint64) int {
	n := d.number(8)
	if n > d.remaining()/minEncodedBytes {
		d.err = ErrInvalidEncoding
		return 0
	}
	if n > d.budget/elementBytes {
		d.err = ErrDecodeBudget
		return 0
	}
	if !d.charge(n * elementBytes) {
		return 0
	}
	return int(n)
}

func (d *mutationDecoder) ownedBytes() []byte {
	n := d.number(8)
	if d.region != nil {
		return d.streamBytes(n)
	}
	v := d.take(n)
	if !d.charge(n) {
		return nil
	}
	return bytes.Clone(v)
}

func (d *mutationDecoder) text() string {
	n := d.number(8)
	if d.region != nil {
		if n > d.budget/2 {
			d.err = ErrDecodeBudget
			return ""
		}
		value := d.streamBytes(n)
		if !d.charge(n) {
			return ""
		}
		text := string(value)
		d.budget += n
		return text
	}
	v := d.take(n)
	if !d.charge(n) {
		return ""
	}
	return string(v)
}

func (d *mutationDecoder) remaining() uint64 {
	if d.region != nil {
		return d.region.size - d.region.offset
	}
	return uint64(len(d.data))
}

func (d *mutationDecoder) streamBytes(n uint64) []byte {
	if d.err != nil {
		return nil
	}
	if n > d.remaining() {
		d.err = ErrInvalidEncoding
		return nil
	}
	if n > uint64(math.MaxInt) || !d.charge(n) {
		d.err = ErrDecodeBudget
		return nil
	}
	value := make([]byte, n)
	d.err = d.region.read(value)
	return value
}

func (d *mutationDecoder) value(depth int) model.Value {
	if d.err != nil {
		return model.Value{}
	}
	if depth > maxValueDepth {
		d.err = ErrInvalidEncoding
		return model.Value{}
	}
	v := model.Value{Type: model.DataType(d.number(1))}
	if d.err != nil {
		return model.Value{}
	}
	if !v.Type.Valid() {
		d.err = ErrInvalidEncoding
		return model.Value{}
	}
	null := d.number(1)
	if null > 1 {
		d.err = ErrInvalidEncoding
		return model.Value{}
	}
	v.IsNull = null == 1
	if v.IsNull {
		return v
	}
	switch v.Type {
	case model.TypeInt64, model.TypeTimestamp:
		v.IntVal = int64(d.number(8))
	case model.TypeBoolean:
		v.IntVal = int64(d.number(1))
		if v.IntVal > 1 {
			d.err = ErrInvalidEncoding
		}
	case model.TypeFloat64:
		v.FloatVal = math.Float64frombits(d.number(8))
	case model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		v.StrVal = d.text()
	case model.TypeBytes:
		v.BytesVal = d.ownedBytes()
	case model.TypeVectorF32:
		count := d.count(4, 4)
		if d.err != nil {
			return model.Value{}
		}
		v.VectorVal = make([]float32, count)
		for i := range v.VectorVal {
			v.VectorVal[i] = math.Float32frombits(uint32(d.number(4)))
		}
	case model.TypeInterval:
		v.IntervalVal = model.Interval{Months: int32(d.number(4)), Days: int32(d.number(4)), Microseconds: int64(d.number(8))}
	case model.TypeArray, model.TypeComposite:
		v = d.nested(v, depth)
	default:
		d.err = ErrInvalidEncoding
	}
	return v
}

func (d *mutationDecoder) nested(v model.Value, depth int) model.Value {
	dimensions := d.number(1)
	if dimensions > 6 {
		d.err = ErrInvalidEncoding
		return model.Value{}
	}
	if !d.charge(dimensions * uint64(unsafe.Sizeof(model.ArrayDimension{}))) {
		return model.Value{}
	}
	v.Dimensions = make([]model.ArrayDimension, int(dimensions))
	for i := range v.Dimensions {
		v.Dimensions[i] = model.ArrayDimension{Length: int32(d.number(4)), LowerBound: int32(d.number(4))}
	}
	count := d.count(uint64(unsafe.Sizeof(model.Value{})), 2)
	if d.err != nil {
		return model.Value{}
	}
	v.ArrayVal = make([]model.Value, count)
	for i := range v.ArrayVal {
		v.ArrayVal[i] = d.value(depth + 1)
		if d.err != nil {
			return model.Value{}
		}
	}
	if err := validateDimensions(v); err != nil {
		d.err = err
	}
	return v
}
