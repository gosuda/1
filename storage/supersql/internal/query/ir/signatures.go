package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (b *Builder) AddSignature(ctx context.Context, spec SignatureSpec) (SignatureID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	s := spec.Signature
	if s.parameters != (extent{}) || (s.Function == 0) == (s.Operator == 0) || !validID(s.Result, b.graph.layouts) || s.Effects.Volatility > Volatile {
		return 0, b.fail(invalid("invalid resolved signature"))
	}
	if len(spec.Parameters) > 100 {
		return 0, b.fail(invalid("signature exceeds function argument limit"))
	}
	for _, id := range spec.Parameters {
		if err := ctx.Err(); err != nil {
			return 0, b.fail(err)
		}
		if !validID(id, b.graph.layouts) {
			return 0, b.fail(invalid("invalid signature parameter layout"))
		}
	}
	if s.VariadicArray {
		if s.Operator != 0 || len(spec.Parameters) == 0 {
			return 0, b.fail(invalid("VARIADIC requires a function and final array parameter"))
		}
		last := b.graph.layouts[spec.Parameters[len(spec.Parameters)-1]-1]
		if last.typ.Storage != model.TypeArray || last.children.count != 1 {
			return 0, b.fail(invalid("VARIADIC requires a final array parameter"))
		}
	}
	var err error
	if s.parameters, err = appendArena(b, &b.graph.layoutRefs, spec.Parameters, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	id, err := appendOne(b, &b.graph.signatures, s, b.limits.MaxNodes)
	return SignatureID(id), err
}
func (g *Graph) validateInvocation(id SignatureID, args extent, result LayoutID, nullable bool, effects Effects) error {
	if !validID(id, g.signatures) || !validExtent(args, g.scalarRefs) {
		return invalid("invalid invocation signature or arguments")
	}
	s := g.signatures[id-1]
	if !validExtent(s.parameters, g.layoutRefs) || args.count != s.parameters.count {
		return invalid("invocation arity, result, or effects differ from signature")
	}
	omitsNullability := s.Nullable && !nullable
	if !g.sameLayout(result, s.Result) || omitsNullability || effects.Union(s.Effects) != effects {
		return invalid("invocation arity, result, or effects differ from signature")
	}
	for i, arg := range sliceAt(g.scalarRefs, args) {
		if !validID(arg, g.scalars) {
			return invalid("invalid invocation argument")
		}
		n := g.scalars[arg-1]
		omitsStrictNullability := s.Strict && n.Nullable && !nullable
		if !g.sameLayout(n.Layout, g.layoutRefs[int(s.parameters.start)+i]) || effects.Union(n.Effects) != effects || omitsStrictNullability {
			return invalid("invocation argument type, effects, or nullability mismatch")
		}
	}
	return nil
}
func (g *Graph) validateSplitInvocation(id SignatureID, direct, args extent, result LayoutID, nullable bool, effects Effects) error {
	if !validID(id, g.signatures) || !validExtent(direct, g.scalarRefs) || !validExtent(args, g.scalarRefs) {
		return invalid("invalid invocation signature or arguments")
	}
	s := g.signatures[id-1]
	if !validExtent(s.parameters, g.layoutRefs) || direct.count+args.count != s.parameters.count {
		return invalid("invocation arity, result, or effects differ from signature")
	}
	omitsNullability := s.Nullable && !nullable
	if !g.sameLayout(result, s.Result) || omitsNullability || effects.Union(s.Effects) != effects {
		return invalid("invocation arity, result, or effects differ from signature")
	}
	index := 0
	for _, part := range [...]extent{direct, args} {
		for _, arg := range sliceAt(g.scalarRefs, part) {
			if !validID(arg, g.scalars) {
				return invalid("invalid invocation argument")
			}
			n := g.scalars[arg-1]
			omitsStrictNullability := s.Strict && n.Nullable && !nullable
			if !g.sameLayout(n.Layout, g.layoutRefs[int(s.parameters.start)+index]) || effects.Union(n.Effects) != effects || omitsStrictNullability {
				return invalid("invocation argument type, effects, or nullability mismatch")
			}
			index++
		}
	}
	return nil
}
func (g *Graph) SignatureCount() uint32                     { return uint32(len(g.signatures)) }
func (g *Graph) Signature(id SignatureID) (Signature, bool) { return atID(g.signatures, id) }
func (s Signature) ParameterCount() uint32                  { return s.parameters.count }
func (g *Graph) SignatureParameter(id SignatureID, index uint32) (LayoutID, bool) {
	s, ok := g.Signature(id)
	if !ok {
		return 0, false
	}
	return atRange(g.layoutRefs, s.parameters, index)
}
