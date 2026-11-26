package opt

import (
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// GINAccess advertises one GIN inverted index access path over a single base
// table column.
type GINAccess struct {
	Enabled   bool
	IndexName string
	TableName string
	OpClass   string
	Column    model.ColumnID
	Layout    model.TypeLayout
}

func equalGINAccess(a, b GINAccess) bool {
	return a.Enabled == b.Enabled &&
		a.IndexName == b.IndexName &&
		a.TableName == b.TableName &&
		a.OpClass == b.OpClass &&
		a.Column == b.Column &&
		model.TypeLayoutsEqual(a.Layout, b.Layout)
}

func validateGINAccess(access Access, path AccessPath) error {
	g := path.GIN
	if !g.Enabled {
		if g.IndexName != "" || g.TableName != "" || g.OpClass != "" || g.Column != 0 || g.Layout.Type != (model.Type{}) || len(g.Layout.Children) != 0 {
			return fmt.Errorf("%w: disabled GIN access carries metadata", ErrInvalid)
		}
		return nil
	}
	if path.Algorithm != physical.SecondaryScan || !path.Lookup.Enabled || g.Column == 0 || g.OpClass == "" {
		return fmt.Errorf("%w: invalid GIN access metadata", ErrInvalid)
	}
	colIdx := slices.IndexFunc(access.Layout, func(c storage.ColumnLayout) bool { return c.ID == g.Column })
	if colIdx < 0 {
		return fmt.Errorf("%w: GIN access column absent from base table", ErrInvalid)
	}
	col := access.Layout[colIdx]
	if !model.TypeLayoutsEqual(g.Layout, model.TypeLayout{Type: col.Type, Children: col.Children}) {
		return fmt.Errorf("%w: GIN access column layout mismatch", ErrInvalid)
	}
	return nil
}

func (o *optimizer) propagateGINPredicate(id ir.RelID, predicate ir.ScalarID, outputs []model.SlotID, parents []uint32, depth uint32) {
	if id == 0 || parents[id] != 1 || depth >= o.options.IR.MaxDepth || !o.step(1) {
		return
	}
	relation, _ := o.graph.Rel(id)
	if relation.SecurityBarrier || relation.Effects.Volatility == ir.Volatile {
		return
	}
	if int(relation.OutputCount()) != len(outputs) {
		return
	}
	switch relation.Kind {
	case ir.Scan:
		if o.ginPredicates[id] == 0 {
			o.ginPredicates[id] = predicate
			o.ginPredicateSlots[id] = outputs
		}
	case ir.Filter:
		child, _ := o.graph.Input(id, 0)
		o.propagateGINPredicate(child, predicate, outputs, parents, depth+1)
	case ir.SetOp:
		for i := range relation.InputCount() {
			child, _ := o.graph.Input(id, i)
			o.propagateGINPredicate(child, predicate, outputs, parents, depth+1)
		}
	case ir.Project:
		child, _ := o.graph.Input(id, 0)
		input, _ := o.graph.Rel(child)
		mapped := allocate[model.SlotID](o, uint64(input.OutputCount()))
		if o.err != nil {
			return
		}
		for i := range outputs {
			output, _ := o.graph.Output(id, uint32(i))
			for j := range relation.BindingCount() {
				binding, _ := o.graph.Binding(id, j)
				if binding.Slot != output {
					continue
				}
				scalar, _ := o.graph.Scalar(binding.Expr)
				if scalar.Kind != ir.Slot {
					break
				}
				for k := range input.OutputCount() {
					slot, _ := o.graph.Output(child, k)
					if slot == scalar.Slot {
						mapped[k] = outputs[i]
						break
					}
				}
				break
			}
		}
		o.propagateGINPredicate(child, predicate, mapped, parents, depth+1)
	}
}

// matchGINPredicate inspects the relation's WHERE predicate conjuncts for a
// GIN-indexable operator targeting gin.Column with a query expression free of
// row-column slot references and volatile side effects.
func (o *optimizer) matchGINPredicate(id ir.RelID, outputs []physical.Slot, gin GINAccess) (model.OperatorID, ir.ScalarID, bool) {
	if !gin.Enabled || gin.Column == 0 {
		return 0, 0, false
	}
	tryMatch := func(predicate ir.ScalarID, predSlots []model.SlotID) (model.OperatorID, ir.ScalarID, bool) {
		if predicate == 0 {
			return 0, 0, false
		}
		colSlots := make(map[model.SlotID]struct{}, 2)
		for i, output := range outputs {
			info, ok := o.graph.Slot(output.ID)
			if !ok || info.Column != gin.Column {
				continue
			}
			colSlots[output.ID] = struct{}{}
			if i < len(predSlots) && predSlots[i] != 0 {
				colSlots[predSlots[i]] = struct{}{}
			}
		}
		if len(colSlots) == 0 {
			return 0, 0, false
		}
		return o.matchGINConjunct(predicate, colSlots, gin.OpClass, 0)
	}
	if op, rhs, ok := tryMatch(o.predicates[id], o.predicateSlots[id]); ok {
		return op, rhs, true
	}
	return tryMatch(o.ginPredicates[id], o.ginPredicateSlots[id])
}

func (o *optimizer) matchGINConjunct(scalarID ir.ScalarID, colSlots map[model.SlotID]struct{}, opClass string, depth uint32) (model.OperatorID, ir.ScalarID, bool) {
	if scalarID == 0 || depth >= o.options.IR.MaxDepth {
		return 0, 0, false
	}
	sc, ok := o.graph.Scalar(scalarID)
	if !ok {
		return 0, 0, false
	}
	if sc.Kind == ir.Bool && sc.BoolOp == ir.And {
		for i := range sc.OperandCount() {
			child, _ := o.graph.Operand(scalarID, i)
			if op, rhs, matched := o.matchGINConjunct(child, colSlots, opClass, depth+1); matched {
				return op, rhs, true
			}
		}
		return 0, 0, false
	}
	if sc.Kind != ir.Call || sc.Operator == 0 || sc.OperandCount() != 2 || sc.Effects.Volatility == ir.Volatile {
		return 0, 0, false
	}
	leftID, _ := o.graph.Operand(scalarID, 0)
	rightID, _ := o.graph.Operand(scalarID, 1)
	if o.isGINColumnSlot(leftID, colSlots, 0) && o.isGINQueryRHS(rightID, 0) {
		if normOp, ok := matchGINOperator(opClass, sc.Operator, false); ok {
			return normOp, rightID, true
		}
	}
	if o.isGINColumnSlot(rightID, colSlots, 0) && o.isGINQueryRHS(leftID, 0) {
		if normOp, ok := matchGINOperator(opClass, sc.Operator, true); ok {
			return normOp, leftID, true
		}
	}
	return 0, 0, false
}

func (o *optimizer) isGINColumnSlot(scalarID ir.ScalarID, colSlots map[model.SlotID]struct{}, depth uint32) bool {
	if scalarID == 0 || depth >= o.options.IR.MaxDepth {
		return false
	}
	sc, ok := o.graph.Scalar(scalarID)
	if !ok {
		return false
	}
	if sc.Kind == ir.Once && sc.OperandCount() == 1 {
		inner, _ := o.graph.Operand(scalarID, 0)
		return o.isGINColumnSlot(inner, colSlots, depth+1)
	}
	if sc.Kind == ir.Slot {
		_, ok := colSlots[sc.Slot]
		return ok
	}
	return false
}

// isGINQueryRHS ensures the RHS query expression can be evaluated once at scan
// open time without row-level slot bindings, subqueries, or volatile effects.
func (o *optimizer) isGINQueryRHS(scalarID ir.ScalarID, depth uint32) bool {
	if scalarID == 0 || depth >= o.options.IR.MaxDepth {
		return false
	}
	sc, ok := o.graph.Scalar(scalarID)
	if !ok {
		return false
	}
	if sc.Effects.Volatility == ir.Volatile {
		return false
	}
	if sc.Kind == ir.Slot || sc.Kind == ir.Subquery {
		return false
	}
	for i := range sc.OperandCount() {
		child, _ := o.graph.Operand(scalarID, i)
		if !o.isGINQueryRHS(child, depth+1) {
			return false
		}
	}
	return true
}

func matchGINOperator(opClass string, opID model.OperatorID, swapped bool) (model.OperatorID, bool) {
	switch opClass {
	case "jsonb_ops":
		if !swapped {
			switch opID {
			case 3246, 3247, 3248, 3249:
				return opID, true
			}
		} else if opID == 3250 { // rhs <@ col  <=>  col @> rhs
			return 3246, true
		}
	case "jsonb_path_ops":
		if !swapped && opID == 3246 {
			return 3246, true
		}
		if swapped && opID == 3250 {
			return 3246, true
		}
	case "array_ops":
		if !swapped {
			switch opID {
			case 2751, 2750: // @>, &&
				return opID, true
			}
		} else {
			switch opID {
			case 2752: // rhs <@ col  <=>  col @> rhs
				return 2751, true
			case 2750: // rhs && col  <=>  col && rhs
				return 2750, true
			}
		}
	case "tsvector_ops":
		if !swapped && opID == 3636 { // tsvector @@ tsquery
			return 3636, true
		}
		if swapped && opID == 3637 { // tsquery @@ tsvector
			return 3636, true
		}
	}
	return 0, false
}
