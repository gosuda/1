package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type constantColumn struct {
	slot  model.SlotID
	typ   model.Type
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

func (n *normalizer) constantSlot(slot model.SlotID) (model.Value, model.Type, bool) {
	for _, column := range n.constantInputs {
		if !n.step(1) {
			return model.Value{}, model.Type{}, false
		}
		if column.slot == slot {
			return column.value, column.typ, true
		}
	}
	return model.Value{}, model.Type{}, false
}

func (n *normalizer) filterConstant(id ir.ScalarID, slot model.SlotID, depth uint32) (model.Value, model.Type, bool) {
	if id == 0 || depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
		}
		return model.Value{}, model.Type{}, false
	}
	scalar, ok := n.g.Scalar(id)
	if !ok {
		n.err = ErrInvalid
		return model.Value{}, model.Type{}, false
	}
	if scalar.Kind == ir.Bool && scalar.BoolOp == ir.And {
		for i := range scalar.OperandCount() {
			child, _ := n.g.Operand(id, i)
			if value, typ, found := n.filterConstant(child, slot, depth+1); found {
				return value, typ, true
			}
		}
		return model.Value{}, model.Type{}, false
	}
	if scalar.Kind != ir.Call || scalar.OperandCount() != 2 {
		return model.Value{}, model.Type{}, false
	}
	operator, ok := builtin.LookupOperator(scalar.Operator)
	if !ok || operator.Name != "=" || operator.Left != operator.Right {
		return model.Value{}, model.Type{}, false
	}
	leftID, _ := n.g.Operand(id, 0)
	rightID, _ := n.g.Operand(id, 1)
	left, right := n.scalars[leftID], n.scalars[rightID]
	var slotExpr, constExpr scalarResult
	switch {
	case left.n.Kind == ir.Slot && left.n.Slot == slot && right.n.Kind == ir.Const && !right.value.IsNull:
		slotExpr, constExpr = left, right
	case right.n.Kind == ir.Slot && right.n.Slot == slot && left.n.Kind == ir.Const && !left.value.IsNull:
		slotExpr, constExpr = right, left
	default:
		return model.Value{}, model.Type{}, false
	}
	slotType, safe := n.safeEqualityConstant(scalar, operator, slot, slotExpr, constExpr)
	if !safe {
		return model.Value{}, model.Type{}, false
	}
	return constExpr.value, slotType, true
}

// safeEqualityConstant reports whether substituting constExpr for slot under an
// equality filter preserves exact SQL type identity and observable value
// representation. Cross-type equalities (e.g. int4 = int8, timestamp = date)
// and non-identity-preserving types (e.g. numeric scale, bpchar trailing
// spaces, float signed zero, timestamptz, jsonb, non-deterministic collations,
// or composite/array/range values) are rejected.
func (n *normalizer) safeEqualityConstant(call ir.Scalar, operator builtin.Operator, slot model.SlotID, slotExpr, constExpr scalarResult) (model.Type, bool) {
	slotInfo, ok := n.g.Slot(slot)
	if !ok {
		return model.Type{}, false
	}
	if n.g.LayoutChildCount(slotInfo.Layout) != 0 || n.g.LayoutChildCount(slotExpr.sourceLayout) != 0 || n.g.LayoutChildCount(constExpr.sourceLayout) != 0 {
		return model.Type{}, false
	}
	declType, ok := n.g.Type(slotInfo.Layout)
	if !ok {
		return model.Type{}, false
	}
	slotType, ok := n.g.Type(slotExpr.sourceLayout)
	if !ok {
		return model.Type{}, false
	}
	constType, ok := n.g.Type(constExpr.sourceLayout)
	if !ok {
		return model.Type{}, false
	}
	if slotType != constType || slotType != declType || constExpr.value.Type != slotType.Storage {
		return model.Type{}, false
	}
	if operator.Left != slotType.ID && !(slotType.ID == 1043 && operator.Left == 25) {
		return model.Type{}, false
	}
	switch slotType.ID {
	case 16, // bool
		17,   // bytea
		20,   // int8
		21,   // int2
		23,   // int4
		26,   // oid
		1082, // date
		1083, // time
		1114, // timestamp
		2950: // uuid
		return slotType, true
	case 25, // text
		1043: // varchar
		bound, ok := n.g.Signature(call.Signature)
		if !ok {
			return model.Type{}, false
		}
		cmpCollation := builtin.InputCollation(bound.InputCollation, []model.Type{slotType, constType})
		if !isDeterministicCollation(slotType.Collation, n.o.Builtin) || !isDeterministicCollation(cmpCollation, n.o.Builtin) {
			return model.Type{}, false
		}
		return slotType, true
	default:
		return model.Type{}, false
	}
}

func isDeterministicCollation(collation model.CollationID, options builtin.ResolveOptions) bool {
	if collation == 100 {
		collation = options.DefaultCollation
	}
	if collation == 0 {
		return false
	}
	if builtin.IsBinaryCollation(collation) {
		return true
	}
	resolver := options.Collations
	if resolver == nil {
		if cr, ok := options.Types.(builtin.CollationResolver); ok {
			resolver = cr
		}
	}
	spec, ok := builtin.LookupICUCollation(collation, resolver)
	return ok && spec.Deterministic
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
		var typ model.Type
		found := false
		switch source.Kind {
		case ir.Values:
			if source.Rows == 1 && source.ExpressionCount() == source.OutputCount() {
				expression, _ := n.g.Expression(id, uint32(i))
				entry := n.scalars[expression]
				if entry.n.Kind == ir.Const {
					if exprType, ok := n.g.Type(entry.sourceLayout); ok && entry.value.Type == exprType.Storage {
						value, typ, found = entry.value, exprType, true
					}
				}
			}
		case ir.Project:
			value, typ, found = n.projectConstant(id, source, slot)
		case ir.Filter:
			value, typ, found = n.filterConstant(source.Predicate, slot, 0)
			if !found {
				value, typ, found = n.constantSlot(slot)
			}
		default:
			value, typ, found = n.constantSlot(slot)
		}
		if found {
			result[used] = constantColumn{slot: slot, typ: typ, value: value}
			used++
		}
	}
	return result[:used]
}

func (n *normalizer) projectConstant(id ir.RelID, source ir.Rel, slot model.SlotID) (model.Value, model.Type, bool) {
	for i := uint32(0); i < source.BindingCount(); i++ {
		if !n.step(1) {
			return model.Value{}, model.Type{}, false
		}
		binding, _ := n.g.Binding(id, i)
		if binding.Slot != slot {
			continue
		}
		entry := n.scalars[binding.Expr]
		if entry.n.Kind == ir.Slot {
			return n.constantSlot(entry.n.Slot)
		}
		if entry.n.Kind == ir.Const {
			if exprType, ok := n.g.Type(entry.sourceLayout); ok && entry.value.Type == exprType.Storage {
				return entry.value, exprType, true
			}
		}
		return model.Value{}, model.Type{}, false
	}
	return model.Value{}, model.Type{}, false
}
