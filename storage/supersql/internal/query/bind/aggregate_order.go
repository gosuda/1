package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) orderedSetArguments(n *zpgparse.FuncCall, s *scope, pos int) (callArguments, int, error) {
	if len(n.Args)+len(n.AggOrder) > builtin.MaxCallArguments {
		return callArguments{}, 0, bindError("54023", "cannot pass more than 100 arguments to a function", pos)
	}
	directScope := *s
	directScope.allowAggregate, directScope.allowWindow = false, false
	directScope.inAggregate = false
	direct, err := b.callArguments(n.Args, &directScope, pos)
	if err != nil {
		return callArguments{}, 0, err
	}
	if direct.names != nil {
		return callArguments{}, 0, bindError("42601", "named arguments are not allowed with WITHIN GROUP", pos)
	}
	for _, value := range direct.values {
		if err := aggregateAvailable(value, s.groupColumns, s.parent); err != nil {
			return callArguments{}, 0, err
		}
	}
	orderScope := *s
	orderScope.allowAggregate, orderScope.allowWindow = false, false
	orderScope.inAggregate = true
	values := make([]expression, len(direct.values), len(direct.values)+len(n.AggOrder))
	copy(values, direct.values)
	for _, node := range n.AggOrder {
		sort := node.GetSortBy()
		if sort == nil || sort.Node == nil {
			return callArguments{}, 0, bindError("42601", "invalid WITHIN GROUP ORDER BY item", pos)
		}
		value, err := b.scalar(sort.Node, &orderScope)
		if err != nil {
			return callArguments{}, 0, err
		}
		values = append(values, value)
	}
	return callArguments{values: values}, len(direct.values), nil
}

func (b *binder) orderedSetOrder(n *zpgparse.FuncCall, args []expression) ([]ir.OrderKey, ir.Effects, error) {
	order := make([]ir.OrderKey, len(n.AggOrder))
	var effects ir.Effects
	for i, node := range n.AggOrder {
		sort := node.GetSortBy()
		if sort == nil || sort.Node == nil {
			return nil, effects, bindError("42601", "invalid WITHIN GROUP ORDER BY item", int(n.Location)+1)
		}
		key, err := b.orderKey(sort, args[i])
		if err != nil {
			return nil, effects, err
		}
		order[i] = key
		effects = effects.Union(args[i].effects)
	}
	return order, effects, nil
}
func (b *binder) aggregateOrder(n *zpgparse.FuncCall, s *scope, args []expression) ([]ir.OrderKey, ir.Effects, error) {
	if len(n.AggOrder) == 0 {
		return nil, ir.Effects{}, nil
	}
	order := make([]ir.OrderKey, 0, len(n.AggOrder))
	nodes := make([]*zpgparse.Node, len(n.Args), len(n.Args)+len(n.AggOrder))
	copy(nodes, n.Args)
	targets := make([]expression, len(args), len(args)+len(n.AggOrder))
	copy(targets, args)
	work := uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands)
	var effects ir.Effects
	for _, node := range n.AggOrder {
		if err := b.tailComparison(&work); err != nil {
			return nil, effects, err
		}
		sort := node.GetSortBy()
		if sort == nil || sort.Node == nil {
			return nil, effects, bindError("42601", "invalid aggregate ORDER BY item", int(n.Location)+1)
		}
		value, matched, err := b.aggregateOrderExpression(sort.Node, s, nodes, targets, &work)
		if err != nil {
			return nil, effects, err
		}
		if n.AggDistinct && !matched {
			return nil, effects, bindError("42P10", "in an aggregate with DISTINCT, ORDER BY expressions must appear in argument list", value.position)
		}
		key, err := b.orderKey(sort, value)
		if err != nil {
			return nil, effects, err
		}
		order = append(order, key)
		effects = effects.Union(value.effects)
		if !matched {
			nodes = append(nodes, sort.Node)
			targets = append(targets, value)
		}
	}
	return order, effects, nil
}

func (b *binder) aggregateOrderExpression(node *zpgparse.Node, s *scope, nodes []*zpgparse.Node, targets []expression, work *uint64) (expression, bool, error) {
	// Match before binding so volatile calls and Once operands retain the
	// aggregate input target's identity, including repeated sort targets.
	for i, target := range nodes {
		if err := b.tailComparison(work); err != nil {
			return expression{}, false, err
		}
		if target != nil && equivalentScopedAST(node.ProtoReflect(), target.ProtoReflect(), s) {
			return targets[i], true, nil
		}
	}
	value, err := b.scalar(node, s)
	if err != nil {
		return expression{}, false, err
	}
	value, err = b.defaultType(s, value)
	if err != nil {
		return expression{}, false, err
	}
	index, err := b.tailSourceTarget(value, targets, work)
	if err != nil {
		return expression{}, false, err
	}
	if index >= 0 {
		return targets[index], true, nil
	}
	return value, false, nil
}

type orderKeyBinding struct {
	key    ir.OrderKey
	family model.OID
	input  model.TypeID
	layout model.TypeLayout
}

func (b *binder) orderKey(sort *zpgparse.SortBy, value expression) (ir.OrderKey, error) {
	bound, err := b.bindOrderKey(sort, value)
	return bound.key, err
}

func (b *binder) bindOrderKey(sort *zpgparse.SortBy, value expression) (orderKeyBinding, error) {
	if err := b.requireKeyCollation(value); err != nil {
		return orderKeyBinding{}, err
	}
	position := int(sort.Location) + 1
	if position <= 0 {
		position = value.position
	}
	direction := ir.Ascending
	nulls := ir.NullsLast
	opName := "<"
	if sort.SortbyDir == zpgparse.SortByDir_SORTBY_DESC {
		direction = ir.Descending
		nulls = ir.NullsFirst
		opName = ">"
	}
	if sort.SortbyDir == zpgparse.SortByDir_SORTBY_USING {
		parts, err := names(sort.UseOp)
		if err != nil {
			return orderKeyBinding{}, err
		}
		if len(parts) == 0 {
			return orderKeyBinding{}, bindError("42601", "missing sort operator", position)
		}
		if len(parts) > 2 || (len(parts) == 2 && parts[0] != "pg_catalog") {
			return orderKeyBinding{}, missing("qualified custom ordering operator", position)
		}
		opName = parts[len(parts)-1]
	}
	op, err := b.resolveOperator(opName, []expression{value, value}, position)
	if err != nil {
		return orderKeyBinding{}, err
	}
	var family model.OID
	var input model.TypeID
	for interpretation := range builtin.RowComparisonInterpretations(op.operator) {
		if interpretation.CompareFunction == 0 || interpretation.Left != op.signature.Arguments[0] || interpretation.Right != op.signature.Arguments[1] {
			continue
		}
		if interpretation.Strategy != 1 && interpretation.Strategy != 5 {
			continue
		}
		if family != 0 {
			return orderKeyBinding{}, bindError("42809", "operator is not a valid ordering operator", position)
		}
		family, input = interpretation.Family, interpretation.Left
		if sort.SortbyDir == zpgparse.SortByDir_SORTBY_USING {
			if interpretation.Strategy == 1 {
				direction, nulls = ir.Ascending, ir.NullsLast
			} else {
				direction, nulls = ir.Descending, ir.NullsFirst
			}
		}
	}
	if family == 0 {
		return orderKeyBinding{}, bindError("42809", "operator is not a valid ordering operator", position)
	}
	if sort.SortbyNulls == zpgparse.SortByNulls_SORTBY_NULLS_FIRST {
		nulls = ir.NullsFirst
	}
	if sort.SortbyNulls == zpgparse.SortByNulls_SORTBY_NULLS_LAST {
		nulls = ir.NullsLast
	}
	key := ir.OrderKey{Expr: value.id, Direction: direction, Nulls: nulls, Operator: op.operator, Collation: value.typ.Type.Collation}
	return orderKeyBinding{key: key, family: family, input: input, layout: value.typ}, nil
}
