package storage

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrInvalidLayout     = errors.New("invalid bound storage column layout")
	ErrInvalidProjection = errors.New("projection references an unbound column")
)

// Validate checks fence shape, not provenance. OpenSnapshot also verifies the
// fence against the published log position under the snapshot/GC mutex.
func (s SnapshotSpec) Validate() error {
	if s.Timestamp <= 0 || s.Fence.ClosedTS <= 0 {
		return ErrSnapshotNotClosed
	}
	if s.Timestamp > s.Fence.ClosedTS {
		return ErrSnapshotNotClosed
	}
	if s.Fence.ID == ([16]byte{}) {
		return ErrSnapshotNotClosed
	}
	return nil
}

// Layout is strictly ordered by ColumnID. Projection retains caller order and
// may repeat IDs without decoding the physical column more than once.
func (r ReadRequest) Validate(ctx context.Context) error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	return validateReadLayout(ctx, r.Layout, r.ColumnIDs)
}

// Validate checks representation. The backend's key codec must additionally
// prove that the typed spans are disjoint and obey the bound SQL ordering.
// Predicate compilation validates only reachable columns/operator signatures.
func (r PushdownRequest) Validate(ctx context.Context) error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if err := validateReadLayout(ctx, r.Layout, r.ColumnIDs); err != nil {
		return err
	}
	if r.Direction != ScanForward && r.Direction != ScanReverse {
		return ErrRangeInvalid
	}
	if !r.SafeLimit.Enabled && r.SafeLimit.Count != 0 {
		return ErrInvalidScanPlan
	}
	for _, key := range r.KeyLayout {
		if err := ctx.Err(); err != nil {
			return err
		}
		column, ok := lookupColumn(r.Layout, key.ColumnID)
		if !ok || !physicalTypeLayoutsEqual(model.TypeLayout{Type: r.Layout[column].Type, Children: r.Layout[column].Children}, model.TypeLayout{Type: key.Type, Children: key.Children}, 0) {
			return ErrInvalidLayout
		}
	}
	if len(r.Spans) != 0 && len(r.EncodedSpans) != 0 {
		return ErrRangeInvalid
	}
	for _, span := range r.Spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateKeyBound(ctx, span.Lower, r.KeyLayout); err != nil {
			return err
		}
		if err := validateKeyBound(ctx, span.Upper, r.KeyLayout); err != nil {
			return err
		}
	}
	for _, span := range r.EncodedSpans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validEncodedKeyBound(span.Lower) || !validEncodedKeyBound(span.Upper) {
			return ErrRangeInvalid
		}
	}
	return nil
}

func validEncodedKeyBound(bound EncodedKeyBound) bool {
	if bound.Unbounded {
		return len(bound.Key) == 0 && !bound.Inclusive && !bound.Prefix
	}
	return len(bound.Key) != 0
}

func validateReadLayout(ctx context.Context, layout []ColumnLayout, projection []model.ColumnID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, column := range layout {
		if err := ctx.Err(); err != nil {
			return err
		}
		if column.ID == 0 {
			return ErrInvalidLayout
		}
		if i > 0 && layout[i-1].ID >= column.ID {
			return ErrInvalidLayout
		}
		typeLayout := model.TypeLayout{Type: column.Type, Children: column.Children}
		if err := typeLayout.Validate(); err != nil {
			return errors.Join(ErrInvalidLayout, err)
		}
		if column.HasMissingValue {
			if column.MissingValue.Type != column.Type.Storage {
				return ErrInvalidLayout
			}
			if column.MissingValue.IsNull && !column.Nullable {
				return ErrInvalidLayout
			}
			if err := ValidateStoredValue(ctx, column.MissingValue, typeLayout); err != nil {
				return errors.Join(ErrInvalidLayout, err)
			}
		}
	}
	for _, id := range projection {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := lookupColumn(layout, id); !ok {
			return ErrInvalidProjection
		}
	}
	return nil
}

func lookupColumn(layout []ColumnLayout, id model.ColumnID) (int, bool) {
	lo, hi := 0, len(layout)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if layout[mid].ID < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(layout) && layout[lo].ID == id
}

func validateKeyBound(ctx context.Context, bound KeyBound, layout []KeyColumn) error {
	if bound.Unbounded {
		if len(bound.Values) != 0 || bound.Inclusive {
			return ErrRangeInvalid
		}
		return nil
	}
	if len(bound.Values) == 0 || len(bound.Values) > len(layout) {
		return ErrRangeInvalid
	}
	for i, value := range bound.Values {
		if value.Type != layout[i].Type.Storage {
			return ErrRangeInvalid
		}
		if value.Type == model.TypeBoolean && !value.IsNull && value.IntVal != 0 && value.IntVal != 1 {
			return ErrRangeInvalid
		}
		if err := ValidateStoredValue(ctx, value, model.TypeLayout{Type: layout[i].Type, Children: layout[i].Children}); err != nil {
			return errors.Join(ErrRangeInvalid, err)
		}
	}
	return nil
}
