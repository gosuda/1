package bind

import (
	"google.golang.org/protobuf/proto"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) bindConflict(s *scope, target mutationTarget, clause *zpgparse.OnConflictClause, spec *ir.RelSpec) error {
	switch clause.Action {
	case zpgparse.OnConflictAction_ONCONFLICT_NOTHING:
		spec.Conflict = ir.ConflictNothing
	case zpgparse.OnConflictAction_ONCONFLICT_UPDATE:
		spec.Conflict = ir.ConflictUpdate
	default:
		return bindError("42601", "invalid ON CONFLICT action", 0)
	}
	if target.logicalTrigger && spec.Conflict == ir.ConflictNothing && clause.Infer == nil {
		return nil
	}
	if target.automatic() && clause.Infer != nil && clause.Infer.Conname != "" {
		return bindError("42704", "ON CONFLICT constraint does not exist", 0)
	}
	if spec.Conflict == ir.ConflictUpdate && clause.Infer == nil {
		return bindError("42601", "ON CONFLICT DO UPDATE requires an inference target", 0)
	}
	arbiters, err := b.conflictArbiters(target.conflictTarget(), clause.Infer)
	if err != nil {
		return err
	}
	spec.ConflictArbiters = arbiters
	if spec.Conflict != ir.ConflictUpdate {
		return nil
	}
	if target.qualifier.name == "excluded" {
		return bindError("42712", "target relation conflicts with EXCLUDED namespace", 0)
	}
	columns := publicColumns(target.table)
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: make([]model.TypeLayout, len(columns))}
	for i, c := range columns {
		layout.Children[i] = c.Layout
	}
	if spec.OldRow == 0 {
		spec.OldRow, err = b.slot(s, layout, true, target.descriptor.ID(), 0)
		if err != nil {
			return err
		}
	}
	spec.ExcludedRow, err = b.slot(s, layout, false, target.descriptor.ID(), 0)
	if err != nil {
		return err
	}
	local := *s
	local.columns, local.namespaces = nil, nil
	local.allowAggregate, local.allowWindow = false, false
	for _, image := range []struct {
		qualifier relationQualifier
		slot      model.SlotID
		hidden    bool
	}{{target.qualifier, spec.OldRow, false}, {relationQualifier{name: "excluded"}, spec.ExcludedRow, true}} {
		if err := b.chargeNamespace(len(columns), 1); err != nil {
			return err
		}
		ns := relationNamespace{qualifier: image.qualifier, descriptor: target.descriptor, systemColumns: image.qualifier.name != "excluded" && target.descriptor != nil && target.descriptor.Kind() == model.ObjectTable}
		for i, c := range columns {
			field := rowField{row: image.slot, layout: layout, index: uint32(i), nullable: !image.hidden}
			col := column{ResultColumn: ResultColumn{Name: c.Name, Layout: c.Layout, Nullable: c.Nullable}, qualifier: image.qualifier.name, namespace: image.qualifier.namespace, hidden: image.hidden, field: &field}
			local.columns = append(local.columns, col)
			ns.columns = append(ns.columns, col)
		}
		ns.row = &column{ResultColumn: ResultColumn{Slot: image.slot, Layout: layout, Nullable: !image.hidden}, record: recordDeclared, recordFields: ns.columns}
		local.namespaces = append(local.namespaces, ns)
	}
	assignments, updated, effects, err := b.updateAssignments(clause.TargetList, &local, target)
	if err != nil {
		return err
	}
	spec.ConflictAssignments, spec.Effects = assignments, spec.Effects.Union(effects)
	for _, column := range updated {
		if !slices.Contains(spec.UpdatedColumns, column) {
			spec.UpdatedColumns = append(spec.UpdatedColumns, column)
		}
	}
	if clause.WhereClause != nil {
		predicate, err := b.scalar(clause.WhereClause, &local)
		if err != nil {
			return err
		}
		predicate, err = b.coerce(&local, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return err
		}
		spec.ConflictPredicate = predicate.id
		spec.Effects = spec.Effects.Union(predicate.effects)
	}
	return nil
}

type inferenceKey struct {
	column     model.ColumnID
	expression *BoundIndexExpression
	collation  model.CollationID
	class      model.OID
}

func (b *binder) conflictArbiters(target mutationTarget, infer *zpgparse.InferClause) ([]model.ConflictArbiter, error) {
	var constraint *model.ConstraintDescriptor
	if infer != nil && infer.Conname != "" {
		for i := range target.table.Constraints {
			if target.table.Constraints[i].Name == infer.Conname {
				constraint = &target.table.Constraints[i]
				break
			}
		}
		if constraint == nil {
			return nil, bindError("42704", "ON CONFLICT constraint does not exist", 0)
		}
	}
	var keys []inferenceKey
	var predicate *BoundIndexExpression
	if infer != nil && constraint == nil {
		for _, raw := range infer.IndexElems {
			elem := raw.GetIndexElem()
			if elem == nil {
				return nil, bindError("42601", "invalid conflict inference element", 0)
			}
			key := inferenceKey{}
			var layout model.TypeLayout
			if elem.Name != "" {
				col, err := publicIndexColumn(target.table.Columns, elem.Name)
				if err != nil {
					return nil, err
				}
				key.column, layout = col.ID, col.Layout
			} else {
				bound, err := b.inferenceExpression(target, elem.Expr, false)
				if err != nil {
					return nil, err
				}
				key.expression, layout = bound, bound.Result
				root, _ := bound.Graph.Scalar(bound.Root)
				if root.Kind == ir.Collate {
					key.collation = layout.Type.Collation
					bound.Root, _ = bound.Graph.Operand(bound.Root, 0)
					root, _ = bound.Graph.Scalar(bound.Root)
				}
				if root.Kind == ir.Param {
					key.column = bound.Parameters[root.Parameter-1].Column
					key.expression = nil
				}
			}
			if len(elem.Collation) != 0 {
				cb, err := newExpressionBinder(b.ctx, b.catalog, b.options)
				if err != nil {
					return nil, err
				}
				layout, err = cb.indexColumnCollation(layout, elem.Collation)
				if err != nil {
					return nil, err
				}
				key.collation = layout.Type.Collation
			}
			if len(elem.Opclass) != 0 {
				operatorType, err := b.indexOperatorClassType(layout)
				if err != nil {
					return nil, err
				}
				key.class, err = indexOperatorClass(operatorType, elem.Opclass)
				if err != nil {
					return nil, err
				}
			}
			keys = append(keys, key)
		}
		if infer.WhereClause != nil {
			bound, err := b.inferenceExpression(target, infer.WhereClause, true)
			if err != nil {
				return nil, err
			}
			predicate = bound
		}
	}
	result, err := b.inferConflictArbiters(target, infer, constraint, keys, predicate)
	if b.deferArbiterError(err) {
		return nil, nil
	}
	return result, err
}

func (b *binder) inferConflictArbiters(target mutationTarget, infer *zpgparse.InferClause, constraint *model.ConstraintDescriptor, keys []inferenceKey, predicate *BoundIndexExpression) ([]model.ConflictArbiter, error) {
	if constraint != nil {
		if constraint.Kind != model.ConstraintPrimaryKey && constraint.Kind != model.ConstraintUnique {
			return nil, bindError("42809", "ON CONFLICT requires a unique or primary key constraint", 0)
		}
		if constraint.Deferrable {
			return nil, bindError("55000", "ON CONFLICT does not support deferrable arbiters", 0)
		}
	}
	var result []model.ConflictArbiter
	admit := func(d *model.Descriptor, primary bool, metadata *model.IndexMetadata) error {
		if !primary && !metadata.Valid() {
			return nil
		}
		if constraint != nil {
			wantedPrimary := constraint.Kind == model.ConstraintPrimaryKey && constraint.Index == 0
			wrongIndex := !primary && constraint.Index != d.ID()
			if wantedPrimary != primary || wrongIndex {
				return nil
			}
		}
		matches := infer == nil || constraint != nil
		if !matches {
			var err error
			matches, err = b.inferenceMatches(target, keys, predicate, metadata)
			if err != nil {
				return err
			}
		}
		if !matches {
			return nil
		}
		for _, c := range target.table.Constraints {
			belongsToArbiter := primary && c.Kind == model.ConstraintPrimaryKey
			if !primary {
				belongsToArbiter = c.Index == d.ID()
			}
			if c.Deferrable && belongsToArbiter {
				return bindError("55000", "ON CONFLICT does not support deferrable arbiters", 0)
			}
		}
		b.dependency(d, 0)
		for _, key := range metadata.Keys {
			if key.Column != 0 {
				b.dependency(target.descriptor, key.Column)
			}
			if key.Expression != nil {
				if infer == nil || constraint != nil {
					if _, err := b.boundConflictExpression(target, key.Expression, model.TypeLayout{Type: key.Type, Children: key.Children}); err != nil {
						return err
					}
				}
				if err := b.conflictExpressionDependencies(key.Expression); err != nil {
					return err
				}
			}
		}
		if metadata.Predicate != nil {
			if infer == nil || constraint != nil {
				if _, err := b.boundConflictExpression(target, metadata.Predicate, b.primitive(16)); err != nil {
					return err
				}
			}
			if err := b.conflictExpressionDependencies(metadata.Predicate); err != nil {
				return err
			}
		}
		if infer != nil {
			a := model.ConflictArbiter{Primary: primary, Version: d.Version(), Generation: d.Physical().Generation}
			if !primary {
				a.Index = d.ID()
			}
			result = append(result, a)
		}
		return nil
	}
	if len(target.table.PrimaryKey) != 0 {
		if err := admit(target.descriptor, true, &model.IndexMetadata{Keys: target.table.PrimaryKey, Unique: true}); err != nil {
			return nil, err
		}
	}
	indexes := slices.Clone(target.table.Indexes)
	slices.Sort(indexes)
	for _, id := range indexes {
		published, err := b.catalog.Descriptor(b.ctx, id)
		if err != nil {
			return nil, err
		}
		d := published.Descriptor
		if d.State() != model.StatePublic {
			continue
		}
		ds, err := b.descriptorSpec(d)
		if err != nil {
			return nil, err
		}
		if ds.Index == nil || ds.Index.Table != target.descriptor.ID() {
			return nil, bindError("XX001", "invalid table index descriptor", 0)
		}
		if !ds.Index.Unique {
			continue
		}
		if err := admit(d, false, ds.Index); err != nil {
			return nil, err
		}
	}
	if infer != nil && len(result) == 0 {
		return nil, bindError("42P10", "no unique constraint matches ON CONFLICT specification", 0)
	}
	return result, nil
}

func (b *binder) conflictExpressionDependencies(def *model.ExpressionDefinition) error {
	for _, dep := range def.Dependencies {
		p, err := b.catalog.Descriptor(b.ctx, dep.Object)
		if err != nil {
			return err
		}
		b.dependency(p.Descriptor, dep.Column)
	}
	return nil
}

func (b *binder) boundConflictExpression(target mutationTarget, def *model.ExpressionDefinition, layout model.TypeLayout) (*BoundIndexExpression, error) {
	columns := target.table.Columns
	if target.automatic() && target.executionTable != nil {
		columns = target.executionTable.Columns
	}
	layouts := make([]storage.ColumnLayout, 0, len(columns))
	for _, c := range columns {
		if c.State == model.StatePublic {
			layouts = append(layouts, storage.ColumnLayout{ID: c.ID, Type: c.Layout.Type, Children: c.Layout.Children})
		}
	}
	return BindIndexExpression(b.ctx, def, layouts, layout, b.catalog, b.options.Limits)
}

func (b *binder) inferenceMatches(target mutationTarget, keys []inferenceKey, predicate *BoundIndexExpression, index *model.IndexMetadata) (bool, error) {
	matched := make([]bool, len(index.Keys))
	for _, want := range keys {
		found := false
		for i, key := range index.Keys {
			if want.collation != 0 && want.collation != key.Collation {
				continue
			}
			operatorMatches, err := b.inferenceOperatorClassMatches(key, want.class)
			if err != nil {
				return false, err
			}
			if !operatorMatches {
				continue
			}
			equal := want.expression == nil && key.Expression == nil && want.column == key.Column
			if want.expression != nil && key.Expression != nil {
				bound, err := b.boundConflictExpression(target, key.Expression, model.TypeLayout{Type: key.Type, Children: key.Children})
				if err != nil {
					return false, err
				}
				root, _ := bound.Graph.Scalar(bound.Root)
				if root.Kind == ir.Collate {
					bound.Root, _ = bound.Graph.Operand(bound.Root, 0)
				}
				equal, err = b.conflictExpressionEqual(want.expression, want.expression.Root, bound, bound.Root)
				if err != nil {
					return false, err
				}
			}
			if equal {
				found = true
				matched[i] = true
			}
		}
		if !found {
			return false, nil
		}
	}
	for _, found := range matched {
		if !found {
			return false, nil
		}
	}
	if index.Predicate == nil {
		return true, nil
	}
	if predicate == nil {
		return false, nil
	}
	bound, err := b.boundConflictExpression(target, index.Predicate, b.primitive(16))
	if err != nil {
		return false, err
	}
	remaining := b.options.Limits.MaxNodes
	return b.conflictImplies(predicate, predicate.Root, bound, bound.Root, &remaining)
}

func (b *binder) inferenceExpression(target mutationTarget, ast *zpgparse.Node, predicate bool) (*BoundIndexExpression, error) {
	if ast == nil {
		return nil, bindError("42601", "missing inference expression", 0)
	}
	owned := proto.Clone(ast).(*zpgparse.Node)
	if err := rewriteIndexAST(b.ctx, owned.ProtoReflect(), func(node *zpgparse.Node) error {
		ref := node.GetColumnRef()
		if ref == nil {
			return nil
		}
		parts, err := names(ref.Fields)
		if err != nil {
			return err
		}
		if len(parts) > 1 {
			valid := len(parts) == 2 && parts[0] == target.qualifier.name || len(parts) == 3 && target.qualifier.namespace != "" && parts[0] == target.qualifier.namespace && parts[1] == target.qualifier.name
			if !valid {
				return bindError("42P01", "inference expression references another relation", int(ref.Location)+1)
			}
			ref.Fields = ref.Fields[len(ref.Fields)-1:]
		}
		return nil
	}); err != nil {
		return nil, err
	}
	_, bound, err := defineIndexExpression(b.ctx, owned, target.descriptor, target.table.Columns, b.catalog, b.options, predicate)
	return bound, err
}

func (b *binder) inferenceOperatorClassMatches(key model.KeyElement, wanted model.OID) (bool, error) {
	if wanted == 0 {
		return true, nil
	}
	class := key.OperatorClass
	if class == 0 {
		typ, err := b.indexOperatorClassType(model.TypeLayout{Type: key.Type, Children: key.Children})
		if err != nil {
			return false, err
		}
		standard, ok := builtin.LookupDefaultOperatorClass("btree", typ.ID)
		if !ok {
			return false, nil
		}
		class = standard.ID
	}
	actual, actualOK := builtin.LookupOperatorClass(class)
	expected, expectedOK := builtin.LookupOperatorClass(wanted)
	return actualOK && expectedOK && actual.Family == expected.Family && actual.InputType == expected.InputType, nil
}
