package physical

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateScalarSubplans(ctx context.Context, n NodeSpec, b *copier) error {
	g, logical, err := p.relation(n.Logical)
	if err != nil {
		return err
	}
	// Property enforcers do not evaluate their input relation's expressions again.
	if len(n.Children) == 1 && p.nodes[n.Children[0]-1].Logical == n.Logical && len(n.Expressions) == 0 {
		if len(n.ScalarSubplans) != 0 {
			return invalid("scalar dependency on property enforcer")
		}
		return nil
	}
	if err := b.charge(uint64(len(n.ScalarSubplans)), 1); err != nil {
		return err
	}
	seen := make([]bool, len(n.ScalarSubplans))
	remaining := uint64(b.limits.MaxOperands)
	defer func() { b.used -= uint64(len(n.ScalarSubplans)) }()
	var visit func(ScalarRef, uint32) error
	visit = func(ref ScalarRef, depth uint32) error {
		if ref.Scalar == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= b.limits.MaxDepth || remaining == 0 {
			return ErrLimit
		}
		remaining--
		graph, ok := p.Graph(ref.Graph)
		if !ok {
			return invalid("scalar graph identity")
		}
		scalar, ok := graph.Scalar(ref.Scalar)
		if !ok {
			return invalid("scalar identity")
		}
		if scalar.Kind == ir.Subquery {
			found := -1
			for i, subplan := range n.ScalarSubplans {
				if remaining == 0 {
					return ErrLimit
				}
				remaining--
				if subplan.Scalar != ref {
					continue
				}
				if found >= 0 {
					return invalid("duplicate scalar dependency")
				}
				found = i
			}
			if found < 0 {
				return invalid("missing scalar dependency")
			}
			if !seen[found] {
				subplan := n.ScalarSubplans[found]
				body := p.nodes[subplan.Root-1]
				if body.Logical != (RelRef{Graph: ref.Graph, Rel: scalar.Relation}) {
					return invalid("scalar body identity")
				}
				switch scalar.SubqueryKind {
				case ir.ExistsSubquery:
				case ir.ScalarSubquery:
					if len(body.Output) != 1 {
						return invalid("scalar body width")
					}
					if err := p.validateSubplanOutputLayout(ctx, graph, scalar.Layout, body.Output[0]); err != nil {
						return err
					}
				case ir.TupleSubquery:
					typ, ok := graph.Type(scalar.Layout)
					if !ok || typ.Storage != model.TypeComposite || int(graph.LayoutChildCount(scalar.Layout)) != len(body.Output) {
						return invalid("tuple body width")
					}
					for i, output := range body.Output {
						child, ok := graph.LayoutChild(scalar.Layout, uint32(i))
						if !ok {
							return invalid("tuple child layout")
						}
						if err := p.validateSubplanOutputLayout(ctx, graph, child, output); err != nil {
							return err
						}
					}
				case ir.ArraySubquery:
					if len(body.Output) != 1 {
						return invalid("ARRAY body width")
					}
					slot, _ := graph.Output(scalar.Relation, 0)
					info, _ := graph.Slot(slot)
					if err := p.validateSubplanOutputLayout(ctx, graph, info.Layout, body.Output[0]); err != nil {
						return err
					}
				default:
					if len(body.Output) != int(scalar.ComparisonCount()) {
						return invalid("row-comparison body width")
					}
					for i, output := range body.Output {
						signature, _ := graph.Comparison(ref.Scalar, uint32(i))
						layout, _ := graph.SignatureParameter(signature, 1)
						if err := p.validateSubplanOutputLayout(ctx, graph, layout, output); err != nil {
							return err
						}
					}
				}
				for _, capture := range body.Delivered.Parameters {
					available, err := p.scalarCaptureAvailable(n, capture, &remaining)
					if err != nil {
						return err
					}
					if !available {
						return invalid("unavailable scalar capture")
					}
				}
				seen[found] = true
			}
		}
		for i := range scalar.OperandCount() {
			child, _ := graph.Operand(ref.Scalar, i)
			if err := visit(ScalarRef{Graph: ref.Graph, Scalar: child}, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	var walkErr error
	add := func(id ir.ScalarID) {
		if walkErr == nil {
			walkErr = visit(ScalarRef{Graph: n.Logical.Graph, Scalar: id}, 0)
		}
	}
	for _, id := range [...]ir.ScalarID{logical.Predicate, logical.Offset, logical.Count, logical.ConflictPredicate, logical.Frame.Start.Offset, logical.Frame.End.Offset, logical.Recursion.CycleValue, logical.Recursion.CycleDefault, logical.Recursion.CycleContinue} {
		add(id)
	}
	for i := range logical.ExpressionCount() {
		id, _ := g.Expression(n.Logical.Rel, i)
		add(id)
	}
	for i := range logical.BindingCount() {
		binding, _ := g.Binding(n.Logical.Rel, i)
		add(binding.Expr)
	}
	for i := range logical.OrderCount() {
		key, _ := g.Order(n.Logical.Rel, i)
		add(key.Expr)
	}
	for i := range logical.PartitionCount() {
		id, _ := g.Partition(n.Logical.Rel, i)
		add(id)
	}
	for i := range logical.AggregateCount() {
		call, _ := g.Aggregate(n.Logical.Rel, i)
		add(call.Filter)
		for j := range call.DirectArgumentCount() {
			id, _ := g.AggregateDirectArgument(n.Logical.Rel, i, j)
			add(id)
		}
		for j := range call.ArgumentCount() {
			id, _ := g.AggregateArgument(n.Logical.Rel, i, j)
			add(id)
		}
		for j := range call.OrderCount() {
			key, _ := g.AggregateOrder(n.Logical.Rel, i, j)
			add(key.Expr)
		}
	}
	for i := range logical.WindowCount() {
		call, _ := g.Window(n.Logical.Rel, i)
		add(call.Filter)
		for j := range call.ArgumentCount() {
			id, _ := g.WindowArgument(n.Logical.Rel, i, j)
			add(id)
		}
	}
	for i := range logical.AssignmentCount() {
		binding, _ := g.Assignment(n.Logical.Rel, i)
		add(binding.Expr)
	}
	for i := range logical.ConflictAssignmentCount() {
		binding, _ := g.ConflictAssignment(n.Logical.Rel, i)
		add(binding.Expr)
	}
	for i := range logical.ActionCount() {
		action, _ := g.MergeAction(n.Logical.Rel, i)
		add(action.Condition)
		for j := range action.AssignmentCount() {
			binding, _ := g.MergeAssignment(n.Logical.Rel, i, j)
			add(binding.Expr)
		}
	}
	if walkErr != nil {
		return walkErr
	}
	for _, used := range seen {
		if !used {
			return invalid("scalar dependency outside logical expressions")
		}
	}
	for _, ref := range n.Expressions {
		if err := visit(ref, 0); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) validateSubplanOutputLayout(ctx context.Context, graph *ir.Graph, layout ir.LayoutID, output Slot) error {
	bodyGraph, _ := p.Graph(output.Graph)
	equal, err := sameLayout(ctx, graph, layout, bodyGraph, output.Layout)
	if err != nil {
		return err
	}
	if !equal {
		return invalid("subquery result layout")
	}
	return nil
}

func (p *Plan) scalarCaptureAvailable(n NodeSpec, capture Slot, remaining *uint64) (bool, error) {
	for _, child := range n.Children {
		for _, slot := range p.nodes[child-1].Output {
			if *remaining == 0 {
				return false, ErrLimit
			}
			*remaining--
			if slot == capture {
				return true, nil
			}
		}
	}
	for _, slot := range n.Delivered.Parameters {
		if *remaining == 0 {
			return false, ErrLimit
		}
		*remaining--
		if slot == capture {
			return true, nil
		}
	}
	g, logical, err := p.relation(n.Logical)
	if err != nil {
		return false, err
	}
	if logical.Kind != ir.Modify || capture.Graph != n.Logical.Graph {
		return false, nil
	}
	if capture.ID != logical.OldRow && capture.ID != logical.NewRow && capture.ID != logical.ExcludedRow && capture.ID != logical.MergeResultRow && capture.ID != logical.MergeActionSlot {
		return false, nil
	}
	info, ok := g.Slot(capture.ID)
	return ok && capture == (Slot{Graph: n.Logical.Graph, ID: capture.ID, Layout: info.Layout, Nullable: info.Nullable}), nil
}
