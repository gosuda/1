package normalize

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (n *normalizer) composeProjection(id ir.RelID, source ir.Rel, first relResult, depth uint32) (ir.RelID, relResult) {
	if source.Kind != ir.Project || source.SecurityBarrier || source.InputCount() != 1 || source.BindingCount() != source.OutputCount() {
		return 0, first
	}
	input, _ := n.g.Input(id, 0)
	project, _ := n.g.Rel(input)
	if project.Kind != ir.Project || project.SecurityBarrier || project.Scope != source.Scope || project.BindingCount() != project.OutputCount() {
		return 0, first
	}
	for i := range source.BindingCount() {
		binding, _ := n.g.Binding(id, i)
		safe, _ := n.projectionScalar(binding.Expr, source.Scope, input, depth+1)
		if !safe {
			return 0, first
		}
	}
	if !n.simpleProjectionInput(input, source.Scope, depth+1) {
		return 0, first
	}
	base, _ := n.g.Input(input, 0)
	return input, n.rels[base]
}

// Only error-free immutable target lists may be substituted: composition can
// otherwise duplicate or discard observable evaluation.
func (n *normalizer) simpleProjectionInput(id ir.RelID, scope ir.ScopeID, depth uint32) bool {
	if !n.step(1) {
		return false
	}
	if depth >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return false
	}
	r, _ := n.g.Rel(id)
	if r.Kind != ir.Project || r.SecurityBarrier || r.Scope != scope || r.Effects.Volatility == ir.Volatile {
		return false
	}
	for i := range r.BindingCount() {
		binding, _ := n.g.Binding(id, i)
		if safe, _ := n.projectionScalar(binding.Expr, scope, 0, depth+1); !safe {
			return false
		}
	}
	return true
}

func (n *normalizer) projectionScalar(id ir.ScalarID, scope ir.ScopeID, input ir.RelID, depth uint32) (bool, bool) {
	if id == 0 {
		return true, false
	}
	if !n.step(1) {
		return false, false
	}
	if depth >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return false, false
	}
	s, _ := n.g.Scalar(id)
	if s.Scope != scope || s.Relation != 0 || s.Kind == ir.Once || s.Effects.Volatility == ir.Volatile {
		return false, false
	}
	found := false
	if input != 0 && s.Kind == ir.Slot {
		binding := n.projectedExpression(input, s.Slot)
		projected, _ := n.g.Scalar(binding)
		found = binding != 0 && (projected.Kind != ir.Slot || projected.Slot != s.Slot)
	}
	if input != 0 && s.Kind == ir.Field {
		operand, _ := n.g.Operand(id, 0)
		parent, _ := n.g.Scalar(operand)
		if parent.Kind == ir.Slot {
			binding := n.projectedExpression(input, parent.Slot)
			row, _ := n.g.Scalar(binding)
			found = row.Kind == ir.Row
		}
	}
	for i := range s.OperandCount() {
		operand, _ := n.g.Operand(id, i)
		safe, field := n.projectionScalar(operand, scope, input, depth+1)
		if !safe {
			return false, false
		}
		found = found || field
	}
	return true, found
}

func (n *normalizer) projectedExpression(input ir.RelID, slot model.SlotID) ir.ScalarID {
	project, _ := n.g.Rel(input)
	for i := range project.BindingCount() {
		if !n.step(1) {
			return 0
		}
		binding, _ := n.g.Binding(input, i)
		if binding.Slot == slot {
			return binding.Expr
		}
	}
	return 0
}

func (n *normalizer) projectedSlot(slot model.SlotID, depth uint32) (scalarResult, bool) {
	if n.projectInput == 0 {
		return scalarResult{}, false
	}
	expression := n.projectedExpression(n.projectInput, slot)
	if expression == 0 {
		return scalarResult{}, false
	}
	source, _ := n.g.Scalar(expression)
	if source.Kind == ir.Slot && source.Slot == slot {
		return scalarResult{}, false
	}
	input := n.projectInput
	n.projectInput = 0
	result := n.scalar(expression, depth+1)
	n.projectInput = input
	return result, true
}
