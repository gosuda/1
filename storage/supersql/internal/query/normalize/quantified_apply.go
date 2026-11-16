package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (n *normalizer) quantifiedPredicate(id ir.ScalarID, subquery ir.Scalar, depth uint32) (predicateInput, bool) {
	operand, _ := n.g.Operand(id, 0)
	// Quantified comparison signatures live in the scalar's comparisons arena;
	// the Subquery payload carries no Signature/Operator/Function of its own.
	signatureID, _ := n.g.Comparison(id, 0)
	signature, _ := n.g.Signature(signatureID)
	if signature.Effects.Volatility != ir.Immutable {
		return predicateInput{}, false
	}
	right, _ := n.g.Rel(subquery.Relation)
	captured, eligible := n.captureQuantifierInput(operand, subquery.Scope, right.Scope, depth+1)
	if !eligible {
		return predicateInput{}, false
	}
	rhs := n.relation(subquery.Relation, depth+1)
	if n.err != nil {
		return predicateInput{}, true
	}
	left, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: right.Scope, Rows: 1}})
	if err != nil {
		n.err = err
		return predicateInput{}, true
	}
	rightSlot, _ := n.g.Output(subquery.Relation, 0)
	rightInfo, _ := n.g.Slot(rightSlot)
	rightValue := n.addScalar(ir.Scalar{Kind: ir.Slot, Scope: right.Scope, Layout: n.layouts[rightInfo.Layout], Nullable: rightInfo.Nullable, Slot: rightSlot}, nil)
	boolean := n.layouts[subquery.Layout]
	condition := n.addScalar(ir.Scalar{Kind: ir.Call, Scope: right.Scope, Layout: boolean, Nullable: true, Signature: signatureID, Function: signature.Function, Operator: signature.Operator, Strict: signature.Strict, Effects: signature.Effects}, []scalarResult{captured, rightValue})
	if n.err != nil {
		return predicateInput{}, true
	}
	mark, err := n.b.AddSlot(n.ctx, ir.SlotInfo{Scope: right.Scope, Layout: boolean, Nullable: true})
	if err != nil {
		n.err = err
		return predicateInput{}, true
	}
	effects := rhs.n.Effects.Union(condition.n.Effects)
	joined, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: right.Scope, JoinKind: ir.MarkJoin, MarkKind: subquery.SubqueryKind, MarkSlot: mark, Predicate: condition.id, Effects: effects}, Inputs: []ir.RelID{left, rhs.id}, Outputs: []model.SlotID{mark}})
	if err != nil {
		n.err = err
		return predicateInput{}, true
	}
	value := n.addScalar(ir.Scalar{Kind: ir.Slot, Scope: right.Scope, Layout: boolean, Nullable: true, Slot: mark}, nil)
	if n.err != nil {
		return predicateInput{}, true
	}
	projected, err := n.b.AddRel(n.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: right.Scope, Effects: effects}, Inputs: []ir.RelID{joined}, Outputs: []model.SlotID{mark}, Bindings: []ir.Binding{{Slot: mark, Expr: value.id}}})
	n.err = err
	// The caller applies the ScalarApply on top of this relation, so it needs
	// this relation's effects plus the MayError the Apply itself contributes.
	return predicateInput{right: projected, kind: ir.Apply, applyKind: ir.ScalarApply, rightEffects: effects.Union(ir.Effects{MayError: true})}, true
}

func (n *normalizer) captureQuantifierInput(id ir.ScalarID, source, target ir.ScopeID, depth uint32) (scalarResult, bool) {
	meta, _ := n.g.Scalar(id)
	switch meta.Kind {
	case ir.Slot, ir.Const, ir.Param:
		value := n.scalar(id, depth)
		if n.err != nil {
			return scalarResult{}, true
		}
		meta = value.n
		meta.Scope = target
		if meta.Kind == ir.Slot {
			n.err = n.b.AddScopeCapture(n.ctx, target, meta.Slot)
			if n.err != nil {
				return scalarResult{}, true
			}
		}
		if meta.Kind == ir.Const {
			return n.constant(meta, value.value), true
		}
		return n.addScalar(meta, nil), true
	}
	clone := scopeClone{n: n, source: source, target: target, captureInputs: true}
	if !clone.scalarSupported(id, depth) {
		return scalarResult{}, false
	}
	clone.initialize()
	captured := clone.scalar(id, depth)
	meta.Scope, meta.Layout = target, n.layouts[meta.Layout]
	return scalarResult{id: captured, n: meta}, true
}
