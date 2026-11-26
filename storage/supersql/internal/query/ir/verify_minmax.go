package ir

import "github.com/gosuda/supersql-alpha/internal/model"

func (g *Graph) validateMinMax(n Scalar, args []ScalarID) error {
	if len(args) == 0 || !validID(n.Signature, g.signatures) {
		return invalid("min/max requires operands and a comparison signature")
	}
	sig := g.signatures[n.Signature-1]
	if sig.VariadicArray || sig.parameters.count != 2 || !validExtent(sig.parameters, g.layoutRefs) || g.layouts[sig.Result-1].typ.Storage != model.TypeBoolean {
		return invalid("min/max comparison must be binary and boolean")
	}
	if n.Function != sig.Function || n.Operator != sig.Operator || n.Strict != sig.Strict || n.Effects.Union(sig.Effects) != n.Effects {
		return invalid("min/max comparison identity or effects differ from signature")
	}
	result := g.layouts[n.Layout-1]
	for _, id := range sliceAt(g.layoutRefs, sig.parameters) {
		parameter := g.layouts[id-1]
		typ := parameter.typ
		typ.ID = result.typ.ID
		if typ != result.typ || parameter.children.count != result.children.count {
			return invalid("min/max comparison changes the operand representation")
		}
		for i, child := range sliceAt(g.layoutRefs, result.children) {
			if !g.sameLayout(child, g.layoutRefs[int(parameter.children.start)+i]) {
				return invalid("min/max comparison changes nested operand layouts")
			}
		}
	}
	allNullable := true
	for _, id := range args {
		operand := g.scalars[id-1]
		if !g.sameLayout(n.Layout, operand.Layout) {
			return invalid("min/max operand type differs from result")
		}
		allNullable = allNullable && operand.Nullable
	}
	if allNullable && !n.Nullable {
		return invalid("min/max result omits all-NULL input")
	}
	return nil
}
