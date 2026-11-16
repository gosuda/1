package bind

import (
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) selectStatement(n *zpgparse.SelectStmt, s *scope, destination *insertTargetContext) (relation, error) {
	pushed := slices.Clone(b.pushedRowLocks[n])
	delete(b.pushedRowLocks, n)
	return b.selectStatementWithRowLocks(n, s, destination, pushed)
}

func (b *binder) selectStatementWithRowLocks(n *zpgparse.SelectStmt, s *scope, destination *insertTargetContext, pushed []rowLockPolicy) (relation, error) {
	if err := b.enter(); err != nil {
		return relation{}, err
	}
	defer func() { b.depth-- }()
	if n == nil {
		return relation{}, bindError("42601", "expected SELECT", 0)
	}
	if n.IntoClause != nil {
		return relation{}, missing("SELECT INTO", 0)
	}
	s = selectScope(s)
	s.rowLocks = pushed
	if err := b.prepareRowLockPropagation(n.FromClause, n.LockingClause, pushed); err != nil {
		return relation{}, err
	}
	rowLocking := len(n.LockingClause) != 0 || len(pushed) != 0
	if n.WithClause != nil {
		if err := b.with(n.WithClause, s); err != nil {
			return relation{}, err
		}
	}
	if rowLocking && (n.Op != zpgparse.SetOperation_SETOP_NONE && n.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED || len(n.ValuesLists) != 0 || len(n.DistinctClause) != 0 || len(n.GroupClause) != 0 || n.HavingClause != nil) {
		return relation{}, bindError("0A000", "row locking is not allowed with this query form", 0)
	}
	if n.Op != zpgparse.SetOperation_SETOP_NONE && n.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED {
		return b.setOperation(n, s)
	}
	if len(n.ValuesLists) > 0 {
		rel, err := b.values(n.ValuesLists, s)
		if err != nil {
			return rel, err
		}
		return b.selectTail(n, s, rel)
	}
	var input relation
	for _, node := range n.FromClause {
		if input.id != 0 {
			if sub := lateralSource(node); sub != nil {
				var err error
				input, err = b.lateral(input, sub, &zpgparse.JoinExpr{}, s, ir.InnerJoin)
				if err != nil {
					return input, err
				}
				continue
			}
		}
		rel, err := b.from(node, s)
		if err != nil {
			return rel, err
		}
		if input.id == 0 {
			input = rel
		} else {
			input, err = b.cross(input, rel, s)
			if err != nil {
				return input, err
			}
		}
	}
	if input.id == 0 {
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: 1}})
		if err != nil {
			return input, err
		}
		input.id = id
	}
	s.columns = input.columns
	s.namespaces = input.namespaces
	s.rowLockTargets = input.lockTargets
	if n.WhereClause != nil {
		predicate, err := b.restrictedScalar(n.WhereClause, s, "WHERE")
		if err != nil {
			return input, err
		}
		predicate, err = b.coerce(s, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return input, err
		}
		input, err = b.filter(input, s, predicate)
		if err != nil {
			return input, err
		}
	}
	definitions, err := resolveWindowDefinitions(n.WindowClause)
	if err != nil {
		return input, err
	}
	s.windowDefinitions = definitions
	grouping, err := b.groupingExpressions(n, s, input.columns)
	if err != nil {
		return input, err
	}
	groupNodes, groupExprs := grouping.nodes, grouping.values
	input, err = b.projectGrouping(input, s, groupExprs)
	if err != nil {
		return input, err
	}
	groupColumns := make([]column, len(groupExprs))
	groupBindings := make([]ir.Binding, len(groupExprs))
	groupIDs := make([]ir.ScalarID, len(groupExprs))
	for i := range groupExprs {
		groupExprs[i], err = b.defaultType(s, groupExprs[i])
		if err != nil {
			return input, err
		}
		e := groupExprs[i]
		if err := b.requireKeyCollation(e); err != nil {
			return input, err
		}
		omitted := len(grouping.omitted) != 0 && grouping.omitted[i]
		nullable := e.nullable || omitted
		slot, err := b.slot(s, e.typ, nullable, 0, 0)
		if err != nil {
			return input, err
		}
		groupColumns[i] = column{ResultColumn: ResultColumn{Name: expressionName(groupNodes[i]), Layout: e.typ, Nullable: nullable, Slot: slot}, collation: b.expressionCollation(e), record: e.record, recordFields: projectedRecordFields(e)}
		groupBindings[i] = ir.Binding{Slot: slot, Expr: e.id}
		groupIDs[i] = e.id
	}
	s.groupSlots = make(map[model.SlotID]column)
	for i, expression := range groupExprs {
		if expression.sourceSlot != 0 {
			s.groupSlots[expression.sourceSlot] = groupColumns[i]
		}
	}
	s.groupNodes = groupNodes
	s.groupColumns = groupColumns
	s.allowAggregate = true
	s.allowWindow = true
	var targets []expression
	var columns []column
	var targetNodes []*zpgparse.Node
	for _, node := range n.TargetList {
		target := node.GetResTarget()
		if target == nil {
			return input, bindError("42601", "invalid SELECT target", 0)
		}
		if ref := starReference(target.Val); ref != nil {
			qualifier, err := starQualifier(ref, len(n.FromClause) != 0)
			if err != nil {
				return input, err
			}
			owner := s
			candidates := input.columns
			if qualifier.name != "" {
				owner, err = s.starScope(qualifier, int(ref.Location)+1)
				if err != nil {
					return input, err
				}
				candidates = owner.columns
			}
			found := false
			for _, c := range candidates {
				if !c.visibleToStar(qualifier) {
					continue
				}
				reference := owner.starColumn(s, c)
				e, err := b.reference(s, reference, int(ref.Location)+1)
				if err != nil {
					return input, err
				}
				targets = append(targets, e)
				columns = append(columns, c)
				targetNodes = append(targetNodes, nil)
				found = true
			}
			if !found && qualifier.name != "" && len(owner.namespaces) == 0 {
				return input, bindError("42P01", "star relation does not exist", int(ref.Location)+1)
			}
			continue
		}
		expanded, names, star, err := b.expandCompositeStar(target.Val, s)
		if err != nil {
			return input, err
		}
		if star {
			if err := b.checkStarExpansion(len(targets)+len(expanded), int(target.Location)+1); err != nil {
				return input, err
			}
			indirection := target.Val.GetAIndirection()
			needsIdentity := len(n.SortClause) != 0 || (len(n.DistinctClause) != 0 && n.DistinctClause[0] != nil && n.DistinctClause[0].Node != nil)
			if needsIdentity {
				count := uint64(len(expanded)) * uint64(len(indirection.Indirection))
				if count > uint64(b.options.Limits.MaxOperands) {
					return input, bindError("54000", "star expansion limit exceeded", int(target.Location)+1)
				}
				if err := b.checkStarExpansion(int(count), int(target.Location)+1); err != nil {
					return input, err
				}
			}
			for i, e := range expanded {
				targets = append(targets, e)
				columns = append(columns, column{ResultColumn: ResultColumn{Name: names[i]}})
				var targetNode *zpgparse.Node
				if needsIdentity {
					// ORDER BY must recognize an expanded field as a visible target.
					fields := slices.Clone(indirection.Indirection)
					fields[len(fields)-1] = zpgparse.MakeStrNode(names[i])
					targetNode = &zpgparse.Node{Node: &zpgparse.Node_AIndirection{AIndirection: &zpgparse.A_Indirection{Arg: indirection.Arg, Indirection: fields}}}
				}
				targetNodes = append(targetNodes, targetNode)
			}
			continue
		}
		e, err := b.targetScalar(target.Val, s)
		if err != nil {
			return input, err
		}
		name := targetName(target)
		targets = append(targets, e)
		columns = append(columns, column{ResultColumn: ResultColumn{Name: name}})
		targetNodes = append(targetNodes, target.Val)
	}
	visible := len(targets)
	targets, columns, tailTargets, err := b.tailTargets(n, s, targets, columns, targetNodes)
	if err != nil {
		return input, err
	}
	s.allowWindow = false
	var having expression
	if n.HavingClause != nil {
		having, err = b.restrictedScalar(n.HavingClause, s, "HAVING")
		if err != nil {
			return input, err
		}
		having, err = b.coerce(s, having, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return input, err
		}
	}
	s.allowAggregate = false
	aggregateQuery := s.aggregateSeen || len(s.aggregates) > 0 || len(s.groupings) > 0
	if rowLocking && (aggregateQuery || len(s.windows) != 0) {
		return input, bindError("0A000", "row locking is not allowed with aggregate or window queries", 0)
	}
	if len(n.GroupClause) > 0 || aggregateQuery || n.HavingClause != nil {
		available := append(slices.Clone(groupColumns), s.aggregateColumns...)
		available = append(available, s.groupingColumns...)
		for _, window := range s.windows {
			if err := aggregateAvailable(expression{reads: window.reads}, available, s.parent); err != nil {
				return input, err
			}
			available = append(available, window.column)
		}
		for _, e := range append(slices.Clone(targets), having) {
			if err := aggregateAvailable(e, available, s.parent); err != nil {
				return input, err
			}
		}
		effects := input.effects
		for _, e := range groupExprs {
			effects = effects.Union(e.effects)
		}
		for _, call := range s.aggregates {
			effects = effects.Union(call.Effects)
		}
		aggregateColumns := append(slices.Clone(groupColumns), s.aggregateColumns...)
		aggregateColumns = append(aggregateColumns, s.groupingColumns...)
		groupSets := grouping.sets
		if groupSets == nil {
			groupSets = [][]uint32{nil}
		}
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(aggregateColumns), Bindings: groupBindings, Expressions: groupIDs, GroupingSets: groupSets, Aggregates: s.aggregates, Groupings: s.groupings})
		if err != nil {
			return input, err
		}
		input = relation{id: id, columns: aggregateColumns, effects: effects}
		if having.id != 0 {
			input, err = b.filter(input, s, having)
			if err != nil {
				return input, err
			}
		}
	}
	for _, window := range s.windows {
		input, err = b.projectWindow(input, s, &window)
		if err != nil {
			return input, err
		}
		spec := window.spec
		spec.Inputs = []ir.RelID{input.id}
		spec.Outputs = append(outputSlots(input.columns), window.column.Slot)
		spec.Effects = spec.Effects.Union(input.effects)
		id, err := b.builder.AddRel(b.ctx, spec)
		if err != nil {
			return input, err
		}
		input = relation{id: id, columns: append(slices.Clone(input.columns), window.column), effects: spec.Effects, namespaces: input.namespaces}
	}
	infer, err := destination.inferOutputs(n, targets, visible, tailTargets)
	if err != nil {
		return input, err
	}
	for i, e := range targets {
		if infer != nil && infer[i] {
			e, err = b.coerce(s, e, destination.columns[i].Layout, ir.AssignmentCast)
		} else {
			e, err = b.defaultType(s, e)
		}
		if err != nil {
			return input, err
		}
		targets[i] = e
	}
	input, targets, expandedSet, err := b.projectSetExpressions(input, s, targets)
	if err != nil {
		return input, err
	}
	bindings := make([]ir.Binding, len(targets))
	effects := input.effects
	for i, e := range targets {
		slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
		if err != nil {
			return input, err
		}
		columns[i].Layout = e.typ
		columns[i].record = e.record
		columns[i].recordFields = projectedRecordFields(e)
		if e.typ.Type.ID == 2249 && columns[i].record == recordUnspecified {
			// A record whose declared field names match its child layouts is fully
			// typed, so its fields are addressable. Only a record without a complete
			// declared shape is a bare variable that needs an external definition.
			if len(e.typ.Children) != 0 && len(e.typ.Children) == len(e.typ.FieldNames) {
				columns[i].record = recordDeclared
				columns[i].recordFields = make([]column, len(e.typ.Children))
				for f := range e.typ.Children {
					columns[i].recordFields[f] = column{ResultColumn: ResultColumn{Name: e.typ.FieldNames[f], Layout: e.typ.Children[f], Nullable: true}}
				}
			} else {
				columns[i].record = recordVariable
			}
		}
		columns[i].collation = b.expressionCollation(e)
		columns[i].Nullable = e.nullable
		columns[i].Slot = slot
		columns[i].hidden = i >= visible
		columns[i].qualifier = ""
		columns[i].namespace = ""
		bindings[i] = ir.Binding{Slot: slot, Expr: e.id}
		effects = effects.Union(e.effects)
	}
	if rowLocking && expandedSet {
		return input, bindError("0A000", "row locking is not allowed with set-returning targets", 0)
	}
	if len(n.DistinctClause) == 0 && !expandedSet {
		return b.deferredTail(n, s, input, columns, bindings, targets, tailTargets, visible)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	if err != nil {
		return input, err
	}
	s.tailColumns = make(map[*zpgparse.Node]column, len(tailTargets))
	for _, target := range tailTargets {
		s.tailColumns[target.node] = columns[target.index]
	}
	result, err := b.selectTail(n, s, relation{id: id, columns: columns, effects: effects})
	if err != nil {
		return result, err
	}
	return b.trimTail(result, s, visible)
}
func expressionName(n *zpgparse.Node) string {
	if n == nil {
		return "?column?"
	}
	if c := n.GetColumnRef(); c != nil && len(c.Fields) > 0 {
		return c.Fields[len(c.Fields)-1].GetString_().GetSval()
	}
	if f := n.GetFuncCall(); f != nil && len(f.Funcname) > 0 {
		return f.Funcname[len(f.Funcname)-1].GetString_().GetSval()
	}
	if indirection := n.GetAIndirection(); indirection != nil {
		for i := len(indirection.Indirection) - 1; i >= 0; i-- {
			if field := indirection.Indirection[i].GetString_(); field != nil {
				return field.Sval
			}
		}
		return expressionName(indirection.Arg)
	}
	if value := n.GetSqlvalueFunction(); value != nil {
		return sqlValueFunctionName(value.Op)
	}
	if value := n.GetMinMaxExpr(); value != nil {
		return minMaxName(value.Op)
	}
	if value := n.GetAExpr(); value != nil && value.Kind == zpgparse.A_Expr_Kind_AEXPR_NULLIF {
		return "nullif"
	}
	if n.GetGroupingFunc() != nil {
		return "grouping"
	}
	if n.GetCoalesceExpr() != nil {
		return "coalesce"
	}
	if n.GetCaseExpr() != nil {
		return "case"
	}
	if c := n.GetTypeCast(); c != nil {
		return expressionName(c.Arg)
	}
	return "?column?"
}
func (b *binder) filter(input relation, s *scope, predicate expression) (relation, error) {
	effects := input.effects.Union(predicate.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: s.id, Predicate: predicate.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(input.columns)})
	return relation{id: id, columns: input.columns, effects: effects, namespaces: input.namespaces}, err
}
func (b *binder) values(rows []*zpgparse.Node, s *scope) (relation, error) {
	width := len(rows[0].GetList().GetItems())
	if uint64(len(rows))*uint64(width) > uint64(b.options.Limits.MaxOperands) {
		return relation{}, bindError("54000", "VALUES operand limit exceeded", 0)
	}
	cells := make([][]expression, len(rows))
	for i, row := range rows {
		list := row.GetList()
		if list == nil || len(list.Items) != width {
			return relation{}, bindError("42601", "VALUES lists must all be the same length", 0)
		}
		args, err := b.scalarList(list.Items, s)
		if err != nil {
			return relation{}, err
		}
		cells[i] = args
	}
	columns := make([]column, width)
	for c := range width {
		values := make([]expression, len(rows))
		for r := range cells {
			values[r] = cells[r][c]
		}
		typ, err := b.commonType(values)
		if err != nil {
			return relation{}, err
		}
		nullable := false
		for r := range cells {
			cells[r][c], err = b.coerce(s, cells[r][c], typ, ir.ImplicitCast)
			if err != nil {
				return relation{}, err
			}
			nullable = nullable || cells[r][c].nullable
			values[r] = cells[r][c]
		}
		state, err := b.commonCollation(values)
		if err != nil {
			return relation{}, err
		}
		typ.Type.Collation = state.id
		for r := range cells {
			cells[r][c], err = b.relabelCollation(s, cells[r][c], state.id)
			if err != nil {
				return relation{}, err
			}
		}
		slot, err := b.slot(s, typ, nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[c] = column{ResultColumn: ResultColumn{Name: fmt.Sprintf("column%d", c+1), Layout: typ, Nullable: nullable, Slot: slot}, record: recordVariable}
		if typ.Type.ID == 2249 && values[0].record == recordDeclared {
			declared := true
			for _, value := range values[1:] {
				if value.record != recordDeclared || !sameRecordFields(values[0].recordFields, value.recordFields) {
					declared = false
					break
				}
			}
			if declared {
				columns[c].record, columns[c].recordFields = recordDeclared, values[0].recordFields
			}
		}
	}
	ids := make([]ir.ScalarID, 0, len(rows)*width)
	effects := ir.Effects{}
	for _, row := range cells {
		for _, e := range row {
			ids = append(ids, e.id)
			effects = effects.Union(e.effects)
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: uint64(len(rows)), Effects: effects}, Outputs: outputSlots(columns), Expressions: ids})
	return relation{id: id, columns: columns, effects: effects}, err
}
func (b *binder) selectTail(n *zpgparse.SelectStmt, s *scope, input relation) (relation, error) {
	tail := *s
	tail.groupNodes = nil
	tail.groupColumns = nil
	tail.groupSlots = nil
	s = &tail
	s.columns = input.columns
	var order []ir.OrderKey
	var orderExpressions []expression
	for _, node := range n.SortClause {
		sort := node.GetSortBy()
		if sort == nil {
			return input, bindError("42601", "invalid ORDER BY item", 0)
		}
		expr, err := b.tailExpression(sort.Node, n, s, input.columns)
		if err != nil {
			return input, err
		}
		key, err := b.orderKey(sort, expr)
		if err != nil {
			return input, err
		}
		order = append(order, key)
		orderExpressions = append(orderExpressions, expr)
		input.effects = input.effects.Union(expr.effects)
	}
	if len(n.DistinctClause) > 0 {
		var keys []ir.ScalarID
		var keyExpressions []expression
		if n.DistinctClause[0] != nil && n.DistinctClause[0].Node != nil {
			for _, node := range n.DistinctClause {
				e, err := b.tailExpression(node, n, s, input.columns)
				if err != nil {
					return input, err
				}
				if err := b.requireKeyCollation(e); err != nil {
					return input, err
				}
				keys = append(keys, e.id)
				keyExpressions = append(keyExpressions, e)
				input.effects = input.effects.Union(e.effects)
			}
			var err error
			keys, order, err = b.distinctOrder(keys, order, keyExpressions, orderExpressions)
			if err != nil {
				return input, err
			}
		}
		if len(keys) == 0 {
			for _, c := range input.columns {
				if err := b.requireKeyCollation(expression{typ: c.Layout, collation: c.collation}); err != nil {
					return input, err
				}
			}
		}
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Distinct, Scope: s.id, DistinctOn: len(keys) > 0, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(input.columns), Expressions: keys, Order: order})
		if err != nil {
			return input, err
		}
		input.id = id
	}
	if len(order) > 0 {
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Sort, Scope: s.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(input.columns), Order: order})
		if err != nil {
			return input, err
		}
		input.id = id
	}
	if len(n.LockingClause) != 0 || len(s.rowLocks) != 0 {
		var err error
		input, err = b.lockRows(input, s, n.LockingClause, s.rowLocks)
		if err != nil {
			return input, err
		}
	}
	if n.LimitCount != nil || n.LimitOffset != nil {
		local := *s
		local.columns = nil
		spec := ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: s.id, Effects: input.effects, WithTies: n.LimitOption == zpgparse.LimitOption_LIMIT_OPTION_WITH_TIES}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(input.columns)}
		for _, pair := range []struct {
			node        *zpgparse.Node
			destination *ir.ScalarID
		}{{n.LimitCount, &spec.Count}, {n.LimitOffset, &spec.Offset}} {
			if pair.node == nil {
				continue
			}
			e, err := b.scalar(pair.node, &local)
			if err != nil {
				return input, err
			}
			e, err = b.coerce(&local, e, b.primitive(20), ir.ImplicitCast)
			if err != nil {
				return input, err
			}
			*pair.destination = e.id
			spec.Effects = spec.Effects.Union(e.effects)
		}
		if spec.WithTies {
			spec.Order = order
		}
		spec.Effects.MayError = true
		id, err := b.builder.AddRel(b.ctx, spec)
		if err != nil {
			return input, err
		}
		input.id = id
		input.effects = spec.Effects
	}
	return input, nil
}

func (b *binder) distinctOrder(keys []ir.ScalarID, order []ir.OrderKey, keyExpressions, orderExpressions []expression) ([]ir.ScalarID, []ir.OrderKey, error) {
	if len(order) == 0 {
		return keys, order, nil
	}
	if len(keys) != len(keyExpressions) || len(order) != len(orderExpressions) {
		return nil, nil, bindError("XX000", "DISTINCT ordering identity mismatch", 0)
	}
	slots := make([]model.SlotID, len(keyExpressions))
	for i, expression := range keyExpressions {
		slots[i] = expression.sourceSlot
		if slots[i] == 0 {
			return nil, nil, bindError("XX000", "DISTINCT ON key was not projected", expression.position)
		}
	}
	matched := make([]bool, len(keys))
	prefix := make([]ir.ScalarID, 0, len(keys))
	skipped := false
	remaining := uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands)
	for i, item := range order {
		if err := b.ctx.Err(); err != nil {
			return nil, nil, err
		}
		slot := orderExpressions[i].sourceSlot
		found := false
		for j, key := range slots {
			if remaining == 0 {
				return nil, nil, bindError("54000", "DISTINCT ordering comparison limit exceeded", 0)
			}
			remaining--
			if key == slot {
				matched[j], found = true, true
			}
		}
		if !found {
			skipped = true
			continue
		}
		if skipped {
			return nil, nil, bindError("42P10", "DISTINCT ON expressions must match initial ORDER BY expressions", 0)
		}
		prefix = append(prefix, item.Expr)
	}
	for i, key := range keys {
		if matched[i] {
			continue
		}
		if skipped {
			return nil, nil, bindError("42P10", "DISTINCT ON expressions must match initial ORDER BY expressions", 0)
		}
		e := keyExpressions[i]
		op, err := b.resolveOperator("<", []expression{e, e}, e.position)
		if err != nil {
			return nil, nil, err
		}
		prefix = append(prefix, key)
		order = append(order, ir.OrderKey{Expr: key, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: op.operator, Collation: e.typ.Type.Collation})
	}
	return prefix, order, nil
}

func (b *binder) outputExpression(node *zpgparse.Node, s *scope, columns []column) (expression, error) {
	if constant := node.GetAConst(); constant != nil && constant.GetIval() != nil {
		ordinal := int(constant.GetIval().Ival)
		if ordinal < 1 || ordinal > len(columns) {
			return expression{}, bindError("42P10", "ORDER BY position is not in select list", int(constant.Location)+1)
		}
		return b.targetReference(s, columns[ordinal-1], int(constant.Location)+1)
	}
	return b.scalar(node, s)
}

func aggregateAvailable(e expression, columns []column, parent *scope) error {
	available := outputSlots(columns)
	for p := parent; p != nil; p = p.parent {
		available = append(available, outputSlots(p.columns)...)
	}
	for _, slot := range e.reads {
		if !slices.Contains(available, slot) {
			return bindError("42803", "column must appear in GROUP BY or be used in an aggregate", e.position)
		}
	}
	return nil
}
