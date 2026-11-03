package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type constantColumn struct {
	slot  model.SlotID
	value model.Value
}

func (n *normalizer) inputConstants(id ir.RelID, source ir.Rel) []constantColumn {
	var count uint64
	for i := uint32(0); i < source.InputCount(); i++ {
		if !constantInputVisible(source, i) {
			continue
		}
		child, _ := n.g.Input(id, i)
		count += uint64(len(n.rels[child].constants))
	}
	result := allocate[constantColumn](n, count)
	at := 0
	for i := uint32(0); i < source.InputCount(); i++ {
		if !constantInputVisible(source, i) {
			continue
		}
		child, _ := n.g.Input(id, i)
		for _, column := range n.rels[child].constants {
			if !n.step(1) {
				return nil
			}
			result[at] = column
			at++
		}
	}
	return result
}

func constantInputVisible(source ir.Rel, index uint32) bool {
	if source.SecurityBarrier {
		return false
	}
	switch source.Kind {
	case ir.Join:
		switch source.JoinKind {
		case ir.InnerJoin, ir.CrossJoin:
			return true
		case ir.LeftJoin, ir.SemiJoin, ir.AntiJoin, ir.MarkJoin:
			return index == 0
		case ir.RightJoin:
			return index == 1
		default:
			return false
		}
	case ir.Apply:
		return index == 0
	case ir.Project, ir.Filter, ir.Sort, ir.Limit, ir.Distinct, ir.Window, ir.LockRows, ir.Aggregate:
		return source.InputCount() == 1
	default:
		return false
	}
}

func (n *normalizer) constantSlot(slot model.SlotID) (model.Value, bool) {
	for _, column := range n.constantInputs {
		if !n.step(1) {
			return model.Value{}, false
		}
		if column.slot == slot {
			return column.value, true
		}
	}
	return model.Value{}, false
}

func (n *normalizer) filterConstant(id ir.ScalarID, slot model.SlotID, depth uint32) (model.Value, bool) {
	if id == 0 || depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
		}
		return model.Value{}, false
	}
	scalar, ok := n.g.Scalar(id)
	if !ok {
		n.err = ErrInvalid
		return model.Value{}, false
	}
	if scalar.Kind == ir.Bool && scalar.BoolOp == ir.And {
		for i := range scalar.OperandCount() {
			child, _ := n.g.Operand(id, i)
			if value, found := n.filterConstant(child, slot, depth+1); found {
				return value, true
			}
		}
		return model.Value{}, false
	}
	if scalar.Kind != ir.Call || scalar.OperandCount() != 2 {
		return model.Value{}, false
	}
	operator, ok := builtin.LookupOperator(scalar.Operator)
	if !ok || operator.Name != "=" {
		return model.Value{}, false
	}
	leftID, _ := n.g.Operand(id, 0)
	rightID, _ := n.g.Operand(id, 1)
	left, right := n.scalars[leftID], n.scalars[rightID]
	if left.n.Kind == ir.Slot && left.n.Slot == slot && right.n.Kind == ir.Const && !right.value.IsNull {
		return right.value, true
	}
	if right.n.Kind == ir.Slot && right.n.Slot == slot && left.n.Kind == ir.Const && !left.value.IsNull {
		return left.value, true
	}
	return model.Value{}, false
}

func (n *normalizer) outputConstants(id ir.RelID, source ir.Rel, outputs []model.SlotID) []constantColumn {
	if source.SecurityBarrier {
		return nil
	}
	if source.Kind == ir.Aggregate || source.Kind == ir.SetOp || source.Kind == ir.RecursiveUnion || source.Kind == ir.Modify || source.Kind == ir.RowsFrom {
		return nil
	}
	if source.Kind == ir.Values && (source.Rows != 1 || source.ExpressionCount() != source.OutputCount()) {
		return nil
	}
	if source.Kind != ir.Values && source.Kind != ir.Project && source.Kind != ir.Filter && len(n.constantInputs) == 0 {
		return nil
	}
	result := allocate[constantColumn](n, uint64(len(outputs)))
	used := 0
	for i, slot := range outputs {
		if !n.step(1) {
			return nil
		}
		var value model.Value
		found := false
		switch source.Kind {
		case ir.Values:
			if source.Rows == 1 && source.ExpressionCount() == source.OutputCount() {
				expression, _ := n.g.Expression(id, uint32(i))
				entry := n.scalars[expression]
				value, found = entry.value, entry.n.Kind == ir.Const
			}
		case ir.Project:
			value, found = n.projectConstant(id, source, slot)
		case ir.Filter:
			value, found = n.filterConstant(source.Predicate, slot, 0)
			if !found {
				value, found = n.constantSlot(slot)
			}
		default:
			value, found = n.constantSlot(slot)
		}
		if found {
			result[used] = constantColumn{slot: slot, value: value}
			used++
		}
	}
	return result[:used]
}

func (n *normalizer) projectConstant(id ir.RelID, source ir.Rel, slot model.SlotID) (model.Value, bool) {
	for i := uint32(0); i < source.BindingCount(); i++ {
		if !n.step(1) {
			return model.Value{}, false
		}
		binding, _ := n.g.Binding(id, i)
		if binding.Slot != slot {
			continue
		}
		entry := n.scalars[binding.Expr]
		if entry.n.Kind == ir.Slot {
			return n.constantSlot(entry.n.Slot)
		}
		return entry.value, entry.n.Kind == ir.Const
	}
	return model.Value{}, false
}
