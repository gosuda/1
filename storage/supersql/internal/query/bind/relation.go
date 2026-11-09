package bind

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) resolve(parts []string, kinds []model.ObjectKind, code string, pos int) (*model.Descriptor, string, error) {
	descriptor, namespace, err := resolveNamespaceObject(b.ctx, b.catalog, b.options.SearchPath, parts, kinds, code, pos)
	if err != nil {
		return nil, "", err
	}
	b.dependency(namespace, 0)
	b.dependency(descriptor, 0)
	return descriptor, namespace.Name().Name, nil
}

func resolveNamespaceObject(ctx context.Context, snapshot CatalogSnapshot, path, parts []string, kinds []model.ObjectKind, code string, pos int) (*model.Descriptor, *model.Descriptor, error) {
	if len(parts) == 0 || len(parts) > 2 || snapshot == nil {
		return nil, nil, bindError(code, "object does not exist", pos)
	}
	if len(parts) == 2 {
		path = []string{parts[0]}
	}
	for _, schema := range path {
		if schema == "pg_catalog" {
			for _, kind := range kinds {
				if kind == model.ObjectTable || kind == model.ObjectView || kind == model.ObjectMaterializedView {
					if err := ValidateBuiltinRelation("pg_catalog."+parts[len(parts)-1], pos); err != nil {
						return nil, nil, err
					}
					break
				}
			}
		}
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for _, kind := range kinds {
			p, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: parts[len(parts)-1]})
			if isMissingObject(err) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if p.Descriptor.State() != model.StatePublic {
				continue
			}
			return p.Descriptor, namespace.Descriptor, nil
		}
	}
	return nil, nil, bindError(code, "object does not exist", pos)
}
func (b *binder) from(node *zpgparse.Node, s *scope) (relation, error) {
	if err := b.enter(); err != nil {
		return relation{}, err
	}
	defer func() { b.depth-- }()
	if n := node.GetRangeVar(); n != nil {
		return b.rangeVar(n, s)
	}
	if n := node.GetRangeSubselect(); n != nil {
		isolated := &scope{id: s.id, parent: s.parent, ctes: s.ctes}
		if n.Subquery.GetSelectStmt() == nil {
			return relation{}, bindError("42601", "FROM subquery must be SELECT", 0)
		}
		rel, err := b.selectStatement(n.Subquery.GetSelectStmt(), isolated, nil)
		if err != nil {
			return rel, err
		}
		rel, err = b.aliasRelation(rel, s, n.Alias, "")
		if err != nil {
			return rel, err
		}
		for i := range rel.namespaces {
			rel.namespaces[i].subquery = true
		}
		rel.lockTargets = slices.Clone(rel.namespaces)
		return rel, nil
	}
	if n := node.GetRangeTableSample(); n != nil {
		return b.tableSample(n, s)
	}
	if n := node.GetRangeFunction(); n != nil {
		return b.rangeFunction(n, s)
	}
	if n := node.GetJsonTable(); n != nil {
		return b.jsonTable(n, s)
	}
	if n := node.GetJoinExpr(); n != nil {
		return b.join(n, s)
	}
	return relation{}, missing("FROM relation form", 0)
}
func (b *binder) tableSample(sample *zpgparse.RangeTableSample, s *scope) (relation, error) {
	position := int(sample.Location) + 1
	if sample.Relation == nil || sample.Relation.GetRangeVar() == nil || len(sample.Method) != 1 || sample.Method[0].GetString_() == nil {
		return relation{}, bindError("42601", "invalid TABLESAMPLE relation", position)
	}
	var method ir.SampleMethod
	switch strings.ToLower(sample.Method[0].GetString_().Sval) {
	case "bernoulli":
		method = ir.BernoulliSample
	case "system":
		method = ir.SystemSample
	default:
		return relation{}, bindError("42704", "tablesample method does not exist", position)
	}
	if len(sample.Args) != 1 {
		return relation{}, bindError("42601", "TABLESAMPLE requires one percentage argument", position)
	}
	input, err := b.from(sample.Relation, s)
	if err != nil {
		return input, err
	}
	local := *s
	local.columns, local.namespaces = nil, nil
	expressions := make([]ir.ScalarID, 0, 2)
	effects := input.effects
	for _, node := range []*zpgparse.Node{sample.Args[0], sample.Repeatable} {
		if node == nil {
			continue
		}
		value, err := b.scalar(node, &local)
		if err != nil {
			return input, err
		}
		value, err = b.coerce(&local, value, b.primitive(701), ir.ImplicitCast)
		if err != nil {
			return input, err
		}
		expressions = append(expressions, value.id)
		effects = effects.Union(value.effects)
	}
	if sample.Repeatable == nil {
		effects.Volatility = ir.Volatile
	}
	effects.MayError = true
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{
		Rel: ir.Rel{Kind: ir.Sample, Scope: s.id, Effects: effects, SampleMethod: method, SampleRepeatable: sample.Repeatable != nil},
		Inputs: []ir.RelID{input.id}, Outputs: outputSlots(input.columns), Expressions: expressions,
	})
	if err != nil {
		return input, err
	}
	input.id, input.effects = id, effects
	return input, nil
}


func (b *binder) cteConsumer(cte cteBinding, alias *zpgparse.Alias, name string, s *scope) (relation, error) {
	if cte.noReturning {
		return relation{}, bindError("0A000", "WITH query \""+name+"\" does not have a RETURNING clause", 0)
	}
	columns := slices.Clone(cte.columns)
	for i := range columns {
		slot, err := b.slot(s, columns[i].Layout, columns[i].Nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i].Slot = slot
	}
	columns, err := aliasOwnedColumns(columns, alias, name)
	if err != nil {
		return relation{}, err
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: s.id, CTE: cte.id, RecursiveReference: cte.recursive, Effects: cte.effects}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	return b.nameRelation(relation{id: id, columns: columns, effects: cte.effects}, s, relationQualifier{name: columnsQualifier(alias, name)}, nil)
}
func (b *binder) view(d *model.Descriptor, view *model.ViewMetadata, alias *zpgparse.Alias, s *scope, pos int, pushed []rowLockPolicy) (relation, error) {
	if b.views[d.ID()] {
		return relation{}, bindError("42P17", "recursive view definition", pos)
	}
	if view.Query.SemanticVersion != 1 || view.Query.SQL != "" || len(view.Query.Encoding) == 0 || uint64(len(view.Query.Encoding)) > b.options.Limits.MaxBytes {
		return relation{}, missing("view semantic definition encoding", pos)
	}
	for _, dep := range view.Query.Dependencies {
		p, err := b.catalog.Descriptor(b.ctx, dep.Object)
		if err != nil {
			return relation{}, err
		}
		if p.Descriptor.Version() != dep.Version {
			return relation{}, bindError("0A000", "view dependency version changed", pos)
		}
		b.dependency(p.Descriptor, dep.Column)
	}
	parsed := &zpgparse.RawStmt{}
	if err := proto.Unmarshal(view.Query.Encoding, parsed); err != nil || parsed.Stmt == nil {
		return relation{}, model.ErrInvalidDescriptor
	}
	if parsed.Stmt.GetSelectStmt() == nil {
		return relation{}, bindError("42601", "view definition must be one SELECT", pos)
	}
	if err := ValidateStatementFeatures(b.ctx, parsed.Stmt); err != nil {
		return relation{}, err
	}
	remaining, bytes := b.options.Limits.MaxNodes, b.options.Limits.MaxBytes
	if err := b.checkAST(parsed.Stmt.ProtoReflect(), 0, &remaining, &bytes); err != nil {
		return relation{}, err
	}
	b.views[d.ID()] = true
	defer delete(b.views, d.ID())
	isolated := &scope{id: s.id, parent: s.parent, ctes: make(map[string]cteBinding)}
	rel, err := b.selectStatementWithRowLocks(parsed.Stmt.GetSelectStmt(), isolated, nil, pushed)
	if err != nil {
		return rel, err
	}
	if len(rel.columns) < len(view.Columns) || len(rel.columns) != len(view.Columns) && b.mutationProvenance == 0 {
		return rel, bindError("42804", "view result differs from descriptor", pos)
	}
	for i, c := range view.Columns {
		if !sameType(c.Layout, rel.columns[i].Layout) {
			return rel, bindError("42804", "view type differs from descriptor", pos)
		}
		rel.columns[i].Name = c.Name
	}
	columns := slices.Clone(rel.columns)
	bindings := make([]ir.Binding, len(columns))
	for i := range columns {
		source := columns[i]
		value, err := b.addScalar(s, expression{typ: source.Layout, nullable: source.Nullable, sourceSlot: source.Slot}, ir.Scalar{Kind: ir.Slot, Slot: source.Slot}, nil)
		if err != nil {
			return rel, err
		}
		if i < len(view.Columns) {
			slot, err := b.slot(s, source.Layout, source.Nullable, d.ID(), view.Columns[i].ID)
			if err != nil {
				return rel, err
			}
			columns[i].Slot = slot
		} else {
			columns[i].hidden = true
			columns[i].internal = true
		}
		bindings[i] = ir.Binding{Slot: columns[i].Slot, Expr: value.id}
	}
	effects := rel.effects
	rel.id, err = b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects, SecurityBarrier: view.SecurityBarrier}, Inputs: []ir.RelID{rel.id}, Outputs: outputSlots(columns), Bindings: bindings})
	if err != nil {
		return rel, err
	}
	rel.columns = columns
	rel.effects = effects
	rel.columns, err = aliasColumns(rel.columns, alias, d.Name().Name)
	return rel, err
}
func (b *binder) cross(left, right relation, s *scope) (relation, error) {
	if err := disjointRelations(left, right); err != nil {
		return relation{}, err
	}
	columns := append(slices.Clone(left.columns), right.columns...)
	effects := left.effects.Union(right.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: s.id, JoinKind: ir.CrossJoin, Effects: effects}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	namespaces, err := b.joinNamespaces(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	lockTargets, err := b.joinLockTargets(left, right, columns)
	return relation{id: id, columns: columns, effects: effects, lockTargets: lockTargets, namespaces: namespaces}, err
}
func (b *binder) join(n *zpgparse.JoinExpr, s *scope) (relation, error) {
	left, err := b.from(n.Larg, s)
	if err != nil {
		return left, err
	}
	kind := ir.InnerJoin
	switch n.Jointype {
	case zpgparse.JoinType_JOIN_LEFT:
		kind = ir.LeftJoin
	case zpgparse.JoinType_JOIN_RIGHT:
		kind = ir.RightJoin
	case zpgparse.JoinType_JOIN_FULL:
		kind = ir.FullJoin
	case zpgparse.JoinType_JOIN_INNER:
	default:
		return relation{}, missing("join kind", 0)
	}
	var right relation
	if sub := lateralSource(n.Rarg); sub != nil {
		if kind == ir.InnerJoin || kind == ir.LeftJoin {
			return b.lateral(left, sub, n, s, kind)
		}
		right, err = b.outerLateralSource(sub, s, left)
	} else {
		right, err = b.from(n.Rarg, s)
	}
	if err != nil {
		return right, err
	}
	if err := disjointRelations(left, right); err != nil {
		return relation{}, err
	}
	inputColumns := append(slices.Clone(left.columns), right.columns...)
	local := *s
	local.columns = inputColumns
	local.namespaces = append(slices.Clone(left.namespaces), right.namespaces...)
	using, err := joinUsingNames(n, left.columns, right.columns)
	if err != nil {
		return relation{}, err
	}
	var predicate expression
	if n.Quals != nil {
		predicate, err = b.scalar(n.Quals, &local)
		if err != nil {
			return relation{}, err
		}
		predicate, err = b.coerce(&local, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
	}
	predicate, err = b.usingPredicate(&local, left.columns, right.columns, using, predicate)
	if err != nil {
		return relation{}, err
	}
	if predicate.id == 0 {
		if kind == ir.InnerJoin {
			kind = ir.CrossJoin
		} else {
			predicate, err = b.addScalar(&local, expression{typ: b.primitive(16), value: model.BoolVal(true)}, ir.Scalar{Kind: ir.Const}, nil)
			if err != nil {
				return relation{}, err
			}
		}
	}
	columns := slices.Clone(inputColumns)
	for i := range columns {
		nullable := columns[i].Nullable || i < len(left.columns) && (kind == ir.RightJoin || kind == ir.FullJoin) || i >= len(left.columns) && (kind == ir.LeftJoin || kind == ir.FullJoin)
		if nullable != columns[i].Nullable {
			slot, err := b.slot(s, columns[i].Layout, nullable, 0, 0)
			if err != nil {
				return relation{}, err
			}
			columns[i].Slot = slot
			columns[i].Nullable = nullable
		}
	}
	effects := left.effects.Union(right.effects).Union(predicate.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: s.id, JoinKind: kind, Predicate: predicate.id, Effects: effects}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	namespaces, err := b.joinNamespaces(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	lockTargets, err := b.joinLockTargets(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	rel := relation{id: id, columns: columns, effects: effects, lockTargets: lockTargets, namespaces: namespaces}
	if len(using) > 0 {
		return b.mergeUsing(rel, len(left.columns), using, kind, n.Alias, n.JoinUsingAlias, s)
	}
	return b.aliasJoinedRelation(rel, n.Alias, s)
}
func (b *binder) lateral(left relation, sub *zpgparse.RangeSubselect, join *zpgparse.JoinExpr, s *scope, kind ir.JoinKind) (relation, error) {
	if kind != ir.InnerJoin && kind != ir.LeftJoin {
		return relation{}, bindError("42P10", "LATERAL requires INNER or LEFT join", 0)
	}
	parent := *s
	parent.columns = left.columns
	parent.namespaces = left.namespaces
	child, err := b.newScope(&parent)
	if err != nil {
		return relation{}, err
	}
	right, err := b.selectStatement(sub.Subquery.GetSelectStmt(), child, nil)
	if err != nil {
		return relation{}, err
	}
	child, right, err = b.shareLateralBody(&parent, child, right)
	if err != nil {
		return relation{}, err
	}
	right, err = b.aliasRelation(right, child, sub.Alias, "")
	if err != nil {
		return relation{}, err
	}
	for i := range right.namespaces {
		right.namespaces[i].subquery = true
	}
	right.lockTargets = slices.Clone(right.namespaces)
	if err := disjointRelations(left, right); err != nil {
		return relation{}, err
	}
	using, err := joinUsingNames(join, left.columns, right.columns)
	if err != nil {
		return relation{}, err
	}
	child.columns = append(slices.Clone(left.columns), right.columns...)
	child.namespaces = append(slices.Clone(left.namespaces), right.namespaces...)
	var predicate expression
	if join.Quals != nil {
		predicate, err = b.scalar(join.Quals, child)
		if err != nil {
			return relation{}, err
		}
		predicate, err = b.coerce(child, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
	}
	predicate, err = b.usingPredicate(child, left.columns, right.columns, using, predicate)
	if err != nil {
		return relation{}, err
	}
	if predicate.id != 0 {
		right, err = b.filter(right, child, predicate)
		if err != nil {
			return relation{}, err
		}
	}
	columns := slices.Clone(left.columns)
	for _, c := range right.columns {
		c.Nullable = c.Nullable || kind == ir.LeftJoin
		slot, err := b.slot(s, c.Layout, c.Nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		c.Slot = slot
		columns = append(columns, c)
	}
	effects := left.effects.Union(right.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: s.id, ApplyKind: ir.LateralApply, LateralOuter: kind == ir.LeftJoin, Effects: effects}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	namespaces, err := b.joinNamespaces(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	lockTargets, err := b.joinLockTargets(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	rel := relation{id: id, columns: columns, effects: effects, lockTargets: lockTargets, namespaces: namespaces}
	if len(using) != 0 {
		return b.mergeUsing(rel, len(left.columns), using, kind, join.Alias, join.JoinUsingAlias, s)
	}
	return b.aliasJoinedRelation(rel, join.Alias, s)
}

func (b *binder) shareLateralBody(parent, child *scope, body relation) (*scope, relation, error) {
	for _, slot := range b.captures[child.id] {
		if b.slotScopes[slot] == parent.id {
			return child, body, nil
		}
	}
	columns := slices.Clone(body.columns)
	for i := range columns {
		slot, err := b.slot(parent, columns[i].Layout, columns[i].Nullable, 0, 0)
		if err != nil {
			return nil, relation{}, err
		}
		columns[i].Slot = slot
	}
	id, err := b.builder.DeclareCTE(b.ctx, ir.CTESpec{Scope: parent.id, Outputs: outputSlots(columns), Materialization: ir.MaterializeAlways, Effects: body.effects})
	if err != nil {
		return nil, relation{}, err
	}
	if err := b.builder.BindCTE(b.ctx, id, body.id); err != nil {
		return nil, relation{}, err
	}
	// Keep per-left ON captures out of the independently owned body scope.
	consumerScope, err := b.newScope(parent)
	if err != nil {
		return nil, relation{}, err
	}
	consumer, err := b.cteConsumer(cteBinding{id: id, columns: columns, effects: body.effects}, nil, "", consumerScope)
	return consumerScope, consumer, err
}
