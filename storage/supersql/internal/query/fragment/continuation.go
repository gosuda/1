package fragment

import (
	"context"
	"errors"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrContinuationMismatch    = errors.New("continuation identity or request mismatch")
	ErrContinuationOrder       = errors.New("continuation key does not advance scan order")
	ErrContinuationOutsideSpan = errors.New("continuation key is outside the original spans")
)

type ScanIdentity struct {
	Snapshot model.Timestamp
	Plan     [32]byte
}

// Continuation owns immutable request and key data, not a snapshot pin. Callers
// must retain the reader's pin, or otherwise ensure retained history is available.
// Copies may share metadata; tokens are neither credentials nor a wire format.
// The zero value is invalid.
type Continuation struct {
	request     *storage.PushdownRequest
	identity    ScanIdentity
	fingerprint [32]byte
	accepted    []model.Value
}

// NewContinuation binds a logical scan independently of partition placement.
func NewContinuation(ctx context.Context, identity ScanIdentity, request storage.PushdownRequest, encoder storage.KeyEncoder, limits routing.Limits) (Continuation, error) {
	phase, err := continuationLimits(ctx, encoder, limits, true)
	if err != nil {
		return Continuation{}, err
	}
	if identity.Snapshot <= 0 || identity.Plan == ([32]byte{}) {
		return Continuation{}, ErrContinuationMismatch
	}
	owned, _, err := storage.CopyPushdownRequest(ctx, request, continuationMetadataBudget(phase)/2)
	if err != nil {
		return Continuation{}, err
	}
	fingerprint, err := continuationHash(ctx, identity, owned, phase)
	if err != nil {
		return Continuation{}, err
	}
	// Empty intersection validates the entire typed span union without retaining
	// a second, normalized copy of the original request.
	if _, err := routing.Intersect(ctx, owned.Spans, nil, owned.KeyLayout, encoder, phase); err != nil {
		return Continuation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Continuation{}, err
	}
	return Continuation{request: &owned, identity: identity, fingerprint: fingerprint}, nil
}

// Advance acknowledges a complete key, never a key prefix. Failure leaves both
// this token and all earlier tokens usable.
func (c Continuation) Advance(ctx context.Context, acceptedKey []model.Value, encoder storage.KeyEncoder, limits routing.Limits) (Continuation, error) {
	phase, err := continuationLimits(ctx, encoder, limits, false)
	if err != nil {
		return Continuation{}, err
	}
	if c.request == nil {
		return Continuation{}, ErrContinuationMismatch
	}
	if len(acceptedKey) != len(c.request.KeyLayout) {
		return Continuation{}, storage.ErrRangeInvalid
	}
	point := storage.KeyBound{Values: acceptedKey, Inclusive: true}
	intersection, err := routing.Intersect(ctx, c.request.Spans, []storage.KeySpan{{Lower: point, Upper: point}}, c.request.KeyLayout, encoder, phase)
	if err != nil {
		return Continuation{}, err
	}
	if len(intersection) == 0 {
		return Continuation{}, ErrContinuationOutsideSpan
	}
	// Intersect owns both endpoints. Retain just one; even equality-class
	// spellings are safe because ordering is established by the SQL encoder.
	key := intersection[0].Lower.Values
	if len(c.accepted) != 0 {
		remaining := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
		boundary := storage.KeyBound{Values: c.accepted}
		if c.request.Direction == storage.ScanReverse {
			remaining.Upper = boundary
		} else {
			remaining.Lower = boundary
		}
		ordered, err := routing.Intersect(ctx, []storage.KeySpan{remaining}, []storage.KeySpan{{Lower: point, Upper: point}}, c.request.KeyLayout, encoder, phase)
		if err != nil {
			return Continuation{}, err
		}
		if len(ordered) == 0 {
			return Continuation{}, ErrContinuationOrder
		}
	}
	if err := ctx.Err(); err != nil {
		return Continuation{}, err
	}
	c.accepted = key
	return c, nil
}

// Resume resolves the exclusive unacknowledged suffix against current placement.
// Returned morsels own their bounds and follow the request's traversal order.
func (c Continuation) Resume(ctx context.Context, identity ScanIdentity, request storage.PushdownRequest, table *routing.Table, encoder storage.KeyEncoder, limits routing.Limits) ([]routing.Morsel, error) {
	phase, err := continuationLimits(ctx, encoder, limits, true)
	if err != nil {
		return nil, err
	}
	if c.request == nil || identity != c.identity {
		return nil, ErrContinuationMismatch
	}
	if uint64(len(c.request.KeyLayout)) > phase.MaxNodes {
		return nil, routing.ErrLimit
	}
	if !table.Matches(c.request.Target, c.request.KeyLayout) {
		return nil, ErrContinuationMismatch
	}
	// Copy before hashing so malformed nested metadata is bounded before the
	// canonical hasher invokes request validation.
	owned, _, err := storage.CopyPushdownRequest(ctx, request, continuationMetadataBudget(phase)/2)
	if err != nil {
		return nil, err
	}
	fingerprint, err := continuationHash(ctx, identity, owned, phase)
	if err != nil {
		return nil, err
	}
	if fingerprint != c.fingerprint {
		return nil, ErrContinuationMismatch
	}
	spans := c.request.Spans
	if len(c.accepted) != 0 {
		tail := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
		boundary := storage.KeyBound{Values: c.accepted}
		if c.request.Direction == storage.ScanReverse {
			tail.Upper = boundary
		} else {
			tail.Lower = boundary
		}
		spans, err = routing.Intersect(ctx, spans, []storage.KeySpan{tail}, c.request.KeyLayout, encoder, phase)
		if err != nil {
			return nil, err
		}
	}
	morsels, err := table.Route(ctx, spans, phase)
	if err != nil {
		return nil, err
	}
	if c.request.Direction == storage.ScanReverse {
		slices.Reverse(morsels)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return morsels, nil
}

// Three disjoint grants cover cumulative operation allocations, not just peak
// live memory. Hashing additionally reserves 8 KiB for its fixed encoder/digest
// scratch. Metadata bytes conservatively charge one node and 128 work units per
// byte, covering bounded-depth copying, validation, and canonical serialization.
func continuationLimits(ctx context.Context, encoder storage.KeyEncoder, limits routing.Limits, hashing bool) (routing.Limits, error) {
	if ctx == nil || encoder == nil {
		return routing.Limits{}, routing.ErrInvalidSpec
	}
	if err := ctx.Err(); err != nil {
		return routing.Limits{}, err
	}
	fixed := uint64(unsafe.Sizeof(Continuation{})) + uint64(unsafe.Sizeof(storage.PushdownRequest{}))
	if hashing {
		fixed += 8192
	}
	limits.MaxBytes = min(limits.MaxBytes, uint64(math.MaxInt))
	insufficientWork := limits.MaxBytes <= fixed || limits.MaxNodes < 3 || limits.MaxWork < 3
	missingCounts := limits.MaxSpans == 0 || limits.MaxPartitions == 0 || limits.MaxMorsels == 0
	if insufficientWork || missingCounts {
		return routing.Limits{}, routing.ErrLimit
	}
	limits.MaxBytes = (limits.MaxBytes - fixed) / 3
	limits.MaxNodes /= 3
	limits.MaxWork /= 3
	if limits.MaxBytes == 0 {
		return routing.Limits{}, routing.ErrLimit
	}
	return limits, nil
}

func continuationMetadataBudget(limits routing.Limits) uint64 {
	return min(limits.MaxBytes, limits.MaxNodes, limits.MaxWork/128)
}

func continuationHash(ctx context.Context, identity ScanIdentity, request storage.PushdownRequest, limits routing.Limits) ([32]byte, error) {
	return storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request, SnapshotTS: identity.Snapshot}, continuationMetadataBudget(limits)/2)
}
