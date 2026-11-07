package storage

import "github.com/gosuda/supersql-alpha/internal/model"

type ScanDirection uint8

const (
	ScanForward ScanDirection = iota
	ScanReverse
)

// A finite endpoint contains a nonempty leading key tuple. Inclusive endpoints
// include every extension of that tuple; exclusive endpoints exclude them all.
// Unbounded endpoints have no values. No arithmetic successor is required.
type KeyBound struct {
	Values    []model.Value
	Inclusive bool
	Unbounded bool
}

type KeySpan struct {
	Lower KeyBound
	Upper KeyBound
}

// EncodedKeyBound is an opaque endpoint produced by the request's KeyEncoder.
// Prefix preserves the ordering of a partial composite-key endpoint.
type EncodedKeyBound struct {
	Key       []byte
	Inclusive bool
	Prefix    bool
	Unbounded bool
}

type EncodedKeySpan struct {
	Lower EncodedKeyBound
	Upper EncodedKeyBound
}

type KeyColumn struct {
	ColumnID   model.ColumnID
	Type       model.Type
	Children   []model.TypeLayout
	Descending bool
	NullsFirst bool
}

// Enabled distinguishes LIMIT 0 from an absent limit.
type ScanLimit struct {
	Count   uint64
	Enabled bool
}

// Requests borrow immutable layout, spans and predicate data through planning.
// Empty spans produce no rows; full scans require an explicit unbounded span.
type PushdownRequest struct {
	Target    ObjectRef
	// Occurrence identifies one bound RTE across its physical scan requests.
	Occurrence model.ScanOccurrence
	IndexID   model.IndexID
	Layout    []ColumnLayout
	KeyLayout []KeyColumn
	ColumnIDs []model.ColumnID
	Spans     []KeySpan
	// EncodedSpans is mutually exclusive with Spans. Transaction admission uses
	// it to publish exactly the already-locked subspans without decoding keys.
	EncodedSpans []EncodedKeySpan
	Direction ScanDirection
	Predicate Predicate
	SafeLimit ScanLimit
}

// KeyOrdinal refers to KeyLayout, not to a query expression or PAX offset.
type ScanOrdering struct {
	KeyOrdinal uint32
	Type       model.Type
	Descending bool
	NullsFirst bool
}

type ScanEstimate struct {
	Rows             float64
	ReadBytes        float64
	DecodeBytes      float64
	VisibilityChecks float64
	IndexProbes      float64
}

// Plans own or retain immutable metadata until ReleasePlan or reader Close.
// ReleasePlan requires every cursor from that plan to be closed. Binding is
// backend-issued and validated against the reader, target and complete layout.
// Accepted filters never excuse omitting rows shadowed by newer versions.
// Residual must preserve the original predicate's SQL three-valued semantics;
// a necessary prefilter of an OR retains the original OR as its residual.
type PushdownPlan struct {
	Request      PushdownRequest
	Accepted     Predicate
	Residual     Predicate
	Projection   []model.ColumnID
	AppliedLimit ScanLimit
	Ordering     []ScanOrdering
	Estimate     ScanEstimate
	SnapshotTS   model.Timestamp
	FenceID      [16]byte
	Binding      [32]byte
}
