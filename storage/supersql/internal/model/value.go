package model

import (
	"bytes"
	"slices"
	"strings"
)

// DataType identifies a physical datum representation. SQL identity, typmods,
// and collation are carried by Type, not inferred from this storage tag.
// Values 0 through 12 retain the legacy PAX representation tags.
type DataType uint8

const (
	TypeAny DataType = iota
	TypeInt64
	TypeFloat64
	TypeTimestamp
	TypeBytes
	TypeString
	_ // Legacy LOB references remain backend-private.
	TypeVectorF32
	TypeJSONB
	TypeArray
	TypeBoolean
	TypeUUID
	TypeNumeric
	TypeComposite
	TypeInterval
)

type Type struct {
	ID        TypeID
	Modifier  int32
	Collation CollationID
	Storage   DataType
}

type ArrayDimension struct {
	Length     int32
	LowerBound int32
}

// Interval preserves PostgreSQL's independent month, day and microsecond fields.
type Interval struct {
	Months       int32
	Days         int32
	Microseconds int64
}

type Value struct {
	Type        DataType
	IsNull      bool
	IntVal      int64
	FloatVal    float64
	StrVal      string
	BytesVal    []byte
	VectorVal   []float32
	ArrayVal    []Value
	Dimensions  []ArrayDimension
	IntervalVal Interval
}

// Clone owns variable-width data, including nested arrays. Datum trees are
// acyclic and depth-bounded by binding/decoding before they enter storage.
func (v Value) Clone() Value {
	v.StrVal = strings.Clone(v.StrVal)
	v.BytesVal = bytes.Clone(v.BytesVal)
	v.VectorVal = slices.Clone(v.VectorVal)
	v.ArrayVal = CloneValues(v.ArrayVal)
	v.Dimensions = slices.Clone(v.Dimensions)
	return v
}

func CloneValues(values []Value) []Value {
	if values == nil {
		return nil
	}
	owned := make([]Value, len(values))
	for i := range values {
		owned[i] = values[i].Clone()
	}
	return owned
}

func Int64Val(value int64) Value     { return Value{Type: TypeInt64, IntVal: value} }
func Float64Val(value float64) Value { return Value{Type: TypeFloat64, FloatVal: value} }
func StringVal(value string) Value   { return Value{Type: TypeString, StrVal: value} }
func BytesVal(value []byte) Value    { return Value{Type: TypeBytes, BytesVal: value} }
func BoolVal(value bool) Value {
	if value {
		return Value{Type: TypeBoolean, IntVal: 1}
	}
	return Value{Type: TypeBoolean}
}
func NullVal(kind DataType) Value { return Value{Type: kind, IsNull: true} }

type Row struct {
	Key      []byte
	CommitTS Timestamp
	Ordinal  uint64
	Deleted  bool
	Values   []Value
}
