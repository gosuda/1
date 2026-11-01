package bind

import (
	"fmt"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) layout(typ model.TypeLayout) (ir.LayoutID, error) {
	var err error
	typ, err = b.presentationLayout(typ, 0)
	if err != nil {
		return 0, err
	}
	if len(typ.Children) == 0 {
		if id, ok := b.layouts[typ.Type]; ok {
			return id, nil
		}
	}
	id, err := b.builder.AddLayout(b.ctx, typ)
	if err == nil && len(typ.Children) == 0 {
		b.layouts[typ.Type] = id
	}
	return id, err
}

func (b *binder) presentationLayout(typ model.TypeLayout, depth uint32) (model.TypeLayout, error) {
	if err := b.ctx.Err(); err != nil {
		return model.TypeLayout{}, err
	}
	if depth >= min(b.options.Limits.MaxDepth, 65) {
		return model.TypeLayout{}, bindError("54000", "type presentation depth limit exceeded", 0)
	}
	if uint64(len(typ.Children)) > uint64(b.options.Limits.MaxOperands) {
		return model.TypeLayout{}, bindError("54000", "type presentation operand limit exceeded", 0)
	}
	if len(typ.Children) != 0 {
		if b.nodes >= b.options.Limits.MaxNodes {
			return model.TypeLayout{}, bindError("54000", "type presentation work limit exceeded", 0)
		}
		b.nodes++
	}
	if typ.Type.Storage == model.TypeComposite && typ.Type.ID != 2249 && typ.FieldNames == nil {
		names, err := b.presentationFieldNames(typ)
		if err != nil {
			return model.TypeLayout{}, err
		}
		typ.FieldNames = names
	}
	ownedChildren := false
	for i, child := range typ.Children {
		hydrated, err := b.presentationLayout(child, depth+1)
		if err != nil {
			return model.TypeLayout{}, err
		}
		if (child.FieldNames == nil) == (hydrated.FieldNames == nil) && slices.Equal(child.FieldNames, hydrated.FieldNames) && !presentationChildrenChanged(child.Children, hydrated.Children) {
			continue
		}
		if !ownedChildren {
			if err := b.chargePresentation(uint64(len(typ.Children)) * uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
				return model.TypeLayout{}, err
			}
			typ.Children = slices.Clone(typ.Children)
			ownedChildren = true
		}
		typ.Children[i] = hydrated
	}
	return typ, nil
}

func (b *binder) presentationFieldNames(typ model.TypeLayout) ([]string, error) {
	descriptor, err := b.findNamedType(typ.Type.ID, 0)
	if err != nil || descriptor == nil {
		return nil, err
	}
	fields, err := b.compositeFields(descriptor, typ, 0)
	if err != nil {
		return nil, err
	}
	if err := b.chargePresentation(uint64(len(fields)) * uint64(unsafe.Sizeof(""))); err != nil {
		return nil, err
	}
	names := make([]string, len(fields))
	for i, field := range fields {
		if err := b.chargePresentation(uint64(len(field.Name))); err != nil {
			return nil, err
		}
		names[i] = strings.Clone(field.Name)
	}
	return names, nil
}

func presentationChildrenChanged(left, right []model.TypeLayout) bool {
	return len(left) != 0 && &left[0] != &right[0]
}

func (b *binder) chargePresentation(bytes uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if bytes > b.options.Limits.MaxBytes-b.namespaceBytes {
		return bindError("54000", "type presentation metadata limit exceeded", 0)
	}
	b.namespaceBytes += bytes
	return nil
}
func (b *binder) primitive(id model.TypeID) model.TypeLayout {
	if typ, ok := b.options.CatalogRelations.LookupTypeID(id); ok {
		return typ
	}
	typ := model.Type{ID: id, Modifier: -1}
	switch id {
	case 16:
		typ.Storage = model.TypeBoolean
	case 20, 21, 23, 28, 29, 790:
		typ.Storage = model.TypeInt64
	case 700, 701:
		typ.Storage = model.TypeFloat64
	case 17, 600, 601, 602, 603, 604, 628, 650, 718, 774, 829, 869, 1266, 1560, 1562, 3614, 3615, 4072, 5017:
		typ.Storage = model.TypeBytes
	case 18, 19, 25, 194, 705, 1042, 1043, 2275:
		typ.Storage = model.TypeString
	case 1700:
		typ.Storage = model.TypeNumeric
	case 1082, 1083, 1114, 1184:
		typ.Storage = model.TypeTimestamp
	case 1186:
		typ.Storage = model.TypeInterval
	case 2950:
		typ.Storage = model.TypeUUID
	case 114, 3802:
		typ.Storage = model.TypeJSONB
	case 2249:
		typ.Storage = model.TypeComposite
	default:
		if _, ok := builtin.LookupRangeFamily(id); ok {
			typ.Storage = model.TypeBytes
		}
		if builtin.IsOIDType(id) {
			typ.Storage = model.TypeInt64
		}
	}
	if info, ok := builtin.LookupType(id); ok {
		typ.Collation = info.Collation
		if info.IsArray() {
			child := b.primitive(info.Element)
			if child.Type.Storage != model.TypeAny {
				typ.Storage = model.TypeArray
				return model.TypeLayout{Type: typ, Children: []model.TypeLayout{child}}
			}
		}
	}
	return model.TypeLayout{Type: typ}
}
func (b *binder) typeLayoutByID(id model.TypeID) (model.TypeLayout, bool) {
	if typ, ok := b.options.CatalogRelations.LookupTypeID(id); ok {
		return typ, true
	}
	if _, known := builtin.LookupType(id); known {
		return b.primitive(id), true
	}
	descriptor, err := b.findNamedType(id, 0)
	if err != nil || descriptor == nil {
		return model.TypeLayout{}, false
	}
	spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
	if err != nil || spec.Type == nil {
		return model.TypeLayout{}, false
	}
	return spec.Type.Layout, true
}
func (b *binder) typeName(node *zpgparse.TypeName) (model.TypeLayout, error) {
	if node == nil {
		return model.TypeLayout{}, bindError("42601", "missing type name", 0)
	}
	pos := int(node.Location) + 1
	parts, err := names(node.Names)
	if err != nil {
		return model.TypeLayout{}, err
	}
	var typ model.TypeLayout
	var parent model.ObjectID
	if node.PctType {
		// func_type carries rel.col%TYPE in Names: the trailing name is a
		// column of the relation spelled by the leading parts.
		if len(parts) < 2 || len(parts) > 3 {
			return model.TypeLayout{}, bindError("42703", "column does not exist", pos)
		}
		typ, parent, err = b.pctColumnLayout(parts[:len(parts)-1], parts[len(parts)-1], pos)
		if err != nil {
			return typ, err
		}
	} else if node.Setof {
		return model.TypeLayout{}, missing("SETOF", pos)
	} else {
		if len(parts) == 0 || len(parts) > 2 {
			return model.TypeLayout{}, bindError("42704", "type does not exist", pos)
		}
		name := parts[len(parts)-1]
		var id model.TypeID
		if len(parts) == 1 || parts[0] == "pg_catalog" {
			for info := range builtin.TypesNamed(name) {
				id = info.ID
				break
			}
		}
		logical, logicalType := b.logicalCatalogType(parts)
		if logicalType {
			typ = logical
		} else if id != 0 {
			if err := ValidateBuiltinType(id, pos); err != nil {
				return typ, err
			}
			typ = b.primitive(id)
			if typ.Type.Storage == model.TypeAny {
				// A pseudo-type has no storage of its own; the catalog gives these
				// typlen = -1 and typtype = 'p'. A declaration may name one as an
				// unresolved identity (internal state, anyelement argument), and the
				// marker is resolved from the actual call's layouts later. Anywhere
				// else the identity is not a physical layout and must keep failing.
				if !b.options.allowPseudoTypes || !model.IsPseudoTypeID(id) {
					return typ, missing("physical layout for resolved type", pos)
				}
			}
		} else {
			descriptor, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectType}, "42704", pos)
			if err != nil {
				return typ, err
			}
			spec, err := b.descriptorSpec(descriptor)
			if err != nil {
				return typ, err
			}
			if spec.Type == nil {
				return typ, bindError("XX000", "type descriptor has no type metadata", pos)
			}
			typ = spec.Type.Layout
			parent = descriptor.Name().Parent
			if err := b.validateIOLayout(typ, pos); err != nil {
				return typ, err
			}
		}
	}
	if _, domain := b.options.CatalogRelations.InformationDomainByType(typ.Type.ID); domain && len(node.Typmods) != 0 {
		return typ, bindError("42601", "type modifier is not allowed for an information schema domain", pos)
	}
	if len(node.Typmods) != 0 {
		modifier, err := b.scalarTypeModifier(typ.Type.ID, node.Typmods, pos)
		if err != nil {
			return typ, err
		}
		typ.Type.Modifier = modifier
	}
	if len(node.ArrayBounds) != 0 {
		arrayID := b.arrayTypeID(typ.Type.ID)
		if arrayID == 0 {
			var err error
			arrayID, err = b.declaredArrayType(typ, parent, pos)
			if err != nil {
				return typ, err
			}
		}
		typ = model.TypeLayout{Type: model.Type{ID: arrayID, Storage: model.TypeArray, Modifier: -1, Collation: typ.Type.Collation}, Children: []model.TypeLayout{typ}}
	}
	return typ, nil
}

// pctColumnLayout resolves the column type for rel.col%TYPE: relParts name a
// table, view, or composite type and column is its public field. Resolving
// through the catalog records the dependency so schema changes to the column
// invalidate the declaration, as PostgreSQL's own %TYPE dependency does.
func (b *binder) pctColumnLayout(relParts []string, column string, pos int) (model.TypeLayout, model.ObjectID, error) {
	descriptor, _, err := b.resolve(relParts, []model.ObjectKind{
		model.ObjectTable, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation,
	}, "42P01", pos)
	if err != nil {
		return model.TypeLayout{}, 0, err
	}
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return model.TypeLayout{}, 0, err
	}
	var columns []model.ColumnDescriptor
	switch descriptor.Name().Kind {
	case model.ObjectTable:
		if spec.Table != nil {
			columns = spec.Table.Columns
		}
	case model.ObjectView, model.ObjectMaterializedView:
		if spec.View != nil {
			columns = spec.View.Columns
		}
	case model.ObjectCompositeRelation:
		if spec.CompositeRelation != nil {
			columns = spec.CompositeRelation.Columns
		}
	}
	for _, candidate := range columns {
		if candidate.SystemIdentity || candidate.State != model.StatePublic {
			continue
		}
		if candidate.Name == column {
			return candidate.Layout, descriptor.Name().Parent, nil
		}
	}
	return model.TypeLayout{}, 0, bindError("42703", fmt.Sprintf("column %q does not exist", column), pos)
}

func (b *binder) arrayTypeID(id model.TypeID) model.TypeID {
	if info, ok := builtin.LookupType(id); ok && info.Array != 0 {
		return info.Array
	}
	array, _ := b.options.CatalogRelations.CompanionArray(id)
	return array
}

func (b *binder) logicalCatalogType(parts []string) (model.TypeLayout, bool) {
	if !b.options.CatalogRelations.IdentityComplete() {
		return model.TypeLayout{}, false
	}
	name := parts[len(parts)-1]
	if len(parts) == 2 {
		return b.options.CatalogRelations.LookupType(parts[0], name)
	}
	if typ, ok := b.options.CatalogRelations.LookupType("pg_catalog", name); ok {
		return typ, true
	}
	for _, namespace := range b.options.SearchPath {
		if typ, ok := b.options.CatalogRelations.LookupType(namespace, name); ok {
			return typ, true
		}
	}
	return model.TypeLayout{}, false
}
func sameType(a, b model.TypeLayout) bool {
	return a.Type == b.Type && slices.EqualFunc(a.Children, b.Children, sameType)
}
func (b *binder) resolveUnknown(s *scope, e expression, typ model.TypeLayout) (expression, error) {
	if e.typ.Type.ID != 0 {
		return b.coerce(s, e, typ, ir.ImplicitCast)
	}
	if e.parameter != 0 {
		if prior, ok := b.params[e.parameter]; ok && !sameType(prior, typ) {
			return e, bindError("42P08", "inconsistent types deduced for parameter", e.position)
		}
		if _, ok := b.params[e.parameter]; !ok {
			owned, _, err := model.CopyTypeLayout(b.ctx, typ, b.options.Limits.MaxBytes)
			if err != nil {
				return e, err
			}
			b.params[e.parameter] = owned
		}
		e.typ = typ
		return b.addScalar(s, e, ir.Scalar{Kind: ir.Param, Parameter: e.parameter}, nil)
	}
	e.typ = typ
	if e.value.IsNull {
		_, domain, err := b.domainBase(typ)
		if err != nil {
			return e, err
		}
		if domain {
			e.typ = b.primitive(2275)
			e.value = model.NullVal(model.TypeString)
			return b.inputLiteral(s, e, typ)
		}
		e.value = model.NullVal(typ.Type.Storage)
	} else if typ.Type.ID != 25 && typ.Type.ID != 705 && typ.Type.ID != 2275 {
		return b.inputLiteral(s, e, typ)
	}
	return b.addScalar(s, e, ir.Scalar{Kind: ir.Const}, nil)
}
func (b *binder) defaultType(s *scope, e expression) (expression, error) {
	if e.typ.Type.ID == 705 {
		return b.coerce(s, e, b.primitive(25), ir.ImplicitCast)
	}
	if e.typ.Type.ID != 0 {
		return e, nil
	}
	if typ, ok := b.params[e.parameter]; e.parameter != 0 && ok {
		return b.resolveUnknown(s, e, typ)
	}
	if e.inferred != 0 {
		return b.resolveUnknown(s, e, b.primitive(e.inferred))
	}
	return b.resolveUnknown(s, e, b.primitive(25))
}

// isInformationDomain reports whether a target is a registered
// information_schema domain. Such a target keeps the collation its domain
// declares instead of adopting the argument's: sql_identifier is a domain over
// name and is collated C, while character_data is a domain over varchar and
// carries the default placeholder. The sealed registry records exactly those
// domain collations for information_schema columns, so a cast to one of these
// domains must not overwrite them. Every other target, including a plain table
// column, adopts the argument's collation here; assignment paths relabel the
// column's own collation explicitly afterwards.
func (b *binder) isInformationDomain(target model.TypeLayout) bool {
	typ, ok := b.options.CatalogRelations.ResolveInformationSchemaType(target.Type.ID)
	return ok && typ.Kind == model.TypeKindDomain
}

func (b *binder) coerce(s *scope, e expression, target model.TypeLayout, context ir.CastContext) (expression, error) {
	if e.typ.Type.ID == 0 && e.inferred != 0 && e.inferred != target.Type.ID {
		resolved, err := b.resolveUnknown(s, e, b.primitive(e.inferred))
		if err != nil {
			return e, err
		}
		return b.coerce(s, resolved, target, context)
	}
	if b.collatable(e.typ.Type) && b.collatable(target.Type) && !b.isInformationDomain(target) {
		target.Type.Collation = e.typ.Type.Collation
	}
	if sameType(e.typ, target) {
		return e, nil
	}
	genericRecord := target.Type.ID == 2249 && (e.typ.Type.ID != 2249 || len(target.Children) == 0)
	if genericRecord && e.typ.Type.Storage == model.TypeComposite {
		if err := b.validateIOLayout(e.typ, e.position); err != nil {
			return e, err
		}
		return e, nil
	}
	if e.typ.Type.ID == 2249 && target.Type.Storage == model.TypeComposite && target.Type.ID != 2249 {
		return b.coerceRecord(s, e, target, context)
	}
	if e.typ.Type.ID == 2249 && target.Type.ID == 2249 && e.value.IsNull {
		e.typ = target
		return b.addScalar(s, e, ir.Scalar{Kind: ir.Const}, nil)
	}
	arrayPair := e.typ.Type.Storage == model.TypeArray && target.Type.Storage == model.TypeArray && len(e.typ.Children) == 1 && len(target.Children) == 1
	if arrayPair && target.Type.ID != 22 && target.Type.ID != 30 {
		if _, explicit := builtin.LookupCast(e.typ.Type.ID, target.Type.ID); !explicit || e.typ.Type.ID == target.Type.ID {
			return b.coerceArray(s, e, target, context)
		}
	}
	if target.Type.Modifier != -1 && e.typ.Type.ID != target.Type.ID && castAcceptsModifier(e.typ.Type.ID, target.Type.ID) {
		// Integer-to-bit conversion selects bits using the destination length;
		// converting to bit(1) and resizing afterward changes the value.
		return b.coerceBase(s, e, target, context)
	}
	base := target
	base.Type.Modifier = -1
	if target.Type.Storage == model.TypeArray && len(target.Children) == 1 && target.Children[0].Type.Modifier != -1 {
		element := target.Children[0]
		element.Type.Modifier = -1
		base.Children = []model.TypeLayout{element}
	}
	if e.typ.Type.ID == target.Type.ID && target.Type.Modifier != -1 {
		base.Type.Modifier = e.typ.Type.Modifier
	}
	converted, err := b.coerceBase(s, e, base, context)
	if err != nil {
		return e, err
	}
	target.Type.Collation = converted.typ.Type.Collation
	if target.Type.Storage == model.TypeArray && converted.typ.Type.Storage == model.TypeArray && !slices.EqualFunc(converted.typ.Children, target.Children, sameType) {
		return b.coerceArray(s, converted, target, context)
	}
	if target.Type.Modifier == -1 || sameType(converted.typ, target) {
		return converted, nil
	}
	cast, ok := builtin.LookupCast(target.Type.ID, target.Type.ID)
	if !ok || cast.Method != 'f' || cast.Function == 0 {
		return e, missing("type modifier coercion", e.position)
	}
	return b.castFunction(s, converted, target, cast.Function, context)
}

func (b *binder) coerceBase(s *scope, e expression, target model.TypeLayout, context ir.CastContext) (expression, error) {
	if e.typ.Type.ID == 0 {
		if b.collatable(target.Type) {
			target.Type.Collation = b.primitive(target.Type.ID).Type.Collation
		}
		return b.resolveUnknown(s, e, target)
	}
	if sameType(e.typ, target) {
		return e, nil
	}
	if e.typ.Type.ID == target.Type.ID {
		if !slices.EqualFunc(e.typ.Children, target.Children, sameType) {
			return e, missing("nested element coercion", e.position)
		}
		return b.addScalar(s, expression{typ: target, nullable: e.nullable, position: e.position, collation: b.expressionCollation(e)}, ir.Scalar{Kind: ir.Relabel, CastContext: context}, []expression{e})
	}
	if e.typ.Type.ID == 705 {
		return b.coerceViaIO(s, e, target)
	}
	base, domain, err := b.domainBase(target)
	if err != nil {
		return e, err
	}
	if domain {
		converted := e
		if !sameType(e.typ, base) {
			converted, err = b.coerce(s, e, base, context)
			if err != nil {
				return e, err
			}
		}
		return b.coerceViaIO(s, converted, target)
	}
	base, domain, err = b.domainBase(e.typ)
	if err != nil {
		return e, err
	}
	if domain {
		// A domain value is physically its base value, so the relabel to the base
		// keeps the operand's type modifier: PostgreSQL's RelabelType never
		// changes exprTypmod, and a domain layout carries no modifier of its own
		// even when its base is modified, such as a domain over varchar(3).
		base.Type.Modifier = e.typ.Type.Modifier
		relabeled, err := b.addScalar(s, expression{typ: base, nullable: e.nullable, position: e.position, collation: b.expressionCollation(e)}, ir.Scalar{Kind: ir.Relabel, CastContext: context}, []expression{e})
		if err != nil {
			return e, err
		}
		return b.coerceBase(s, relabeled, target, context)
	}
	cast, ok := builtin.LookupCast(e.typ.Type.ID, target.Type.ID)
	if !ok {
		// A user-defined cast may still justify this coercion. Builtin casts are
		// consulted first so fixed semantics are never redefined by a user object.
		if user := b.castLookup(e.typ.Type.ID, target.Type.ID); user.err != nil {
			return e, user.err
		} else if user.found {
			if !castAdmits(user.metadata.Context, context) {
				return e, bindError("42804", "types cannot be matched", e.position)
			}
			return b.coerceUserBase(s, e, target, user.metadata, context)
		}
		if b.canCoerceViaIO(e.typ, target, context) {
			return b.coerceViaIO(s, e, target)
		}
		return e, missing("cast between resolved types", e.position)
	}
	implicitMismatch := context == ir.ImplicitCast && cast.Context != 'i'
	assignmentMismatch := context == ir.AssignmentCast && cast.Context == 'e'
	if implicitMismatch || assignmentMismatch {
		return e, bindError("42804", "types cannot be matched", e.position)
	}
	if cast.Method == 'i' {
		return b.coerceViaIO(s, e, target)
	}
	if cast.Method == 'b' {
		return b.addScalar(s, expression{typ: target, nullable: e.nullable, position: e.position, collation: b.expressionCollation(e)}, ir.Scalar{Kind: ir.Relabel, CastContext: context}, []expression{e})
	}
	if cast.Method != 'f' || cast.Function == 0 {
		return e, missing("binary cast", e.position)
	}
	return b.castFunction(s, e, target, cast.Function, context)
}

// castAdmits reports whether a persisted castcontext admits the requested
// coercion: 'i' is implicit/assignment/explicit, 'a' is assignment/explicit,
// and 'e' is explicit only. This mirrors PostgreSQL's find_coercion_pathway.
func castAdmits(persisted byte, context ir.CastContext) bool {
	switch context {
	case ir.ImplicitCast:
		return persisted == 'i'
	case ir.AssignmentCast:
		return persisted == 'i' || persisted == 'a'
	default:
		return true
	}
}

func implicitCast(from, to model.TypeID) bool {
	if from == to {
		return true
	}
	cast, ok := builtin.LookupCast(from, to)
	if ok {
		return cast.Context == 'i' && (cast.Method == 'f' || cast.Method == 'b')
	}
	if to == 22 || to == 30 {
		return false
	}
	source, sourceKnown := builtin.LookupType(from)
	target, targetKnown := builtin.LookupType(to)
	return sourceKnown && targetKnown && source.IsArray() && target.IsArray() && implicitCast(source.Element, target.Element)
}
func (b *binder) commonType(expressions []expression) (model.TypeLayout, error) {
	var typ model.TypeLayout
	var nullRecord model.TypeLayout
	for _, e := range expressions {
		if e.typ.Type.ID == 0 && e.inferred != 0 {
			e.typ = b.primitive(e.inferred)
		}
		if e.typ.Type.ID == 0 || e.typ.Type.ID == 705 {
			continue
		}
		if e.typ.Type.ID == 2249 && e.value.IsNull {
			nullRecord = e.typ
			continue
		}
		if typ.Type.ID == 0 {
			typ = e.typ
			continue
		}
		if sameType(typ, e.typ) {
			continue
		}
		if typ.Type.ID == e.typ.Type.ID {
			if !slices.EqualFunc(typ.Children, e.typ.Children, sameType) {
				if typ.Type.Storage != model.TypeArray || e.typ.Type.Storage != model.TypeArray || len(typ.Children) != 1 || len(e.typ.Children) != 1 {
					return typ, missing("nested common type coercion", e.position)
				}
				element, err := b.commonType([]expression{{typ: typ.Children[0], position: e.position}, {typ: e.typ.Children[0], position: e.position}})
				if err != nil {
					return typ, err
				}
				typ.Children = []model.TypeLayout{element}
			}
			continue
		}
		toCurrent := implicitCast(e.typ.Type.ID, typ.Type.ID)
		if !toCurrent && implicitCast(typ.Type.ID, e.typ.Type.ID) {
			typ = e.typ
			continue
		}
		if !toCurrent {
			return typ, bindError("42804", "types cannot be matched", e.position)
		}
	}
	if nullRecord.Type.ID != 0 && typ.Type.ID != 0 && typ.Type.ID != 2249 {
		return typ, bindError("42804", "types cannot be matched", 0)
	}
	if typ.Type.ID == 0 && nullRecord.Type.ID != 0 {
		typ = nullRecord
	}
	if typ.Type.ID == 0 {
		typ = b.primitive(25)
	}
	for _, e := range expressions {
		if e.typ.Type.ID != typ.Type.ID || e.typ.Type.Modifier != typ.Type.Modifier {
			typ.Type.Modifier = -1
			break
		}
	}
	if typ.Type.Storage == model.TypeArray && len(typ.Children) == 1 && typ.Children[0].Type.Modifier != -1 {
		for _, e := range expressions {
			if e.typ.Type.ID == typ.Type.ID && len(e.typ.Children) == 1 && e.typ.Children[0].Type.Modifier == typ.Children[0].Type.Modifier {
				continue
			}
			element := typ.Children[0]
			element.Type.Modifier = -1
			typ.Children = []model.TypeLayout{element}
			break
		}
	}
	state, err := b.commonCollation(expressions)
	if err != nil {
		return typ, err
	}
	typ, _ = b.resultCollation(typ, state)
	return typ, nil
}
