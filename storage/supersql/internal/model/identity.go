package model

import (
	"errors"
	"math"
	"time"
)

type ObjectID uint64

// CatalogCollectionObjectID is the lowest ObjectID reserved for catalog
// collections; user and legacy objects occupy [1, CatalogCollectionObjectID).
// The catalog store owns the three collection identities at and above it, so a
// mutation whose target is at least this floor rewrites catalog rows.
const CatalogCollectionObjectID ObjectID = ^ObjectID(0) - 2

type ColumnID uint32
type IndexID uint64
type TypeID uint32
type CollationID uint32
type FunctionID uint32
type OperatorID uint32
type SlotID uint32
type ScanOccurrence uint32
type CatalogRelationID uint16
type TxnID [16]byte

// Timestamp is a positive SI nanosecond count since 1970-01-01 UTC.
// It is not UnixNano; zero is reserved for absent metadata.
type Timestamp int64

var ErrTimestampRange = errors.New("timestamp outside positive SI nanosecond range")

func NewTimestamp(value int64) (Timestamp, error) {
	if value <= 0 {
		return 0, ErrTimestampRange
	}
	return Timestamp(value), nil
}

func (t Timestamp) Add(delta time.Duration) (Timestamp, error) {
	if t <= 0 {
		return 0, ErrTimestampRange
	}
	d := int64(delta)
	if d > 0 && int64(t) > math.MaxInt64-d {
		return 0, ErrTimestampRange
	}
	if d < 0 && d <= -int64(t) {
		return 0, ErrTimestampRange
	}
	return Timestamp(int64(t) + d), nil
}
