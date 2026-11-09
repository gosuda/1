package bind

import (
	"errors"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type relationNamespace struct {
	qualifier   relationQualifier
	columns     []column
	physical    []column
	identity    [4]column
	descriptor  *model.Descriptor
	occurrence  model.ScanOccurrence
	subquery    bool
	// systemColumns reports whether PostgreSQL would expose physical system
	// columns (ctid, xmin, cmin, xmax, cmax) on this range: base tables,
	// materialized views, and system catalog relations.
	systemColumns bool
	rowType       model.TypeID
	presence      column
	row           *column
}

func (s *scope) relationNamespace(q relationQualifier, pos int) (relationNamespace, bool, error) {
	var result relationNamespace
	found := false
	for _, n := range s.namespaces {
		if n.qualifier.name != q.name || q.namespace != "" && n.qualifier.namespace != q.namespace {
			continue
		}
		if found {
			return relationNamespace{}, false, bindError("42P09", "table reference is ambiguous", pos)
		}
		result, found = n, true
	}
	if found {
		return result, true, nil
	}
	// Mutation image fields and USING aliases also participate in column lookup.
	present, err := q.present(s.columns, pos)
	return result, present, err
}

func (b *binder) chargeNamespace(columns, namespaces int) error {
	bytes := uint64(columns)*uint64(unsafe.Sizeof(column{})) + uint64(namespaces)*uint64(unsafe.Sizeof(relationNamespace{}))
	if bytes > b.options.Limits.MaxBytes-b.namespaceBytes || uint64(namespaces) > uint64(b.options.Limits.MaxOperands) {
		return bindError("54000", "relation namespace metadata limit exceeded", 0)
	}
	b.namespaceBytes += bytes
	return b.ctx.Err()
}

func (b *binder) noteWholeRowReference(ref *zpgparse.ColumnRef) error {
	if len(ref.Fields) != 1 && len(ref.Fields) != 2 {
		return nil
	}
	first := ref.Fields[0].GetString_()
	if first == nil {
		return nil
	}
	q := relationQualifier{name: first.Sval}
	if len(ref.Fields) == 2 {
		second := ref.Fields[1].GetString_()
		if second == nil {
			return nil
		}
		q.namespace, q.name = first.Sval, second.Sval
	}
	if b.wholeRowNames[q] {
		return nil
	}
	if err := b.chargeNamespace(0, 1); err != nil {
		return err
	}
	if b.wholeRowNames == nil {
		b.wholeRowNames = make(map[relationQualifier]bool)
	}
	b.wholeRowNames[q] = true
	return nil
}
func (b *binder) nameRelation(rel relation, s *scope, q relationQualifier, descriptor *model.Descriptor) (relation, error) {
	return b.nameRelationWithRowType(rel, s, q, descriptor, 0)
}

func (b *binder) nameRelationWithRowType(rel relation, s *scope, q relationQualifier, descriptor *model.Descriptor, rowType model.TypeID) (relation, error) {
	if q.name == "" {
		rel.namespaces = nil
		return rel, nil
	}
	if err := b.chargeNamespace(2*len(rel.columns), 1); err != nil {
		return relation{}, err
	}
	n := relationNamespace{qualifier: q, descriptor: descriptor, rowType: rowType, occurrence: rel.occurrence, columns: make([]column, 0, len(rel.columns)), physical: slices.Clone(rel.columns)}
	if descriptor != nil {
		switch descriptor.Kind() {
		case model.ObjectTable, model.ObjectMaterializedView:
			n.systemColumns = true
		}
	}
	for _, c := range rel.columns {
		if !c.hidden && !c.internal && !c.system {
			n.columns = append(n.columns, c)
		}
	}
	if descriptor != nil && descriptor.Kind() == model.ObjectTable && len(rel.columns) >= len(n.identity) {
		copy(n.identity[:], rel.columns[len(rel.columns)-len(n.identity):])
	}
	return b.bindNamespace(rel, s, n)
}

func (b *binder) bindNamespace(rel relation, s *scope, n relationNamespace) (relation, error) {
	q := n.qualifier
	demanded := b.wholeRowNames[q] || b.wholeRowNames[relationQualifier{name: q.name}]
	if q.namespace == "" || !b.wholeRowNames[q] {
		for _, c := range n.columns {
			if c.Name == q.name {
				demanded = false
				break
			}
		}
	}
	if demanded {
		// Null extension changes this non-null sentinel even for zero/all-NULL rows.
		value, err := b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(true)}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, value.typ, false, 0, 0)
		if err != nil {
			return relation{}, err
		}
		n.presence = column{ResultColumn: ResultColumn{Slot: slot, Layout: value.typ}, hidden: true, internal: true}
		bindings := make([]ir.Binding, 0, len(rel.columns)+1)
		for _, c := range rel.columns {
			e, err := b.reference(s, c, 0)
			if err != nil {
				return relation{}, err
			}
			bindings = append(bindings, ir.Binding{Slot: c.Slot, Expr: e.id})
		}
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
		rel.columns = append(slices.Clone(rel.columns), n.presence)
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: rel.effects}, Inputs: []ir.RelID{rel.id}, Outputs: outputSlots(rel.columns), Bindings: bindings})
		if err != nil {
			return relation{}, err
		}
		rel.id = id
	}
	rel.namespaces = []relationNamespace{n}
	return rel, nil
}

func (b *binder) aliasRelation(rel relation, s *scope, alias *zpgparse.Alias, fallback string) (relation, error) {
	columns, err := aliasColumns(rel.columns, alias, fallback)
	if err != nil {
		return relation{}, err
	}
	rel.columns = columns
	return b.nameRelation(rel, s, relationQualifier{name: columnsQualifier(alias, fallback)}, nil)
}

func (b *binder) joinNamespaces(left, right relation, output []column) ([]relationNamespace, error) {
	count := len(left.namespaces) + len(right.namespaces)
	if err := b.chargeNamespace(0, count); err != nil {
		return nil, err
	}
	result := make([]relationNamespace, 0, count)
	remapped := make(map[model.SlotID]column)
	for i, c := range left.columns {
		if c.Slot != output[i].Slot || c.Nullable != output[i].Nullable {
			remapped[c.Slot] = output[i]
		}
	}
	for i, c := range right.columns {
		if c.Slot != output[len(left.columns)+i].Slot || c.Nullable != output[len(left.columns)+i].Nullable {
			remapped[c.Slot] = output[len(left.columns)+i]
		}
	}
	for _, side := range [][]relationNamespace{left.namespaces, right.namespaces} {
		for _, n := range side {
			var err error
			n, err = b.remapNamespace(n, remapped)
			if err != nil {
				return nil, err
			}
			result = append(result, n)
		}
	}
	return result, nil
}

func (b *binder) joinLockTargets(left, right relation, output []column) ([]relationNamespace, error) {
	left.namespaces = left.lockTargets
	right.namespaces = right.lockTargets
	return b.joinNamespaces(left, right, output)
}

func (b *binder) remapNamespace(n relationNamespace, remapped map[model.SlotID]column) (relationNamespace, error) {
	changed := func(columns []column) bool {
		for _, c := range columns {
			if _, ok := remapped[c.Slot]; ok {
				return true
			}
		}
		return false
	}
	remap := func(columns []column) ([]column, error) {
		if !changed(columns) {
			return columns, nil
		}
		if err := b.chargeNamespace(len(columns), 0); err != nil {
			return nil, err
		}
		columns = slices.Clone(columns)
		for i, c := range columns {
			if replacement, ok := remapped[c.Slot]; ok {
				columns[i].Slot, columns[i].Nullable = replacement.Slot, replacement.Nullable
			}
		}
		return columns, nil
	}
	var err error
	n.columns, err = remap(n.columns)
	if err != nil {
		return n, err
	}
	n.physical, err = remap(n.physical)
	if err != nil {
		return n, err
	}
	for i, identity := range n.identity {
		if replacement, ok := remapped[identity.Slot]; ok {
			n.identity[i].Slot, n.identity[i].Nullable = replacement.Slot, replacement.Nullable
		}
	}
	if replacement, ok := remapped[n.presence.Slot]; ok {
		n.presence = replacement
	}
	return n, nil
}

func (b *binder) wholeRowReference(s *scope, parts []string, pos int, columnError error) (expression, error) {
	var diagnostic *model.Error
	if !errors.As(columnError, &diagnostic) {
		return expression{}, columnError
	}
	notFound := diagnostic.Code == "42703" || diagnostic.Code == "42P01"
	if !notFound || len(parts) < 1 || len(parts) > 2 {
		return expression{}, columnError
	}
	q := relationQualifier{name: parts[len(parts)-1]}
	if len(parts) == 2 {
		q.namespace = parts[0]
	}
	for p := s; p != nil; p = p.parent {
		n, found, err := p.relationNamespace(q, pos)
		if err != nil {
			return expression{}, err
		}
		if !found {
			continue
		}
		if p.lateralForbidden {
			return expression{}, bindError("42P10", "LATERAL reference requires INNER or LEFT join", pos)
		}
		if n.qualifier.name == "" {
			return expression{}, columnError
		}
		if !p.inAggregate {
			for i, group := range p.groupNodes {
				ref := group.GetColumnRef()
				if ref == nil {
					continue
				}
				parts, err := names(ref.Fields)
				if err != nil || len(parts) == 0 || len(parts) > 2 {
					continue
				}
				if _, err := p.lookup(parts, pos); err == nil {
					continue
				}
				qualifier := relationQualifier{name: parts[len(parts)-1]}
				if len(parts) == 2 {
					qualifier.namespace = parts[0]
				}
				grouped, found, err := p.relationNamespace(qualifier, pos)
				if err == nil && found && grouped.qualifier == n.qualifier {
					return b.reference(s, p.groupColumns[i], pos)
				}
			}
		}
		if n.row != nil {
			row, err := b.reference(s, *n.row, pos)
			if err != nil {
				return expression{}, err
			}
			layout, err := b.relationRowType(n.descriptor, pos)
			if err != nil {
				return expression{}, err
			}
			return b.addScalar(s, expression{typ: layout, nullable: row.nullable, position: pos, record: recordDeclared, recordFields: n.columns}, ir.Scalar{Kind: ir.Relabel, CastContext: ir.ImplicitCast}, []expression{row})
		}
		return b.relationRow(s, p, n, pos)
	}
	return expression{}, columnError
}

func (b *binder) relationRow(s, owner *scope, n relationNamespace, pos int) (expression, error) {
	if err := b.checkStarExpansion(len(n.columns), pos); err != nil {
		return expression{}, err
	}
	var layout model.TypeLayout
	if n.descriptor != nil {
		var err error
		layout, err = b.relationRowType(n.descriptor, pos)
		if err != nil {
			return expression{}, err
		}
	} else if n.rowType != 0 {
		layout = model.TypeLayout{Type: model.Type{ID: n.rowType, Storage: model.TypeComposite, Modifier: -1}, Children: make([]model.TypeLayout, len(n.columns))}
		for i, c := range n.columns {
			layout.Children[i] = c.Layout
		}
	} else {
		layout = b.primitive(2249)
		layout.Children = make([]model.TypeLayout, len(n.columns))
		for i, c := range n.columns {
			layout.Children[i] = c.Layout
		}
	}
	if err := b.chargePresentation(uint64(len(n.columns)) * uint64(unsafe.Sizeof(""))); err != nil {
		return expression{}, err
	}
	layout.FieldNames = make([]string, len(n.columns))
	for i, c := range n.columns {
		if err := b.chargePresentation(uint64(len(c.Name))); err != nil {
			return expression{}, err
		}
		layout.FieldNames[i] = strings.Clone(c.Name)
	}
	args := make([]expression, len(n.columns))
	for i, c := range n.columns {
		var err error
		args[i], err = b.reference(s, owner.starColumn(s, c), pos)
		if err != nil {
			return expression{}, err
		}
	}
	row, err := b.addScalar(s, expression{typ: layout, position: pos}, ir.Scalar{Kind: ir.Row}, args)
	if err != nil {
		return expression{}, err
	}
	if n.presence.Nullable {
		present, err := b.reference(s, n.presence, pos)
		if err != nil {
			return expression{}, err
		}
		absent, err := b.addScalar(s, expression{typ: layout, nullable: true, value: model.NullVal(model.TypeComposite)}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return expression{}, err
		}
		row, err = b.addScalar(s, expression{typ: layout, nullable: true, position: pos}, ir.Scalar{Kind: ir.Case}, []expression{present, row, absent})
		if err != nil {
			return expression{}, err
		}
	}
	row.record, row.recordFields, row.rowArgs = recordDeclared, n.columns, nil
	return row, nil
}

func (b *binder) relationRowType(relation *model.Descriptor, pos int) (model.TypeLayout, error) {
	relationSpec, err := b.descriptorSpec(relation)
	if err != nil {
		return model.TypeLayout{}, err
	}
	var rowType model.ObjectID
	var columns []model.ColumnDescriptor
	switch relation.Kind() {
	case model.ObjectTable:
		if relationSpec.Table != nil {
			rowType = relationSpec.Table.RowType
			columns = publicColumns(relationSpec.Table)
		}
	case model.ObjectView:
		if relationSpec.View != nil {
			rowType = relationSpec.View.RowType
			columns = relationSpec.View.Columns
		}
	case model.ObjectMaterializedView:
		if relationSpec.Table != nil && relationSpec.View != nil && relationSpec.Table.RowType == relationSpec.View.RowType {
			rowType = relationSpec.Table.RowType
			columns = relationSpec.View.Columns
		}
	}
	if rowType == 0 {
		return model.TypeLayout{}, bindError("42704", "relation row type metadata is unavailable", pos)
	}
	published, err := b.catalog.Descriptor(b.ctx, rowType)
	if isMissingObject(err) {
		return model.TypeLayout{}, bindError("42704", "relation row type metadata is unavailable", pos)
	}
	if err != nil {
		return model.TypeLayout{}, err
	}
	descriptor := published.Descriptor
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return model.TypeLayout{}, err
	}
	if descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectType || spec.Type == nil || spec.Type.Relation != relation.ID() || spec.Type.Layout.Type.ID == 2249 || spec.Type.Layout.Type.ID != model.TypeID(descriptor.OID()) || spec.Type.Layout.Type.Storage != model.TypeComposite {
		return model.TypeLayout{}, bindError("XX000", "relation row type metadata is inconsistent", pos)
	}
	index := 0
	for _, field := range spec.Type.Fields {
		if field.State != model.StatePublic {
			continue
		}
		if index >= len(columns) || index >= len(spec.Type.Layout.Children) || field.ID != columns[index].ID || field.Name != columns[index].Name || !sameType(field.Layout, columns[index].Layout) || !sameType(field.Layout, spec.Type.Layout.Children[index]) {
			return model.TypeLayout{}, bindError("XX000", "relation row type fields differ from relation", pos)
		}
		index++
		b.dependency(descriptor, field.ID)
	}
	if index != len(columns) || index != len(spec.Type.Layout.Children) {
		return model.TypeLayout{}, bindError("XX000", "relation row type fields differ from relation", pos)
	}
	b.dependency(descriptor, 0)
	return spec.Type.Layout, nil
}

func projectedRecordFields(e expression) []column {
	if e.typ.Type.ID != 2249 {
		return nil
	}
	return e.recordFields
}

func sameRecordFields(left, right []column) bool {
	return slices.EqualFunc(left, right, func(a, b column) bool {
		return a.Name == b.Name && sameType(a.Layout, b.Layout) && a.record == b.record && sameRecordFields(a.recordFields, b.recordFields)
	})
}
