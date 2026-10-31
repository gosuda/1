package storage

import (
	"cmp"
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ObjectLayout is immutable schema data borrowed with a mutation batch. It
// preserves nested element types even when every stored value is NULL or empty.
// Layouts are ordered by target; columns are ordered by ColumnID.
type ObjectLayout struct {
	Target  ObjectRef
	Columns []ColumnLayout
}

func (l ObjectLayout) Validate(ctx context.Context) error {
	if err := l.Target.Validate(); err != nil {
		return err
	}
	return validateReadLayout(ctx, l.Columns, nil)
}

func compareTargets(a, b ObjectRef) int {
	if order := cmp.Compare(a.ObjectID, b.ObjectID); order != 0 {
		return order
	}
	if order := cmp.Compare(a.Generation, b.Generation); order != 0 {
		return order
	}
	if order := cmp.Compare(a.DescriptorVersion, b.DescriptorVersion); order != 0 {
		return order
	}
	return cmp.Compare(a.EncodingVersion, b.EncodingVersion)
}

// Layout returns a borrowed declaration from the batch's ordered layout set.
func (b MutationBatch) Layout(target ObjectRef) (ObjectLayout, bool) {
	lo, hi := 0, len(b.Layouts)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if compareTargets(b.Layouts[mid].Target, target) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(b.Layouts) || b.Layouts[lo].Target != target {
		return ObjectLayout{}, false
	}
	return b.Layouts[lo], true
}

func (b MutationBatch) validateLayouts(ctx context.Context) error {
	for i, l := range b.Layouts {
		if err := l.Validate(ctx); err != nil {
			return err
		}
		if i > 0 && compareTargets(b.Layouts[i-1].Target, l.Target) >= 0 {
			return ErrInvalidLayout
		}
	}
	return nil
}

func (b MutationBatch) validateRowLayout(ctx context.Context, m Mutation) error {
	layout, ok := b.Layout(m.Target)
	if !ok {
		return ErrInvalidLayout
	}
	if m.Op != MutationUpsert {
		return nil
	}
	if len(m.ColumnIDs) != len(layout.Columns) || len(m.Values) != len(layout.Columns) {
		return ErrUnnormalizedBatch
	}
	for i, column := range layout.Columns {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.ColumnIDs[i] != column.ID || (m.Values[i].IsNull && !column.Nullable) {
			return ErrUnnormalizedBatch
		}
		if err := ValidateStoredValue(ctx, m.Values[i], model.TypeLayout{Type: column.Type, Children: column.Children}); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStoredValue checks datum shape against an already validated layout.
// Column nullability is enforced by the row's owner.
func ValidateStoredValue(ctx context.Context, v model.Value, layout model.TypeLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.Type != layout.Type.Storage {
		return ErrUnnormalizedBatch
	}
	if v.IsNull {
		return nil
	}
	switch v.Type {
	case model.TypeArray:
		if err := model.ValidateArrayShape(v.Dimensions, uint64(len(v.ArrayVal))); err != nil {
			return err
		}
		for _, element := range v.ArrayVal {
			if err := ValidateStoredValue(ctx, element, layout.Children[0]); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(v.ArrayVal) != len(layout.Children) {
			return ErrUnnormalizedBatch
		}
		for i, field := range v.ArrayVal {
			if err := ValidateStoredValue(ctx, field, layout.Children[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *mutationEncoder) objectLayout(layout ObjectLayout) {
	e.number(uint64(layout.Target.ObjectID), 8)
	e.number(layout.Target.Generation, 8)
	e.number(layout.Target.DescriptorVersion, 8)
	e.number(uint64(layout.Target.EncodingVersion), 4)
	e.number(uint64(len(layout.Columns)), 8)
	for _, column := range layout.Columns {
		if e.err != nil {
			return
		}
		e.number(uint64(column.ID), 4)
		e.typeLayout(model.TypeLayout{Type: column.Type, Children: column.Children})
		var flags uint64
		if column.Nullable {
			flags |= 1
		}
		if column.HasMissingValue {
			flags |= 2
		}
		e.number(flags, 1)
		if column.HasMissingValue {
			e.value(column.MissingValue, 0)
		}
	}
}

func (e *mutationEncoder) typeLayout(layout model.TypeLayout) {
	e.number(uint64(layout.Type.ID), 4)
	e.number(uint64(layout.Type.Storage), 1)
	e.number(uint64(uint32(layout.Type.Modifier)), 4)
	e.number(uint64(layout.Type.Collation), 4)
	e.number(uint64(len(layout.Children)), 8)
	for _, child := range layout.Children {
		if e.err != nil {
			return
		}
		e.typeLayout(child)
	}
}
