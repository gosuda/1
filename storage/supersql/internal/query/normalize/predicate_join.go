package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Only a whole filter predicate is unmasked here. CASE and boolean combinations
// retain their scalar dependency until their demand masks can be represented.
func (n *normalizer) predicateJoin(source ir.Rel, spec ir.RelSpec, left relResult, depth uint32) (relResult, bool) {
	if source.Kind != ir.Filter || len(spec.Inputs) != 1 {
		return relResult{}, false
	}
	id := source.Predicate
	subquery, _ := n.g.Scalar(id)
	negated := subquery.Kind == ir.Bool && subquery.BoolOp == ir.Not && subquery.OperandCount() == 1
	if negated {
		id, _ = n.g.Operand(id, 0)
		subquery, _ = n.g.Scalar(id)
	}
	if subquery.Kind != ir.Subquery || subquery.SubqueryKind == ir.ScalarSubquery {
		return relResult{}, false
	}
	plan, eligible := n.preparePredicateInput(source.Scope, id, subquery, depth)
	if !eligible {
		return relResult{}, false
	}
	boolean := n.layouts[subquery.Layout]
	if n.err != nil {
		return relResult{}, true
	}
	nullable := subquery.SubqueryKind != ir.ExistsSubquery
	mark, err := n.b.AddSlot(n.ctx, ir.SlotInfo{Scope: source.Scope, Layout: boolean, Nullable: nullable})
	if err != nil {
		n.err = err
		return relResult{}, true
	}
	outputs := allocate[model.SlotID](n, uint64(len(left.outputs)+1))
	if n.err != nil {
		return relResult{}, true
	}
	copy(outputs, left.outputs)
	outputs[len(left.outputs)] = mark
	// The lowered right relation's effects are part of this relation's inputs,
	// so verification requires them in the declared effects even when the
	// condition scalar alone would not carry them.
	effects := spec.Effects.Union(plan.rightEffects).Union(plan.condition.n.Effects)
	if plan.applyKind == ir.ScalarApply {
		effects.MayError = true
	}
	operation := ir.Rel{Kind: plan.kind, Scope: source.Scope, Effects: effects, MarkSlot: mark, Predicate: plan.condition.id}
	if plan.kind == ir.Join {
		operation.JoinKind, operation.MarkKind = ir.MarkJoin, subquery.SubqueryKind
	} else {
		operation.ApplyKind = plan.applyKind
		if plan.applyKind == ir.ScalarApply {
			operation.MarkSlot = 0
		}
	}
	join, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: operation, Inputs: []ir.RelID{left.id, plan.right}, Outputs: outputs})
	if err != nil {
		n.err = err
		return relResult{}, true
	}
	predicate := n.addScalar(ir.Scalar{Kind: ir.Slot, Scope: source.Scope, Layout: boolean, Nullable: nullable, Slot: mark}, nil)
	if negated {
		predicate = n.addScalar(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Not, Scope: source.Scope, Layout: boolean, Nullable: nullable}, []scalarResult{predicate})
	}
	if n.err != nil {
		return relResult{}, true
	}
	filtered, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: source.Scope, Effects: effects, Predicate: predicate.id}, Inputs: []ir.RelID{join}, Outputs: outputs})
	if err != nil {
		n.err = err
		return relResult{}, true
	}
	bindings := allocate[ir.Binding](n, uint64(len(spec.Outputs)))
	if n.err != nil {
		return relResult{}, true
	}
	for i, slot := range spec.Outputs {
		info, _ := n.g.Slot(slot)
		value := n.addScalar(ir.Scalar{Kind: ir.Slot, Scope: source.Scope, Layout: n.layouts[info.Layout], Nullable: info.Nullable, Slot: slot}, nil)
		bindings[i] = ir.Binding{Slot: slot, Expr: value.id}
	}
	if n.err != nil {
		return relResult{}, true
	}
	project := ir.Rel{Kind: ir.Project, Scope: source.Scope, Effects: effects, Span: source.Span}
	root, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: project, Inputs: []ir.RelID{filtered}, Outputs: spec.Outputs, Bindings: bindings})
	n.err = err
	return relResult{id: root, n: project, input: filtered, outputs: spec.Outputs}, true
}
