package model

import (
	"bytes"
	"context"
	"slices"
)

// ValidateSuccessor preserves historical decoding identities. New column IDs
// must exceed every ID previously allocated, even after dropped columns vanish
// from the visible column set. Rewrites publish a fresh physical generation.
func (d *Descriptor) ValidateSuccessor(ctx context.Context, next *Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || next == nil {
		return ErrInvalidDescriptor
	}
	old, new := &d.spec, &next.spec
	if old.ID != new.ID || old.OID != new.OID || old.Name.Kind != new.Name.Kind || old.Version == ^uint64(0) || new.Version != old.Version+1 {
		return ErrInvalidDescriptor
	}
	if new.Physical.Generation < old.Physical.Generation {
		return ErrInvalidDescriptor
	}
	sameGeneration := new.Physical.Generation == old.Physical.Generation
	if sameGeneration && new.Physical.EncodingVersion != old.Physical.EncodingVersion {
		return ErrInvalidDescriptor
	}
	if old.Table != nil {
		if new.Table.ColumnIDHighWatermark < old.Table.ColumnIDHighWatermark {
			return ErrInvalidDescriptor
		}
		if old.Table.RowType != 0 && old.Table.RowType != new.Table.RowType {
			return ErrInvalidDescriptor
		}
		previous := make(map[ColumnID]*ColumnDescriptor, len(old.Table.Columns))
		for i := range old.Table.Columns {
			column := &old.Table.Columns[i]
			previous[column.ID] = column
		}
		for _, column := range new.Table.Columns {
			if err := ctx.Err(); err != nil {
				return err
			}
			prior, exists := previous[column.ID]
			if !exists {
				if column.ID <= old.Table.ColumnIDHighWatermark {
					return ErrInvalidDescriptor
				}
				continue
			}
			if prior.SystemIdentity != column.SystemIdentity {
				return ErrInvalidDescriptor
			}
			if sameGeneration && !TypeLayoutsEqual(prior.Layout, column.Layout) {
				return ErrInvalidDescriptor
			}
		}
		if sameGeneration && !equalKeys(old.Table.PrimaryKey, new.Table.PrimaryKey) {
			return ErrInvalidDescriptor
		}
	}
	if old.CompositeRelation != nil {
		if new.CompositeRelation == nil || old.CompositeRelation.RowType != new.CompositeRelation.RowType || new.CompositeRelation.FieldIDHighWatermark < old.CompositeRelation.FieldIDHighWatermark {
			return ErrInvalidDescriptor
		}
		previous := make(map[ColumnID]struct{}, len(old.CompositeRelation.Columns))
		for _, field := range old.CompositeRelation.Columns {
			previous[field.ID] = struct{}{}
		}
		for _, field := range new.CompositeRelation.Columns {
			if _, exists := previous[field.ID]; !exists && field.ID <= old.CompositeRelation.FieldIDHighWatermark {
				return ErrInvalidDescriptor
			}
		}
	}
	if old.View != nil && old.View.RowType != 0 && (new.View == nil || old.View.RowType != new.View.RowType) {
		return ErrInvalidDescriptor
	}
	if old.Index != nil && sameGeneration {
		sameTable := old.Index.Table == new.Index.Table
		if !sameTable || !equalKeys(old.Index.Keys, new.Index.Keys) {
			return ErrInvalidDescriptor
		}
	}
	if old.Sequence != nil && old.Sequence.Identity != new.Sequence.Identity {
		return ErrInvalidDescriptor
	}
	if old.Sequence != nil && sameGeneration && old.Sequence.Type != new.Sequence.Type {
		return ErrInvalidDescriptor
	}
	if old.Type != nil {
		kind := old.Type.DeclaredKind()
		nextKind := TypeKind(0)
		if new.Type != nil {
			nextKind = new.Type.DeclaredKind()
		}
		shellCompletion := kind == TypeKindShell && nextKind == TypeKindRange && old.Type.Layout.Type == new.Type.Layout.Type
		if new.Type == nil || !shellCompletion && (kind != nextKind || old.Type.Layout.Type != new.Type.Layout.Type || old.Type.Array != new.Type.Array || old.Type.Relation != new.Type.Relation || old.Type.Base != new.Type.Base || kind != TypeKindArray && !TypeLayoutsEqual(old.Type.BaseLayout, new.Type.BaseLayout) || !equalRangeTypeMetadata(old.Type.Range, new.Type.Range) || new.Type.FieldIDHighWatermark < old.Type.FieldIDHighWatermark) {
			return ErrInvalidDescriptor
		}
		if kind == TypeKindEnum {
			position := 0
			for _, oid := range old.Type.ElementOIDs {
				next := slices.Index(new.Type.ElementOIDs[position:], oid)
				if next < 0 {
					return ErrInvalidDescriptor
				}
				position += next + 1
			}
		}
		previous := make(map[ColumnID]ColumnDescriptor, len(old.Type.Fields))
		for _, field := range old.Type.Fields {
			previous[field.ID] = field
		}
		for _, field := range new.Type.Fields {
			_, exists := previous[field.ID]
			if !exists {
				if field.ID <= old.Type.FieldIDHighWatermark {
					return ErrInvalidDescriptor
				}
				continue
			}
		}
	}
	return nil
}
func equalRangeTypeMetadata(a, b *RangeTypeMetadata) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.OperatorClass == b.OperatorClass && a.Collation == b.Collation && a.Canonical == b.Canonical && a.Difference == b.Difference && a.CanonicalFunction == b.CanonicalFunction && a.DifferenceFunction == b.DifferenceFunction && a.Range == b.Range && a.Multirange == b.Multirange && TypeLayoutsEqual(a.Subtype, b.Subtype)
}
func equalKeys(a, b []KeyElement) bool {
	if len(a) != len(b) {
		return false
	}
	for i, key := range a {
		other := b[i]
		sameIdentity := key.Column == other.Column && key.ExpressionName == other.ExpressionName && TypeLayoutsEqual(TypeLayout{Type: key.Type, Children: key.Children}, TypeLayout{Type: other.Type, Children: other.Children})
		sameOrdering := key.OperatorClass == other.OperatorClass && key.Collation == other.Collation && key.Descending == other.Descending && key.NullsFirst == other.NullsFirst
		if !sameIdentity || !sameOrdering || !equalDefinitions(key.Expression, other.Expression) {
			return false
		}
	}
	return true
}
func equalDefinitions(a, b *ExpressionDefinition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SQL == b.SQL && a.SemanticVersion == b.SemanticVersion && bytes.Equal(a.Encoding, b.Encoding) && slices.Equal(a.Dependencies, b.Dependencies)
}
