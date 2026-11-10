package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type collationStrength uint8

const (
	collationNone collationStrength = iota
	collationImplicit
	collationConflict
	collationExplicit
)

type collationState struct {
	id       model.CollationID
	strength collationStrength
	position int
}

// collatable reports whether a type takes part in collation derivation.
// Catalog types answer from the leaf predicate; a registered
// information_schema domain is collatable when its base type is, because
// PostgreSQL derives the domain's collation from that base. Only the sealed
// registry knows those domains, so the binder owns this rule and the leaf
// stays an identity check over the static catalog.
func (b *binder) collatable(typ model.Type) bool {
	if builtin.TypeCollatable(typ) {
		return true
	}
	binding, ok := b.options.CatalogRelations.ResolveInformationSchemaType(typ.ID)
	if !ok {
		return false
	}
	switch binding.Kind {
	case model.TypeKindDomain:
		return builtin.TypeCollatable(binding.BaseLayout.Type)
	case model.TypeKindArray:
		// A domain array's base is the domain itself; recurse to reach the
		// static type that actually carries the collation.
		return b.collatable(binding.BaseLayout.Type)
	default:
		return false
	}
}

func (b *binder) expressionCollation(e expression) collationState {
	if e.collation.strength != collationNone {
		return e.collation
	}
	if b.collatable(e.typ.Type) && e.typ.Type.Collation != 0 {
		return collationState{e.typ.Type.Collation, collationImplicit, e.position}
	}
	return collationState{}
}

func (b *binder) commonCollation(args []expression) (collationState, error) {
	var result collationState
	for _, arg := range args {
		next := b.expressionCollation(arg)
		if next.strength > result.strength {
			result = next
			continue
		}
		if next.strength != result.strength || next.id == result.id {
			continue
		}
		switch next.strength {
		case collationImplicit:
			if result.id == 100 {
				result = next
			} else if next.id != 100 {
				result = collationState{strength: collationConflict, position: next.position}
			}
		case collationExplicit:
			return result, bindError("42P21", "collation mismatch between explicit collations", next.position)
		}
	}
	return result, nil
}

func (b *binder) resultCollation(typ model.TypeLayout, state collationState) (model.TypeLayout, collationState) {
	if !b.collatable(typ.Type) {
		return typ, collationState{}
	}
	if state.strength == collationNone {
		if info, ok := builtin.LookupType(typ.Type.ID); ok {
			typ.Type.Collation = info.Collation
		}
		state = b.expressionCollation(expression{typ: typ})
	}
	typ.Type.Collation = state.id
	return typ, state
}

func (b *binder) relabelCollation(s *scope, e expression, id model.CollationID) (expression, error) {
	if e.typ.Type.Collation == id {
		return e, nil
	}
	// Physical relabels for common layouts retain the source's SQL derivation.
	result := expression{typ: e.typ, nullable: e.nullable, position: e.position, collation: b.expressionCollation(e)}
	result.typ.Type.Collation = id
	return b.addScalar(s, result, ir.Scalar{Kind: ir.Collate}, []expression{e})
}

func (b *binder) coerceCommon(s *scope, e expression, target model.TypeLayout) (expression, error) {
	result, err := b.coerce(s, e, target, ir.ImplicitCast)
	if err != nil || !b.collatable(target.Type) {
		return result, err
	}
	return b.relabelCollation(s, result, target.Type.Collation)
}

func (b *binder) collate(n *zpgparse.CollateClause, s *scope) (expression, error) {
	pos := int(n.Location) + 1
	arg, err := b.scalar(n.Arg, s)
	if err != nil {
		return arg, err
	}
	arg, err = b.defaultType(s, arg)
	if err != nil {
		return arg, err
	}
	id, err := b.columnCollation(n, arg.typ)
	if err != nil {
		return arg, err
	}
	result := expression{typ: arg.typ, nullable: arg.nullable, position: pos, collation: collationState{id, collationExplicit, pos}}
	result.typ.Type.Collation = id
	return b.addScalar(s, result, ir.Scalar{Kind: ir.Collate}, []expression{arg})
}

func (b *binder) columnCollation(n *zpgparse.CollateClause, typ model.TypeLayout) (model.CollationID, error) {
	pos := int(n.Location) + 1
	parts, err := names(n.Collname)
	if err != nil {
		return 0, err
	}
	if len(parts) == 0 || len(parts) > 2 {
		return 0, bindError("42704", "collation does not exist", pos)
	}
	var info builtin.CollationInfo
	found := false
	for _, schema := range b.functionPath(parts) {
		if schema == "pg_catalog" {
			info, found = builtin.LookupCollationName(parts[len(parts)-1])
			break
		}
	}
	if !found {
		return 0, bindError("42704", "collation does not exist", pos)
	}
	if err := ValidateBuiltinCollation(info.ID, pos); err != nil {
		return 0, err
	}
	if !b.collatable(typ.Type) {
		return 0, bindError("42804", "collations are not supported by this type", pos)
	}
	return info.ID, nil
}

func (b *binder) requireKeyCollation(e expression) error {
	if b.expressionCollation(e).strength == collationConflict {
		return bindError("42P21", "collation mismatch between implicit collations", e.position)
	}
	return nil
}
