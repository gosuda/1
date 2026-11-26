package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type rowField struct {
	row      model.SlotID
	layout   model.TypeLayout
	index    uint32
	nullable bool
}

func (b *binder) returning(s *scope, target mutationTarget, kind ir.ModifyKind, clause *zpgparse.ReturningClause) (ir.RelSpec, []column, error) {
	var spec ir.RelSpec
	if clause == nil && kind != ir.Merge {
		return spec, nil, nil
	}
	var input []column
	if kind != ir.Insert {
		input = s.columns
	}
	sourceNamespace := &scope{columns: input}
	if kind != ir.Insert {
		sourceNamespace.namespaces = s.namespaces
	}
	aliases := returningAliases{old: "old", new: "new"}
	if clause != nil {
		var err error
		aliases, err = bindReturningAliases(clause, target.qualifier, sourceNamespace)
		if err != nil {
			return spec, nil, err
		}
	}
	columns := publicColumns(target.table)
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: make([]model.TypeLayout, len(columns))}
	for i, c := range columns {
		layout.Children[i] = c.Layout
	}
	oldNullable, newNullable := kind == ir.Insert || kind == ir.Merge, kind == ir.Delete || kind == ir.Merge
	oldRow, err := b.slot(s, layout, oldNullable, target.descriptor.ID(), 0)
	if err != nil {
		return spec, nil, err
	}
	newRow, err := b.slot(s, layout, newNullable, target.descriptor.ID(), 0)
	if err != nil {
		return spec, nil, err
	}
	spec.OldRow = oldRow
	spec.NewRow = newRow
	oid := b.primitive(26)
	if !target.view || kind == ir.Merge {
		spec.OldTableOID, err = b.slot(s, oid, oldNullable, target.descriptor.ID(), 0)
		if err != nil {
			return spec, nil, err
		}
		spec.NewTableOID, err = b.slot(s, oid, newNullable, target.descriptor.ID(), 0)
		if err != nil {
			return spec, nil, err
		}
	}
	if kind == ir.Merge {
		spec.MergeResultRow, err = b.slot(s, layout, false, target.descriptor.ID(), 0)
		if err != nil {
			return spec, nil, err
		}
		spec.MergeActionSlot, err = b.slot(s, b.primitive(25), false, 0, 0)
		if err != nil {
			return spec, nil, err
		}
		spec.MergeTableOID, err = b.slot(s, oid, false, target.descriptor.ID(), 0)
		if err != nil {
			return spec, nil, err
		}
	}
	if clause == nil {
		return spec, nil, nil
	}
	local := *s
	local.columns = nil
	local.namespaces = nil
	local.allowAggregate = false
	local.allowWindow = false
	local.mergeActionSlot = spec.MergeActionSlot
	for i, c := range columns {
		primary := rowField{row: newRow, layout: layout, index: uint32(i), nullable: newNullable}
		if kind == ir.Delete {
			primary = rowField{row: oldRow, layout: layout, index: uint32(i), nullable: oldNullable}
		}
		if kind == ir.Merge {
			primary = rowField{row: spec.MergeResultRow, layout: layout, index: uint32(i)}
		}
		local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: c.Name, Layout: c.Layout, Nullable: c.Nullable}, qualifier: target.qualifier.name, namespace: target.qualifier.namespace, field: &primary})
		if aliases.old != "" {
			oldField := rowField{row: oldRow, layout: layout, index: uint32(i), nullable: oldNullable}
			local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: c.Name, Layout: c.Layout, Nullable: c.Nullable}, qualifier: aliases.old, hidden: true, field: &oldField})
		}
		if aliases.new != "" {
			newField := rowField{row: newRow, layout: layout, index: uint32(i), nullable: newNullable}
			local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: c.Name, Layout: c.Layout, Nullable: c.Nullable}, qualifier: aliases.new, hidden: true, field: &newField})
		}
	}
	if !target.view {
		primaryOID, primaryOIDNullable := spec.NewTableOID, newNullable
		if kind == ir.Delete {
			primaryOID, primaryOIDNullable = spec.OldTableOID, oldNullable
		}
		if kind == ir.Merge {
			primaryOID, primaryOIDNullable = spec.MergeTableOID, false
		}
		local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: "tableoid", Layout: oid, Nullable: primaryOIDNullable, Slot: primaryOID}, qualifier: target.qualifier.name, namespace: target.qualifier.namespace, system: true})
		if aliases.old != "" {
			local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: "tableoid", Layout: oid, Nullable: oldNullable, Slot: spec.OldTableOID}, qualifier: aliases.old, hidden: true, system: true})
		}
		if aliases.new != "" {
			local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: "tableoid", Layout: oid, Nullable: newNullable, Slot: spec.NewTableOID}, qualifier: aliases.new, hidden: true, system: true})
		}
	}
	for _, c := range input {
		if !target.qualifier.matches(c) {
			local.columns = append(local.columns, c)
		}
	}
	for _, n := range s.namespaces {
		// The RETURNING image namespace below replaces the target's FROM
		// registration; match on the qualifier name because the target's
		// qualifier carries its resolved schema namespace.
		if n.qualifier.name != target.qualifier.name {
			local.namespaces = append(local.namespaces, n)
		}
	}
	for _, image := range []struct {
		qualifier relationQualifier
		slot      model.SlotID
		nullable  bool
	}{
		{target.qualifier, newRow, newNullable},
		{relationQualifier{name: aliases.old}, oldRow, oldNullable},
		{relationQualifier{name: aliases.new}, newRow, newNullable},
	} {
		if image.qualifier.name == "" {
			continue
		}
		if image.qualifier == target.qualifier && kind == ir.Delete {
			image.slot, image.nullable = oldRow, oldNullable
		}
		if image.qualifier == target.qualifier && kind == ir.Merge {
			image.slot, image.nullable = spec.MergeResultRow, false
		}
		namespaceColumns := len(columns)
		if !target.view {
			namespaceColumns++
		}
		if err := b.chargeNamespace(namespaceColumns, 1); err != nil {
			return spec, nil, err
		}
		n := relationNamespace{qualifier: image.qualifier, descriptor: target.descriptor, systemColumns: !target.view && target.descriptor != nil && target.descriptor.Kind() == model.ObjectTable}
		var recordFields []column
		for _, c := range local.columns {
			if image.qualifier.matches(c) {
				n.columns = append(n.columns, c)
				if c.field != nil {
					recordFields = append(recordFields, c)
				}
			}
		}
		n.row = &column{ResultColumn: ResultColumn{Slot: image.slot, Layout: layout, Nullable: image.nullable}, record: recordDeclared, recordFields: recordFields}
		local.namespaces = append(local.namespaces, n)
	}
	var output []column
	add := func(name string, e expression) error {
		e, err = b.defaultType(&local, e)
		if err != nil {
			return err
		}
		slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
		if err != nil {
			return err
		}
		c := column{ResultColumn: ResultColumn{Name: name, Layout: e.typ, Nullable: e.nullable, Slot: slot}, collation: b.expressionCollation(e), record: e.record, recordFields: projectedRecordFields(e)}
		if e.typ.Type.ID == 2249 && c.record == recordUnspecified {
			c.record = recordVariable
		}
		output = append(output, c)
		spec.Outputs = append(spec.Outputs, slot)
		spec.Bindings = append(spec.Bindings, ir.Binding{Slot: slot, Expr: e.id})
		spec.Effects = spec.Effects.Union(e.effects)
		return nil
	}
	for _, node := range clause.Exprs {
		target := node.GetResTarget()
		if target == nil {
			return spec, nil, bindError("42601", "invalid RETURNING target", 0)
		}
		if ref := target.Val.GetColumnRef(); ref != nil && len(ref.Fields) > 0 && ref.Fields[len(ref.Fields)-1].GetAStar() != nil {
			qualifier, err := starQualifier(ref, true)
			if err != nil {
				return spec, nil, err
			}
			if qualifier.name != "" {
				if _, err := local.starScope(qualifier, int(ref.Location)+1); err != nil {
					return spec, nil, err
				}
			}
			found := false
			for _, c := range local.columns {
				if !c.visibleToStar(qualifier) {
					continue
				}
				e, err := b.reference(&local, c, int(ref.Location)+1)
				if err != nil {
					return spec, nil, err
				}
				if err := add(c.Name, e); err != nil {
					return spec, nil, err
				}
				found = true
			}
			if !found && qualifier.name != "" && len(local.namespaces) == 0 {
				return spec, nil, bindError("42P01", "RETURNING star relation does not exist", int(ref.Location)+1)
			}
			continue
		}
		expanded, names, star, err := b.expandCompositeStar(target.Val, &local)
		if err != nil {
			return spec, nil, err
		}
		if star {
			if err := b.checkStarExpansion(len(output)+len(expanded), int(target.Location)+1); err != nil {
				return spec, nil, err
			}
			for i, e := range expanded {
				if err := add(names[i], e); err != nil {
					return spec, nil, err
				}
			}
			continue
		}
		e, err := b.scalar(target.Val, &local)
		if err != nil {
			return spec, nil, err
		}
		name := targetName(target)
		if err := add(name, e); err != nil {
			return spec, nil, err
		}
	}
	return spec, output, nil
}
func (b *binder) fieldReference(s *scope, c column, pos int) (expression, error) {
	field := c.field
	if err := b.capture(s, field.row); err != nil {
		return expression{}, err
	}
	row, err := b.addScalar(s, expression{typ: field.layout, nullable: field.nullable, reads: []model.SlotID{field.row}}, ir.Scalar{Kind: ir.Slot, Slot: field.row}, nil)
	if err != nil {
		return expression{}, err
	}
	// Composite slot layouts do not prove individual fields non-null.
	return b.addScalar(s, expression{typ: c.Layout, nullable: true, position: pos}, ir.Scalar{Kind: ir.Field, FieldIndex: field.index}, []expression{row})
}
