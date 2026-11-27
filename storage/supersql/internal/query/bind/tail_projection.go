package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type tailTarget struct {
	node  *zpgparse.Node
	index int
}

// Tail expressions bind while aggregate/window collection is still open. Hidden
// targets survive sorting and LIMIT, but never become result columns.
func (b *binder) tailTargets(n *zpgparse.SelectStmt, s *scope, targets []expression, columns []column, targetNodes []*zpgparse.Node) ([]expression, []column, []tailTarget, error) {
	visible := len(targets)
	var keys []tailTarget
	var nodes []*zpgparse.Node
	for _, item := range n.SortClause {
		sort := item.GetSortBy()
		if sort == nil {
			return nil, nil, nil, bindError("42601", "invalid ORDER BY item", 0)
		}
		nodes = append(nodes, sort.Node)
	}
	for _, node := range n.DistinctClause {
		if node != nil && node.Node != nil {
			nodes = append(nodes, node)
		}
	}
	work := uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands)
	for _, node := range nodes {
		if err := b.ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		index, err := b.tailTargetIndex(node, targets[:visible], columns[:visible], targetNodes, &work)
		if err != nil {
			return nil, nil, nil, err
		}
		if index < 0 {
			for _, key := range keys {
				if err := b.tailComparison(&work); err != nil {
					return nil, nil, nil, err
				}
				if equivalentAST(node.ProtoReflect(), key.node.ProtoReflect()) {
					index = key.index
					break
				}
			}
		}
		if index < 0 {
			value, err := b.targetScalar(node, s)
			if err != nil {
				return nil, nil, nil, err
			}
			index, err = b.tailSourceTarget(value, targets[:visible], &work)
			if err != nil {
				return nil, nil, nil, err
			}
			if index >= 0 {
				keys = append(keys, tailTarget{node: node, index: index})
				continue
			}
			if len(n.DistinctClause) != 0 && (n.DistinctClause[0] == nil || n.DistinctClause[0].Node == nil) {
				return nil, nil, nil, bindError("42P10", "ORDER BY expressions must appear in SELECT DISTINCT list", value.position)
			}
			index = len(targets)
			targets = append(targets, value)
			columns = append(columns, column{hidden: true})
		}
		keys = append(keys, tailTarget{node: node, index: index})
	}
	return targets, columns, keys, nil
}

func (b *binder) tailTargetIndex(node *zpgparse.Node, targets []expression, columns []column, targetNodes []*zpgparse.Node, work *uint64) (int, error) {
	if constant := node.GetAConst(); constant != nil && constant.GetIval() != nil {
		index := int(constant.GetIval().Ival) - 1
		if index < 0 || index >= len(columns) {
			return -1, bindError("42P10", "ORDER BY position is not in select list", int(constant.Location)+1)
		}
		return index, nil
	}
	if ref := node.GetColumnRef(); ref != nil && len(ref.Fields) == 1 && ref.Fields[0].GetString_() != nil {
		index, err := b.tailAlias(ref, targets, columns, targetNodes, work)
		if err != nil || index >= 0 {
			return index, err
		}
	}
	for i, target := range targetNodes {
		if err := b.tailComparison(work); err != nil {
			return -1, err
		}
		if target != nil && equivalentAST(node.ProtoReflect(), target.ProtoReflect()) {
			return i, nil
		}
	}
	return -1, nil
}

func (b *binder) tailAlias(ref *zpgparse.ColumnRef, targets []expression, columns []column, nodes []*zpgparse.Node, work *uint64) (int, error) {
	name := ref.Fields[0].GetString_().GetSval()
	index := -1
	for i, c := range columns {
		if err := b.tailComparison(work); err != nil {
			return -1, err
		}
		if c.Name != name {
			continue
		}
		if index < 0 {
			index = i
			continue
		}
		if !sameTailTarget(targets[index], targets[i], nodes[index], nodes[i]) {
			return -1, bindError("42702", "ORDER BY column is ambiguous", int(ref.Location)+1)
		}
	}
	return index, nil
}

func sameTailTarget(left, right expression, leftNode, rightNode *zpgparse.Node) bool {
	if left.sourceSlot != 0 && right.sourceSlot != 0 {
		return left.sourceSlot == right.sourceSlot && sameType(left.typ, right.typ)
	}
	return leftNode != nil && rightNode != nil && equivalentAST(leftNode.ProtoReflect(), rightNode.ProtoReflect())
}

func (b *binder) tailComparison(work *uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if *work == 0 {
		return bindError("54000", "ordering comparison limit exceeded", 0)
	}
	*work -= 1
	return nil
}

func (b *binder) tailSourceTarget(value expression, targets []expression, work *uint64) (int, error) {
	if value.sourceSlot == 0 {
		return -1, nil
	}
	for i, target := range targets {
		if err := b.tailComparison(work); err != nil {
			return -1, err
		}
		if value.sourceSlot == target.sourceSlot && sameType(value.typ, target.typ) {
			return i, nil
		}
	}
	return -1, nil
}

func (b *binder) trimTail(input relation, s *scope, visible int) (relation, error) {
	if visible == len(input.columns) {
		return input, nil
	}
	columns := slices.Clone(input.columns[:visible])
	bindings := make([]ir.Binding, len(columns))
	for i, c := range columns {
		e, err := b.reference(s, c, 0)
		if err != nil {
			return input, err
		}
		bindings[i] = ir.Binding{Slot: c.Slot, Expr: e.id}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: input.effects}, err
}

func (b *binder) deferredTail(n *zpgparse.SelectStmt, s *scope, input relation, columns []column, bindings []ir.Binding, targets []expression, keys []tailTarget, visible int) (relation, error) {
	if len(keys) == 0 {
		input, err := b.selectTail(n, s, input)
		if err != nil {
			return input, err
		}
		return b.projectTailResult(input, s, columns, bindings, targets)
	}
	selected := make([]bool, len(columns))
	s.tailColumns = make(map[*zpgparse.Node]column, len(keys))
	beforeColumns := make([]column, 0, len(input.columns))
	before := make([]ir.Binding, 0, len(input.columns))
	for _, c := range input.columns {
		// Unavailable catalog fields cannot be dependencies of a successfully
		// bound target or ordering key; do not retain them through tail stages.
		if c.unsupported != "" {
			continue
		}
		e, err := b.reference(s, c, 0)
		if err != nil {
			return input, err
		}
		beforeColumns = append(beforeColumns, c)
		before = append(before, ir.Binding{Slot: c.Slot, Expr: e.id})
	}
	for _, key := range keys {
		s.tailColumns[key.node] = columns[key.index]
		if selected[key.index] {
			continue
		}
		selected[key.index] = true
		beforeColumns = append(beforeColumns, columns[key.index])
		before = append(before, bindings[key.index])
		input.effects = input.effects.Union(targets[key.index].effects)
	}
	if len(keys) != 0 {
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(beforeColumns), Bindings: before})
		if err != nil {
			return input, err
		}
		input.id, input.columns = id, beforeColumns
	}
	input, err := b.selectTail(n, s, input)
	if err != nil {
		return input, err
	}
	for i := range visible {
		if selected[i] {
			e, err := b.reference(s, columns[i], 0)
			if err != nil {
				return input, err
			}
			bindings[i].Expr = e.id
		}
	}
	return b.projectTailResult(input, s, columns[:visible], bindings[:visible], targets[:visible])
}

func (b *binder) projectTailResult(input relation, s *scope, columns []column, bindings []ir.Binding, targets []expression) (relation, error) {
	if b.mutationProvenance != 0 {
		for _, source := range input.columns {
			info, ok := b.builder.Slot(source.Slot)
			if !ok || info.Object != b.mutationProvenance && !source.internal || slices.ContainsFunc(columns, func(column column) bool { return column.Slot == source.Slot }) {
				continue
			}
			value, err := b.reference(s, source, 0)
			if err != nil {
				return input, err
			}
			source.hidden = true
			source.internal = true
			columns = append(columns, source)
			bindings = append(bindings, ir.Binding{Slot: source.Slot, Expr: value.id})
			targets = append(targets, value)
		}
	}
	for _, target := range targets {
		input.effects = input.effects.Union(target.effects)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: input.effects}, err
}
