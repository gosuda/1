package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func joinUsingNames(join *zpgparse.JoinExpr, left, right []column) ([]string, error) {
	using, err := names(join.UsingClause)
	if err != nil {
		return nil, err
	}
	if join.JoinUsingAlias != nil && len(using) == 0 {
		return nil, bindError("42601", "USING alias requires a USING clause", 0)
	}
	for i, name := range using {
		if slices.Contains(using[:i], name) {
			return nil, bindError("42701", "column appears more than once in USING", 0)
		}
	}
	if !join.IsNatural {
		return using, nil
	}
	if len(using) != 0 {
		return nil, bindError("42601", "NATURAL join cannot have a USING clause", 0)
	}
	for _, l := range left {
		if l.hidden {
			continue
		}
		for _, r := range right {
			if !r.hidden && l.Name == r.Name && !slices.Contains(using, l.Name) {
				using = append(using, l.Name)
			}
		}
	}
	return using, nil
}

func (b *binder) usingPredicate(s *scope, left, right []column, using []string, predicate expression) (expression, error) {
	leftScope, rightScope := scope{columns: left}, scope{columns: right}
	for _, name := range using {
		l, err := leftScope.lookup([]string{name}, 0)
		if err != nil {
			return expression{}, err
		}
		r, err := rightScope.lookup([]string{name}, 0)
		if err != nil {
			return expression{}, err
		}
		le, err := b.reference(s, l, 0)
		if err != nil {
			return expression{}, err
		}
		re, err := b.reference(s, r, 0)
		if err != nil {
			return expression{}, err
		}
		equal, err := b.operator(s, "=", []expression{le, re}, 0)
		if err != nil {
			return expression{}, err
		}
		if predicate.id == 0 {
			predicate = equal
			continue
		}
		predicate, err = b.boolean(s, ir.And, []expression{predicate, equal}, 0)
		if err != nil {
			return expression{}, err
		}
	}
	return predicate, nil
}

func (b *binder) mergedUsingValue(s *scope, left, right column, kind ir.JoinKind) (expression, error) {
	l, err := b.reference(s, left, 0)
	if err != nil {
		return expression{}, err
	}
	r, err := b.reference(s, right, 0)
	if err != nil {
		return expression{}, err
	}
	typ, err := b.commonType([]expression{l, r})
	if err != nil {
		return expression{}, err
	}
	originalLeft, originalRight := l.id, r.id
	l, err = b.coerceCommon(s, l, typ)
	if err != nil {
		return expression{}, err
	}
	r, err = b.coerceCommon(s, r, typ)
	if err != nil {
		return expression{}, err
	}
	switch kind {
	case ir.RightJoin:
		return r, nil
	case ir.FullJoin:
		return b.addScalar(s, expression{typ: typ, nullable: l.nullable && r.nullable}, ir.Scalar{Kind: ir.Coalesce}, []expression{l, r})
	case ir.InnerJoin:
		if l.id != originalLeft && r.id == originalRight {
			return r, nil
		}
	}
	return l, nil
}

func (b *binder) aliasJoinedRelation(rel relation, alias *zpgparse.Alias, s *scope) (relation, error) {
	if alias == nil {
		return rel, nil
	}
	count := 0
	for _, c := range rel.columns {
		if !c.hidden {
			count++
		}
	}
	if count == len(rel.columns) {
		return b.aliasRelation(rel, s, alias, "")
	}
	visible := make([]column, 0, count)
	for _, c := range rel.columns {
		if !c.hidden {
			visible = append(visible, c)
		}
	}
	columns, err := aliasOwnedColumns(visible, alias, "")
	if err != nil {
		return relation{}, err
	}
	bindings := make([]ir.Binding, len(visible))
	for i, c := range visible {
		e, err := b.reference(s, c, 0)
		if err != nil {
			return relation{}, err
		}
		bindings[i] = ir.Binding{Slot: columns[i].Slot, Expr: e.id}
		rel.effects = rel.effects.Union(e.effects)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: rel.effects}, Inputs: []ir.RelID{rel.id}, Outputs: outputSlots(columns), Bindings: bindings})
	if err != nil {
		return relation{}, err
	}
	return b.nameRelation(relation{id: id, columns: columns, effects: rel.effects, lockTargets: rel.lockTargets}, s, relationQualifier{name: alias.Aliasname}, nil)
}

func (b *binder) mergeUsing(rel relation, leftWidth int, using []string, kind ir.JoinKind, alias, usingAlias *zpgparse.Alias, s *scope) (relation, error) {
	if usingAlias != nil {
		if usingAlias.Aliasname == "" || len(usingAlias.Colnames) != 0 {
			return relation{}, bindError("42601", "invalid USING alias", 0)
		}
		for _, c := range rel.columns {
			if c.qualifier == usingAlias.Aliasname {
				return relation{}, bindError("42712", "table name specified more than once", 0)
			}
		}
		for _, n := range rel.namespaces {
			if n.qualifier.name == usingAlias.Aliasname {
				return relation{}, bindError("42712", "table name specified more than once", 0)
			}
		}
	}
	merged := make([]column, 0, len(rel.columns)+len(using))
	bindings := make([]ir.Binding, 0, len(rel.columns)+len(using))
	for _, name := range using {
		li, ri := -1, -1
		for i, c := range rel.columns {
			if c.Name != name || c.hidden {
				continue
			}
			if i < leftWidth {
				li = i
			} else {
				ri = i
			}
		}
		if li < 0 || ri < 0 {
			return relation{}, bindError("42703", "USING column does not exist", 0)
		}
		e, err := b.mergedUsingValue(s, rel.columns[li], rel.columns[ri], kind)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		c := column{ResultColumn: ResultColumn{Name: name, Layout: e.typ, Nullable: e.nullable, Slot: slot}, collation: b.expressionCollation(e)}
		if usingAlias != nil {
			c.qualifier = usingAlias.Aliasname
		}
		merged = append(merged, c)
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: e.id})
		rel.effects = rel.effects.Union(e.effects)
		rel.columns[li].hidden = true
		rel.columns[ri].hidden = true
	}
	for _, c := range rel.columns {
		e, err := b.reference(s, c, 0)
		if err != nil {
			return relation{}, err
		}
		merged = append(merged, c)
		bindings = append(bindings, ir.Binding{Slot: c.Slot, Expr: e.id})
		rel.effects = rel.effects.Union(e.effects)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: rel.effects}, Inputs: []ir.RelID{rel.id}, Outputs: outputSlots(merged), Bindings: bindings})
	if err != nil {
		return relation{}, err
	}
	result := relation{id: id, columns: merged, effects: rel.effects, lockTargets: rel.lockTargets, namespaces: rel.namespaces}
	if usingAlias != nil {
		if err := b.chargeNamespace(len(using), len(result.namespaces)+1); err != nil {
			return relation{}, err
		}
		namespaces := result.namespaces
		result, err = b.bindNamespace(result, s, relationNamespace{qualifier: relationQualifier{name: usingAlias.Aliasname}, columns: slices.Clone(merged[:len(using)])})
		if err != nil {
			return relation{}, err
		}
		result.namespaces = append(slices.Clone(namespaces), result.namespaces...)
	}
	return b.aliasJoinedRelation(result, alias, s)
}
