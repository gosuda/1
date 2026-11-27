package builtin

import (
	"context"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// featureIndexKeyOrdering names the embedded_pg18 profile exclusion the key
// encoder raises for operator classes without an equivalent SQL key
// ordering, equality, or key representation. The bind package owns the
// canonical FeatureID (bind.IndexKeyOrdering) but cannot be imported here:
// bind imports builtin. gates_test.go's manifest-driven exclusion table
// fails if this id drifts from testdata/postgres/embedded_features.json.
const featureIndexKeyOrdering = "index_key_ordering"

var _ storage.IndexKeyEncoder = (*KeyEncoder)(nil)

// ValidateOperatorClassInput checks B-tree input compatibility, not key codec
// support. PostgreSQL binary coercibility requires a direct implicit binary cast;
// matching storage kinds or assignment casts are insufficient.
func ValidateOperatorClassInput(typ model.Type, classID model.OID) error {
	_, err := lookupKeyOperatorClass(typ, classID)
	return err
}

// BinaryCoercibleTypes reports whether two SQL types share one key
// representation: identical types, or a direct implicit binary cast ('b'
// method) in either direction. This mirrors the binder's Relabel criterion
// and the operator class input rule above: matching storage kinds or
// assignment casts are insufficient. A slot read as a binary-coercible type
// yields byte-identical values, so key, ordering and distribution checks may
// name the coerced type for a slot carrying the original one.
func BinaryCoercibleTypes(a, b model.Type) bool {
	if a == b {
		return true
	}
	if a.Storage != b.Storage || a.Collation != b.Collation {
		return false
	}
	if a.ID == b.ID {
		return true
	}
	if cast, ok := LookupCast(a.ID, b.ID); ok && cast.Context == 'i' && cast.Method == 'b' {
		return true
	}
	if cast, ok := LookupCast(b.ID, a.ID); ok && cast.Context == 'i' && cast.Method == 'b' {
		return true
	}
	return false
}

// BinaryCoercibleKeyLayouts reports whether two key column layouts share one
// hash/merge key representation: identical layouts, or binary-coercible types
// applied recursively to children. The planner emits cross-type join keys
// only for such pairs, and the executor encodes both sides with the left
// layout, so byte-identical values hash identically and the residual rechecks
// every candidate pair.
func BinaryCoercibleKeyLayouts(a, b model.TypeLayout) bool {
	if model.TypeLayoutsEqual(a, b) {
		return true
	}
	if len(a.Children) != len(b.Children) || !BinaryCoercibleTypes(a.Type, b.Type) {
		return false
	}
	for i := range a.Children {
		if !BinaryCoercibleKeyLayouts(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

func lookupKeyOperatorClass(typ model.Type, classID model.OID) (OperatorClassInfo, error) {
	class, ok := LookupOperatorClass(classID)
	if !ok {
		return OperatorClassInfo{}, sqlError("42704", fmt.Sprintf("operator class %d does not exist", classID))
	}
	if class.Method != "btree" {
		return OperatorClassInfo{}, sqlError("0A000", fmt.Sprintf("operator class %q does not use B-tree", class.Name))
	}
	if rangeKeyGenericInput(typ, class.InputType) {
		return class, nil
	}
	if typ.ID == class.InputType {
		return class, nil
	}
	if class.InputType == 2277 && typ.Storage == model.TypeArray {
		return class, nil
	}
	if class.InputType == 2249 && typ.Storage == model.TypeComposite {
		return class, nil
	}
	cast, ok := LookupCast(typ.ID, class.InputType)
	if ok && cast.Context == 'i' && cast.Method == 'b' {
		return class, nil
	}
	return OperatorClassInfo{}, sqlError("42804", fmt.Sprintf("operator class %q does not accept data type %d", class.Name, typ.ID))
}

func (e *KeyEncoder) operatorClassType(typ model.Type) model.Type {
	layout, declared := e.keyLayout(model.TypeLayout{Type: typ})
	typ = layout.Type
	if declared != nil {
		switch declared.Kind() {
		case model.TypeKindEnum:
			typ.ID = 3500
			return typ
		case model.TypeKindComposite:
			typ.ID = 2249
			return typ
		case model.TypeKindArray:
			typ.ID = 2277
			return typ
		}
	}
	if e != nil && e.informationSchemaTypes != nil {
		if binding, known := e.informationSchemaTypes.ResolveInformationSchemaType(typ.ID); known && binding.Kind == model.TypeKindArray {
			typ.ID = 2277
			return typ
		}
	}
	if family, known := e.rangeFamily(typ.ID); known {
		if typ.ID == family.Range {
			typ.ID = 3831
		} else {
			typ.ID = 4537
		}
	}
	return typ
}

func (e *KeyEncoder) keyOrderingEquivalent(source model.Type, input model.TypeID) bool {
	return KeyOrderingEquivalent(e.operatorClassType(source), input)
}

// OrderingEquivalent reports whether EncodePrefix's ordering and equality for
// source match the operator class input, unwrapping declared types the codec
// resolves. Codec availability and collation are certified separately.
func (e *KeyEncoder) OrderingEquivalent(source model.Type, input model.TypeID) bool {
	return e.keyOrderingEquivalent(source, input)
}

// ValidateOperatorClass certifies the existing EncodePrefix equality and ordering
// for typ without changing its key representation. It retains no catalog state.
func (e *KeyEncoder) ValidateOperatorClass(ctx context.Context, layout model.TypeLayout, classID model.OID) error {
	typ := layout.Type
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return storage.ErrInvalidScanPlan
	}
	class, err := lookupKeyOperatorClass(e.operatorClassType(typ), classID)
	if err != nil {
		return err
	}
	if !keyOperatorClassFamilyEquivalent(class) {
		return &model.Error{Code: "0A000", Message: fmt.Sprintf("operator class %q has no equivalent SQL key ordering and equality", class.Name), FeatureID: featureNonBtreeIndexes}
	}
	if err := e.validateKeyLayout(ctx, layout, 0); err != nil {
		return fmt.Errorf("operator class %q: %w", class.Name, err)
	}
	if !e.keyOrderingEquivalent(typ, class.InputType) {
		return &model.Error{Code: "0A000", Message: fmt.Sprintf("operator class %q requires different SQL key ordering or equality for data type %d", class.Name, typ.ID), FeatureID: featureNonBtreeIndexes}
	}
	if class.KeyType != 0 && class.KeyType != class.InputType {
		return &model.Error{Code: "0A000", Message: fmt.Sprintf("operator class %q requires a different SQL key representation", class.Name), FeatureID: featureNonBtreeIndexes}
	}
	return ctx.Err()
}

func keyOperatorClassFamilyEquivalent(class OperatorClassInfo) bool {
	standard, ok := LookupDefaultOperatorClass("btree", class.InputType)
	if ok && class.Family == standard.Family {
		return true
	}
	// REL_18_4 varlena.c and varchar.c use memcmp followed by length for
	// pattern comparison, just like their C/POSIX comparators. Both bpchar
	// comparators trim trailing U+0020 through bcTruelen, as keyTextLength does.
	// validateKeyType must still certify the encoder's effective collation.
	switch class.Name {
	case "text_pattern_ops", "varchar_pattern_ops", "bpchar_pattern_ops":
		return true
	default:
		return false
	}
}

// KeyOrderingEquivalent reports identical canonical SQL key equality and order.
// Codec availability and the operator's collation are checked separately.
func KeyOrderingEquivalent(source model.Type, input model.TypeID) bool {
	if rangeKeyGenericInput(source, input) {
		return true
	}
	if source.ID == input {
		return true
	}
	if input == 2277 && source.Storage == model.TypeArray {
		return source.ID != 30
	}
	if input == 2249 && source.Storage == model.TypeComposite {
		return true
	}
	// Binary-compatible does not imply equal ordering: int4 -> oid changes
	// signedness, and text/varchar -> bpchar discards trailing spaces.
	switch input {
	case 25:
		return source.ID == 1043
	case 26:
		return IsOIDType(source.ID)
	case 869:
		return source.ID == 650
	case 1560, 1562:
		return source.ID == 1560 || source.ID == 1562
	default:
		return false
	}
}
