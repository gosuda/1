package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type predicateInput struct {
	right     ir.RelID
	condition scalarResult
	kind      ir.RelKind
	applyKind ir.ApplyKind
	// rightEffects is the normalized right relation's own effects. Lowering
	// replaces the original subquery relation with a freshly built one whose
	// effects differ -- quantified_apply adds a ScalarApply, which raises 21000
	// and so reports MayError. Callers must union this into every relation they
	// build over the new right side, because verification requires a relation's
	// effects to cover its inputs' (ir/verify_rel.go, relationEffects).
	rightEffects ir.Effects
}

func (n *normalizer) preparePredicateInput(scope ir.ScopeID, id ir.ScalarID, subquery ir.Scalar, depth uint32) (predicateInput, bool) {
	right, _ := n.g.Rel(subquery.Relation)
	if n.g.CaptureCount(right.Scope) != 0 {
		if subquery.SubqueryKind == ir.AnySubquery || subquery.SubqueryKind == ir.AllSubquery {
			return n.quantifiedPredicate(id, subquery, depth)
		}
		if subquery.SubqueryKind != ir.ExistsSubquery || !n.simpleExistsInput(subquery.Relation, depth+1) {
			return predicateInput{}, false
		}
		input := subquery.Relation
		// SELECT-list evaluation cannot change this input's existence. Keeping
		// it would incorrectly execute errors or volatile effects in unused targets.
		if right.Kind == ir.Project && !right.SecurityBarrier {
			input, _ = n.g.Input(input, 0)
		}
		result := n.relation(input, depth+1)
		return predicateInput{right: result.id, kind: ir.Apply, applyKind: ir.ExistsApply, rightEffects: result.n.Effects}, true
	}
	input := subquery.Relation
	if subquery.SubqueryKind == ir.ExistsSubquery && right.Kind == ir.Project && !right.SecurityBarrier {
		input, _ = n.g.Input(input, 0)
		right, _ = n.g.Rel(input)
	}
	if right.Effects.Volatility != ir.Immutable || right.Effects.MayError {
		return predicateInput{}, false
	}
	clone := scopeClone{n: n, source: right.Scope, target: scope}
	if !clone.supported(input, depth+1) {
		return predicateInput{}, false
	}
	var lhs scalarResult
	if subquery.SubqueryKind != ir.ExistsSubquery {
		operand, _ := n.g.Operand(id, 0)
		input := scopeClone{n: n, source: scope, target: scope, captureInputs: true}
		if !input.scalarSupported(operand, depth+1) {
			return predicateInput{}, false
		}
		lhs = n.scalar(operand, depth+1)
		if n.err != nil {
			return predicateInput{}, true
		}
	}
	clone.initialize()
	cloned := clone.relation(input, depth+1)
	clonedRel, _ := n.g.Rel(cloned)
	plan := predicateInput{right: cloned, kind: ir.Join, rightEffects: clonedRel.Effects}
	boolean := n.layouts[subquery.Layout]
	if subquery.SubqueryKind == ir.ExistsSubquery {
		plan.condition = n.constant(ir.Scalar{Scope: scope, Layout: boolean}, model.BoolVal(true))
		return plan, true
	}
	output, _ := n.g.Output(subquery.Relation, 0)
	info, _ := n.g.Slot(output)
	rhs := n.addScalar(ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: n.layouts[info.Layout], Nullable: info.Nullable, Slot: clone.slot(output)}, nil)
	// Quantified comparison signatures live in the scalar's comparisons arena;
	// the Subquery payload carries no Signature/Operator/Function of its own.
	signatureID, _ := n.g.Comparison(id, 0)
	signature, _ := n.g.Signature(signatureID)
	plan.condition = n.addScalar(ir.Scalar{Kind: ir.Call, Scope: scope, Layout: boolean, Nullable: true, Signature: signatureID, Function: signature.Function, Operator: signature.Operator, Strict: signature.Strict, Effects: signature.Effects}, []scalarResult{lhs, rhs})
	return plan, true
}

// Stateful SQL forms need their own existence-preserving rewrite rules; do not
// discard DISTINCT/OFFSET/aggregate/window semantics while pruning target lists.
func (n *normalizer) simpleExistsInput(id ir.RelID, depth uint32) bool {
	if !n.step(1) {
		return false
	}
	if depth >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return false
	}
	relation, _ := n.g.Rel(id)
	switch relation.Kind {
	case ir.Scan, ir.Values, ir.Filter, ir.Project, ir.Join, ir.Apply, ir.GenerateSeries, ir.GenerateSubscripts, ir.Unnest, ir.RowsFrom:
	default:
		return false
	}
	for i := range relation.InputCount() {
		child, _ := n.g.Input(id, i)
		if !n.simpleExistsInput(child, depth+1) {
			return false
		}
	}
	return true
}
