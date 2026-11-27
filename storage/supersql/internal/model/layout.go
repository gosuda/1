package model

import (
	"errors"
	"strings"
)

// TypeLayout supplies resolved identities for nested physical values. Arrays
// have one child; composite children follow descriptor attribute order.
// Layouts are immutable and owned by the pinned descriptor snapshot.
type TypeLayout struct {
	Type     Type
	Children []TypeLayout
	// FieldNames is composite SQL presentation metadata parallel to Children.
	// Binding restores it from pinned descriptors; durable layouts omit it.
	FieldNames []string `json:"-"`
}

var ErrInvalidTypeLayout = errors.New("invalid physical type layout")

// TypeLayoutsEqual reports whether two layouts resolve the same physical
// identities: the same Type at every node and the same children in the same
// order. FieldNames is presentation metadata restored by binding, so durable
// layouts omit it and it is deliberately not compared.
func TypeLayoutsEqual(a, b TypeLayout) bool {
	if a.Type != b.Type || len(a.Children) != len(b.Children) {
		return false
	}
	for i := range a.Children {
		if !TypeLayoutsEqual(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

// IsPseudoTypeID reports whether id names a PostgreSQL pseudo-type that a
// declaration may carry as an unresolved type identity: the polymorphic
// family, `internal`, and the routine result/handler pseudotypes (void,
// trigger, event_trigger, *_handler, pg_ddl_command). Every ID here was
// confirmed against the pinned PostgreSQL 18.4 type catalog (the generated
// TypeInfo table, whose typtype is 'p'); the set is recorded once, in model,
// so binding and descriptor validation cannot drift apart.
//
// A pseudo-type has no storage of its own: the catalog gives these typlen = -1
// and typtype = 'p'. A layout naming one is therefore a DECLARED marker rather
// than a runtime value. HARD INVARIANT: a marker must never reach execution,
// storage, a key layout, or a PAX/encoding path. Pseudo-types are resolved to
// concrete layouts at the point the actual argument layouts are known; a marker
// that escapes that point is a defect, not a fallback.
func IsPseudoTypeID(id TypeID) bool {
	switch id {
	case 2281, // internal
		2276, // any
		2277, // anyarray
		2283, // anyelement
		2776, // anynonarray
		3500, // anyenum
		3831, // anyrange
		4537, // anymultirange
		4538, // anycompatiblemultirange
		5077, // anycompatible
		5078, // anycompatiblearray
		5079, // anycompatiblenonarray
		5080, // anycompatiblerange
		32,   // pg_ddl_command
		269,  // table_am_handler
		325,  // index_am_handler
		2278, // void
		2279, // trigger
		2280, // language_handler
		3115, // fdw_handler
		3310, // tsm_handler
		3838: // event_trigger
		return true
	}
	return false
}

// IsRoutineParameterPseudoTypeID narrows IsPseudoTypeID to the identities a
// routine parameter may declare: the polymorphic family plus `internal`, which
// descriptor validation further restricts to internal-language routines. The
// result and handler identities (void, trigger, event_trigger, *_handler,
// pg_ddl_command) are legal in RETURNS position only.
func IsRoutineParameterPseudoTypeID(id TypeID) bool {
	switch id {
	case 2281, // internal
		2277, // anyarray
		2283, // anyelement
		2776, // anynonarray
		3500, // anyenum
		3831, // anyrange
		4537, // anymultirange
		4538, // anycompatiblemultirange
		5077, // anycompatible
		5078, // anycompatiblearray
		5079, // anycompatiblenonarray
		5080: // anycompatiblerange
		return true
	}
	return false
}

func (l TypeLayout) Validate() error { return l.validate(0) }

// ValidateDeclaredLayout validates a layout recorded in DECLARATION metadata,
// where a pseudo-type identity is a legal childless leaf. It admits exactly
// that marker and validates everything else by the ordinary node rules, so no
// existing validation path is weakened and no new storage tag is introduced.
// The zero-value hazard stays closed: an unset storage on a NON-pseudo-type is
// still invalid, so a layout like {ID: 25, Storage: 0} remains rejected.
func (l TypeLayout) ValidateDeclaredLayout() error { return l.validateDeclared(0) }

func (l TypeLayout) validateDeclared(depth int) error {
	if IsPseudoTypeID(l.Type.ID) {
		return l.validateDeclaredMarker()
	}
	if err := l.validateNode(depth); err != nil {
		return err
	}
	for _, name := range l.FieldNames {
		if strings.IndexByte(name, 0) >= 0 {
			return ErrInvalidTypeLayout
		}
	}
	for _, child := range l.Children {
		if err := child.validateDeclared(depth + 1); err != nil {
			return err
		}
	}
	return nil
}

// validateDeclaredMarker accepts the representations a declared pseudo-type
// identity may carry: the open marker, which has no storage of its own, and
// `internal`'s bounded byte payload, which is the established state
// representation for that one pseudo-type. Any other pseudo-type paired with
// concrete storage is rejected, so a marker cannot be spelled a second way, and
// a pseudo-type never acquires a storage class of its own.
func (l TypeLayout) validateDeclaredMarker() error {
	if len(l.Children) != 0 || len(l.FieldNames) != 0 || l.Type.Modifier != -1 {
		return ErrInvalidTypeLayout
	}
	if l.Type.Storage == TypeAny || l.Type.ID == 2281 && l.Type.Storage == TypeBytes {
		return nil
	}
	return ErrInvalidTypeLayout
}

func (l TypeLayout) validate(depth int) error {
	if err := l.validateNode(depth); err != nil {
		return err
	}
	for _, name := range l.FieldNames {
		if strings.IndexByte(name, 0) >= 0 {
			return ErrInvalidTypeLayout
		}
	}
	for _, child := range l.Children {
		if err := child.validate(depth + 1); err != nil {
			return err
		}
	}
	return nil
}

func (l TypeLayout) validateNode(depth int) error {
	if depth > 64 || l.Type.ID == 0 || !l.Type.Storage.Valid() {
		return ErrInvalidTypeLayout
	}
	if len(l.FieldNames) != 0 && (l.Type.Storage != TypeComposite || len(l.FieldNames) != len(l.Children)) {
		return ErrInvalidTypeLayout
	}
	switch l.Type.Storage {
	case TypeArray:
		if len(l.Children) != 1 {
			return ErrInvalidTypeLayout
		}
	case TypeComposite:
	default:
		if len(l.Children) != 0 {
			return ErrInvalidTypeLayout
		}
	}
	return nil
}
