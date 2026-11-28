package builtin

import (
	"context"
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type keyPreparation struct {
	numerics    []numericKey
	jsonb       [][]byte
	nextNumeric int
	nextJSONB   int
}

func nestedKeyType(typ model.Type) bool {
	return typ.Storage == model.TypeArray || typ.Storage == model.TypeComposite
}

func (e *KeyEncoder) keyLayout(layout model.TypeLayout) (model.TypeLayout, *ResolvedType) {
	for depth := 0; depth <= 64 && e != nil; depth++ {
		if e.types != nil {
			if resolved, known := e.types.ResolveType(layout.Type.ID); known {
				if resolved.Kind() != model.TypeKindDomain {
					return layout, resolved
				}
				layout = resolved.BaseLayout()
				continue
			}
		}
		if e.informationSchemaTypes != nil {
			if binding, known := e.informationSchemaTypes.ResolveInformationSchemaType(layout.Type.ID); known {
				if binding.Kind != model.TypeKindDomain {
					return layout, nil
				}
				if !sameArrayConstructionLayout(layout, binding.Layout) {
					return model.TypeLayout{}, nil
				}
				layout = binding.BaseLayout
				continue
			}
		}
		return layout, nil
	}
	return model.TypeLayout{}, nil
}

func (e *KeyEncoder) rangeFamily(id model.TypeID) (RangeFamilyInfo, bool) {
	var types TypeResolver
	if e != nil {
		types = e.types
	}
	return ResolveRangeFamily(ResolveOptions{Types: types}, id)
}

func (e *KeyEncoder) validateKeyLayout(ctx context.Context, layout model.TypeLayout, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	layout, declared := e.keyLayout(layout)
	if depth > 64 || layout.Type.ID == 0 || !layout.Type.Storage.Valid() {
		return model.ErrInvalidTypeLayout
	}
	if declared != nil && declared.Kind() != model.TypeKindDomain && !sameArrayConstructionLayout(layout, declared.Layout()) {
		return model.ErrInvalidTypeLayout
	}
	if !nestedKeyType(layout.Type) {
		if len(layout.Children) != 0 {
			return model.ErrInvalidTypeLayout
		}
		if declared != nil && declared.Kind() == model.TypeKindEnum {
			if layout.Type.Storage != model.TypeInt64 || layout.Type.Collation != 0 {
				return model.ErrInvalidTypeLayout
			}
			return nil
		}
		if _, known := e.rangeFamily(layout.Type.ID); known {
			if layout.Type.Storage != model.TypeBytes {
				return model.ErrInvalidTypeLayout
			}
			return nil
		}
		return e.validateKeyType(layout.Type)
	}
	if layout.Type.Storage == model.TypeArray {
		if _, _, err := arrayCodecLayout(layout, ResolveOptions{Types: e.types, InformationSchemaTypes: e.informationSchemaTypes}); err != nil {
			return err
		}
		if err := validateCatalogVectorLayout(layout); err != nil {
			return err
		}
		if layout.Type.Collation != 0 && layout.Type.Collation != 100 && layout.Type.Collation != 950 && layout.Type.Collation != 951 {
			return sqlError("42P22", "array SQL key requires a resolved C or POSIX collation")
		}
	} else {
		if layout.Type.Collation != 0 {
			return sqlError("42804", "composite SQL key has a collation")
		}
		if info, known := LookupType(layout.Type.ID); known && info.Kind != 'c' && layout.Type.ID != 2249 {
			return sqlError("42804", "composite SQL key has a non-composite identity")
		}
	}
	for _, child := range layout.Children {
		if err := e.validateKeyLayout(ctx, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (e *KeyEncoder) measureNestedKeyField(ctx context.Context, layout model.TypeLayout, value *model.Value, maximum uint64, depth int, prepared *keyPreparation) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	layout, _ = e.keyLayout(layout)
	if depth > 64 || value.Type != layout.Type.Storage {
		return 0, sqlError("42804", "nested SQL key value differs from its declared layout")
	}
	if maximum == 0 {
		return 0, columnar.ErrBatchBudget
	}
	if value.IsNull {
		return 1, nil
	}
	if !nestedKeyType(layout.Type) {
		return e.measureScalar(ctx, layout.Type, value, maximum, prepared)
	}
	if layout.Type.ID == 30 {
		if err := validateCatalogVector(ctx, *value, false, catalogVectorShape); err != nil {
			return 0, err
		}
		size := uint64(5) + uint64(len(value.ArrayVal))*9
		if size > maximum {
			return 0, columnar.ErrBatchBudget
		}
		return size, nil
	}
	size := uint64(1)
	if layout.Type.Storage == model.TypeArray {
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return 0, err
		}
		size += 5 + 8*uint64(len(value.Dimensions))
	} else if len(value.ArrayVal) != len(layout.Children) {
		return 0, sqlError("42804", "composite SQL key field count differs from its declaration")
	}
	if size > maximum {
		return 0, columnar.ErrBatchBudget
	}
	for i := range value.ArrayVal {
		child := 0
		if layout.Type.Storage == model.TypeComposite {
			child = i
		}
		bytes, err := e.measureNestedKeyField(ctx, layout.Children[child], &value.ArrayVal[i], maximum-size, depth+1, prepared)
		if err != nil {
			return 0, err
		}
		size += bytes
	}
	return size, nil
}

// Nested fields always sort NULL above a non-NULL value. The enclosing key's
// direction complements both their markers and payloads, unlike top-level NULLS.
func (e *KeyEncoder) appendNestedKeyField(ctx context.Context, key []byte, layout model.TypeLayout, value *model.Value, mask byte, depth int, prepared *keyPreparation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	layout, _ = e.keyLayout(layout)
	if depth > 64 {
		return nil, model.ErrInvalidTypeLayout
	}
	if value.IsNull {
		return append(key, 2^mask), nil
	}
	key = append(key, 1^mask)
	return e.appendNestedKeyValue(ctx, key, layout, value, mask, depth, prepared)
}

func (e *KeyEncoder) appendNestedKeyValue(ctx context.Context, key []byte, layout model.TypeLayout, value *model.Value, mask byte, depth int, prepared *keyPreparation) ([]byte, error) {
	layout, declared := e.keyLayout(layout)
	if family, known := e.rangeFamily(layout.Type.ID); known {
		return appendRangeKey(ctx, key, family, layout.Type.ID, *value, mask)
	}
	if !nestedKeyType(layout.Type) {
		if layout.Type.ID == 1700 {
			numeric := prepared.numerics[prepared.nextNumeric]
			prepared.nextNumeric++
			return appendNumericKey(ctx, key, numeric, mask != 0)
		}
		if layout.Type.ID == 3802 {
			payload := prepared.jsonb[prepared.nextJSONB]
			prepared.nextJSONB++
			return appendPreparedJSONBKey(ctx, key, payload, mask)
		}
		if declared != nil && declared.Kind() == model.TypeKindEnum {
			order, err := declared.enumOrder(*value)
			if err != nil {
				return nil, err
			}
			ranked := model.Int64Val(int64(order))
			column := storage.KeyColumn{Type: model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}, Descending: mask != 0}
			return appendKeyValue(ctx, key, &column, &ranked)
		}
		column := storage.KeyColumn{Type: layout.Type, Children: layout.Children, Descending: mask != 0}
		return appendKeyValue(ctx, key, &column, value)
	}
	if layout.Type.ID == 30 {
		key = appendNestedKeyNumber(key, uint32(len(value.ArrayVal)), mask)
	}
	for i := range value.ArrayVal {
		child := 0
		if layout.Type.Storage == model.TypeComposite {
			child = i
		}
		var err error
		key, err = e.appendNestedKeyField(ctx, key, layout.Children[child], &value.ArrayVal[i], mask, depth+1, prepared)
		if err != nil {
			return nil, err
		}
	}
	if layout.Type.Storage == model.TypeComposite || layout.Type.ID == 30 {
		return key, nil
	}
	// The terminator orders shorter equal prefixes first. Dimensional metadata
	// follows all elements, matching PostgreSQL's final comparison tie-breaks.
	key = append(key, mask)
	key = appendNestedKeyNumber(key, uint32(len(value.Dimensions)), mask)
	for _, dimension := range value.Dimensions {
		key = appendNestedKeyNumber(key, uint32(dimension.Length), mask)
	}
	for _, dimension := range value.Dimensions {
		key = appendNestedKeyNumber(key, uint32(dimension.LowerBound)^(uint32(1)<<31), mask)
	}
	return key, ctx.Err()
}

func appendNestedKeyNumber(key []byte, value uint32, mask byte) []byte {
	if mask != 0 {
		value = ^value
	}
	return binary.BigEndian.AppendUint32(key, value)
}

func (e *KeyEncoder) measureKeyColumn(ctx context.Context, column *storage.KeyColumn, value *model.Value, maximum uint64, prepared *keyPreparation) (uint64, error) {
	if nestedKeyType(column.Type) {
		return e.measureNestedKeyField(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, value, maximum, 0, prepared)
	}
	return e.measureScalar(ctx, column.Type, value, maximum, prepared)
}

func (e *KeyEncoder) measureScalar(ctx context.Context, typ model.Type, value *model.Value, maximum uint64, prepared *keyPreparation) (uint64, error) {
	layout, declared := e.keyLayout(model.TypeLayout{Type: typ})
	typ = layout.Type
	if value.Type != typ.Storage || value.IsNull {
		return keyFieldSize(ctx, typ, value, maximum)
	}
	if family, known := e.rangeFamily(typ.ID); known {
		return rangeKeyFieldSize(ctx, family, typ.ID, *value, maximum)
	}
	if declared != nil && declared.Kind() == model.TypeKindEnum {
		if _, err := declared.enumOrder(*value); err != nil {
			return 0, err
		}
		if maximum < 9 {
			return 0, columnar.ErrBatchBudget
		}
		return 9, nil
	}
	if typ.ID == 1700 {
		numeric, size, err := prepareNumericKey(ctx, value.StrVal, maximum)
		if err != nil {
			return 0, err
		}
		prepared.numerics = append(prepared.numerics, numeric)
		return size, nil
	}
	if typ.ID == 3802 {
		if maximum <= 1 {
			return 0, columnar.ErrBatchBudget
		}
		payload, err := prepareJSONBKey(ctx, value.StrVal, maximum-1)
		if err != nil {
			return 0, err
		}
		prepared.jsonb = append(prepared.jsonb, payload)
		return uint64(len(payload)) + 1, nil
	}
	return keyFieldSize(ctx, typ, value, maximum)
}
