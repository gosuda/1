package columnar

import (
	"errors"
	"fmt"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type Encoding uint8

const (
	Flat Encoding = iota
	Constant
	Dictionary
)

// Range addresses bytes, float32 elements, or child-vector lanes according to
// the vector's storage type. Separate lengths allow borrowed PAX payloads with
// interleaved length headers without copying their bytes into a new arena.
type Range struct {
	Offset uint32
	Length uint32
}

// Vector is a borrowed column view. Null bits are one for NULL, matching PAX.
// Dictionary nulls address logical lanes; codes address non-null payload lanes.
// Constant vectors require only one payload lane regardless of Length.
type Vector struct {
	Type        model.Type
	Encoding    Encoding
	Offset      int
	Length      int
	Nulls       []byte
	Indices     []uint32
	Int64       []int64
	Float64     []float64
	Data        []byte
	Ranges      []Range
	Float32     []float32
	Intervals   []model.Interval
	Children    []Vector
	ShapeRanges []Range
	Dimensions  []model.ArrayDimension
}

var ErrInvalidVector = errors.New("invalid columnar vector")

func (v *Vector) IsNull(row int) bool {
	if row < 0 || row >= v.Length {
		panic("columnar lane out of bounds")
	}
	index := v.Offset + row
	if v.Encoding == Constant {
		index = 0
	}
	return len(v.Nulls) != 0 && v.Nulls[index/8]&(1<<uint(index%8)) != 0
}

// ValueIndex is for scalar bridges and consumers outside typed hot loops.
// Vector kernels select flat/constant/dictionary dispatch once per batch.
func (v *Vector) ValueIndex(row int) int {
	if row < 0 || row >= v.Length {
		panic("columnar lane out of bounds")
	}
	index := v.Offset + row
	switch v.Encoding {
	case Flat:
		return index
	case Constant:
		return 0
	case Dictionary:
		return int(v.Indices[index])
	default:
		panic("unvalidated columnar encoding")
	}
}

func (v *Vector) Validate() error { return v.validate(0) }

// MatchesLayout reports whether the vector resolves the same physical
// identities as layout: the same Type at every node and the same children in
// the same order. Encodings and lengths are irrelevant to the comparison.
func (v *Vector) MatchesLayout(layout model.TypeLayout) bool {
	if v.Type != layout.Type || len(v.Children) != len(layout.Children) {
		return false
	}
	for i := range v.Children {
		if !v.Children[i].MatchesLayout(layout.Children[i]) {
			return false
		}
	}
	return true
}

func (v *Vector) validate(depth int) error {
	if depth > 512 || v.Type.ID == 0 {
		return ErrInvalidVector
	}
	if v.Offset < 0 || v.Length < 0 {
		return ErrInvalidVector
	}
	if v.Offset > math.MaxInt-v.Length {
		return ErrInvalidVector
	}
	end := v.Offset + v.Length
	nullEnd := end
	payloadRows := end
	switch v.Encoding {
	case Flat:
	case Constant:
		if v.Offset != 0 {
			return ErrInvalidVector
		}
		payloadRows, nullEnd = 0, 0
		if v.Length > 0 {
			payloadRows, nullEnd = 1, 1
		}
	case Dictionary:
		if len(v.Indices) < end {
			return ErrInvalidVector
		}
		payloadRows = 0
	default:
		return ErrInvalidVector
	}
	bitmapBytes := nullEnd / 8
	if nullEnd%8 != 0 {
		bitmapBytes++
	}
	if len(v.Nulls) != 0 && len(v.Nulls) < bitmapBytes {
		return ErrInvalidVector
	}
	if v.Encoding == Dictionary {
		for row := range v.Length {
			if v.IsNull(row) {
				continue
			}
			index := v.Indices[v.Offset+row]
			if uint64(index) >= uint64(math.MaxInt) {
				return ErrInvalidVector
			}
			payloadRows = max(payloadRows, int(index)+1)
		}
	}
	if !v.Type.Storage.Valid() {
		return fmt.Errorf("%w: storage type %d", ErrInvalidVector, v.Type.Storage)
	}
	if v.Encoding == Constant && v.Length > 0 && v.IsNull(0) {
		return nil
	}
	switch v.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		if len(v.Int64) < payloadRows {
			return ErrInvalidVector
		}
		if v.Type.Storage == model.TypeBoolean {
			for row := range v.Length {
				if v.IsNull(row) {
					continue
				}
				value := v.Int64[v.ValueIndex(row)]
				if value != 0 && value != 1 {
					return ErrInvalidVector
				}
			}
		}
	case model.TypeFloat64:
		if len(v.Float64) < payloadRows {
			return ErrInvalidVector
		}
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		if err := v.validateRanges(v.Ranges, payloadRows, len(v.Data)); err != nil {
			return err
		}
	case model.TypeVectorF32:
		if err := v.validateRanges(v.Ranges, payloadRows, len(v.Float32)); err != nil {
			return err
		}
	case model.TypeInterval:
		if len(v.Intervals) < payloadRows {
			return ErrInvalidVector
		}
	case model.TypeArray:
		if len(v.Children) != 1 {
			return ErrInvalidVector
		}
		if err := v.validateRanges(v.Ranges, payloadRows, v.Children[0].Length); err != nil {
			return err
		}
		if err := v.validateRanges(v.ShapeRanges, payloadRows, len(v.Dimensions)); err != nil {
			return err
		}
		for row := range v.Length {
			if v.IsNull(row) {
				continue
			}
			index := v.ValueIndex(row)
			shape := v.ShapeRanges[index]
			end := uint64(shape.Offset) + uint64(shape.Length)
			dimensions := v.Dimensions[shape.Offset:end]
			if err := model.ValidateArrayShape(dimensions, uint64(v.Ranges[index].Length)); err != nil {
				return errors.Join(ErrInvalidVector, err)
			}
		}
	case model.TypeComposite:
		for i := range v.Children {
			if v.Children[i].Length < payloadRows {
				return ErrInvalidVector
			}
		}
	default:
		return fmt.Errorf("%w: storage type %d", ErrInvalidVector, v.Type.Storage)
	}
	for i := range v.Children {
		if err := v.Children[i].validate(depth + 1); err != nil {
			return err
		}
	}
	return nil
}

func (v *Vector) validateRanges(ranges []Range, count, size int) error {
	if len(ranges) < count || size < 0 {
		return ErrInvalidVector
	}
	for row := range v.Length {
		if v.IsNull(row) {
			continue
		}
		span := ranges[v.ValueIndex(row)]
		if uint64(span.Offset)+uint64(span.Length) > uint64(size) {
			return ErrInvalidVector
		}
	}
	return nil
}
