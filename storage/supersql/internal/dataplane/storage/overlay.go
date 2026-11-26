package storage

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// OverlayView is a frozen, structurally shared transaction write set. The
// transaction owns it; a snapshot retains it through all of its cursor lifetimes.
// Lookup resolves point/range ordinal precedence, including absent-key deletes.
// Scan orders points by key and ranges by their activation boundary: start in
// forward scans, end in reverse scans. Equal forward boundaries use descending
// ordinals; equal reverse boundaries emit points before ranges, then descending
// ordinals within each kind. Emit ranges crossing either requested bound.
// Neither operation may expose subsequent transaction changes.
type OverlayView interface {
	Lookup(context.Context, ObjectRef, []byte) (Mutation, bool, error)
	Scan(context.Context, OverlayScanRequest) (MutationCursor, error)
	Retain() (OverlayView, error)
	Close() error
}

type OverlayScanRequest struct {
	Target    ObjectRef
	StartKey  []byte
	EndKey    []byte
	Direction ScanDirection
}

// CompareVersion orders both durable versions and same-transaction operations.
func CompareVersion(aTS model.Timestamp, aOrdinal uint64, bTS model.Timestamp, bOrdinal uint64) int {
	if aTS < bTS {
		return -1
	}
	if aTS > bTS {
		return 1
	}
	if aOrdinal < bOrdinal {
		return -1
	}
	if aOrdinal > bOrdinal {
		return 1
	}
	return 0
}
