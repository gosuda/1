package neosepia

import (
	"bytes"
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// A cut lies before or after every extension of its prefix. This distinguishes
// exclusive prefix bounds without computing a byte-string successor.
type keyCut struct {
	prefix   []byte
	infinity int8
	after    bool
}

type encodedSpan struct{ lower, upper keyCut }

func compareCuts(a, b keyCut) int {
	if a.infinity != 0 || b.infinity != 0 {
		if a.infinity < b.infinity {
			return -1
		}
		if a.infinity > b.infinity {
			return 1
		}
		return 0
	}
	if bytes.Equal(a.prefix, b.prefix) {
		if a.after == b.after {
			return 0
		}
		if a.after {
			return 1
		}
		return -1
	}
	if bytes.HasPrefix(b.prefix, a.prefix) {
		if a.after {
			return 1
		}
		return -1
	}
	if bytes.HasPrefix(a.prefix, b.prefix) {
		if b.after {
			return -1
		}
		return 1
	}
	return bytes.Compare(a.prefix, b.prefix)
}

func compileSpans(ctx context.Context, request storage.PushdownRequest, encoder storage.KeyEncoder, maxBytes uint64) ([]encodedSpan, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if len(request.EncodedSpans) != 0 {
		return compileEncodedSpans(ctx, request.EncodedSpans, maxBytes)
	}
	unit := uint64(unsafe.Sizeof(encodedSpan{}))
	if uint64(len(request.Spans)) > maxBytes/unit {
		return nil, 0, columnar.ErrBatchBudget
	}
	used := uint64(len(request.Spans)) * unit
	spans := make([]encodedSpan, len(request.Spans))
	for i, span := range request.Spans {
		lower, bytes, err := encodeCut(ctx, span.Lower, true, request.KeyLayout, encoder, maxBytes-used)
		if err != nil {
			return nil, 0, err
		}
		used += bytes
		upper, bytes, err := encodeCut(ctx, span.Upper, false, request.KeyLayout, encoder, maxBytes-used)
		if err != nil {
			return nil, 0, err
		}
		used += bytes
		if compareCuts(lower, upper) >= 0 {
			return nil, 0, storage.ErrRangeInvalid
		}
		if i > 0 && compareCuts(spans[i-1].upper, lower) > 0 {
			return nil, 0, storage.ErrRangeInvalid
		}
		spans[i] = encodedSpan{lower: lower, upper: upper}
	}
	return spans, used, nil
}

func compileEncodedSpans(ctx context.Context, source []storage.EncodedKeySpan, maxBytes uint64) ([]encodedSpan, uint64, error) {
	unit := uint64(unsafe.Sizeof(encodedSpan{}))
	if uint64(len(source)) > maxBytes/unit {
		return nil, 0, columnar.ErrBatchBudget
	}
	spans := make([]encodedSpan, len(source))
	for i, span := range source {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		lower := encodedBoundCut(span.Lower, true)
		upper := encodedBoundCut(span.Upper, false)
		if compareCuts(lower, upper) >= 0 || i > 0 && compareCuts(spans[i-1].upper, lower) > 0 {
			return nil, 0, storage.ErrRangeInvalid
		}
		spans[i] = encodedSpan{lower: lower, upper: upper}
	}
	return spans, uint64(len(spans)) * unit, nil
}

func encodedBoundCut(bound storage.EncodedKeyBound, lower bool) keyCut {
	if bound.Unbounded {
		if lower {
			return keyCut{infinity: -1}
		}
		return keyCut{infinity: 1}
	}
	after := bound.Inclusive
	if lower {
		after = !after
	}
	return keyCut{prefix: bound.Key, after: after}
}

func encodeCut(ctx context.Context, bound storage.KeyBound, lower bool, layout []storage.KeyColumn, encoder storage.KeyEncoder, maxBytes uint64) (keyCut, uint64, error) {
	if err := ctx.Err(); err != nil {
		return keyCut{}, 0, err
	}
	if bound.Unbounded {
		if lower {
			return keyCut{infinity: -1}, 0, nil
		}
		return keyCut{infinity: 1}, 0, nil
	}
	if encoder == nil {
		return keyCut{}, 0, storage.ErrInvalidScanPlan
	}
	prefix, err := encoder.EncodePrefix(ctx, layout, bound.Values, maxBytes)
	if err != nil {
		return keyCut{}, 0, err
	}
	if uint64(len(prefix)) > maxBytes {
		return keyCut{}, 0, columnar.ErrBatchBudget
	}
	if len(prefix) == 0 {
		return keyCut{}, 0, storage.ErrInvalidScanPlan
	}
	after := bound.Inclusive
	if lower {
		after = !after
	}
	return keyCut{prefix: prefix, after: after}, uint64(len(prefix)), nil
}

func (s encodedSpan) contains(key []byte) bool {
	before := keyCut{prefix: key}
	after := keyCut{prefix: key, after: true}
	return compareCuts(s.lower, before) <= 0 && compareCuts(after, s.upper) <= 0
}
