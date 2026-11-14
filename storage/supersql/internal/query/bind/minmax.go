package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func minMaxName(op zpgparse.MinMaxOp) string {
	switch op {
	case zpgparse.MinMaxOp_IS_GREATEST:
		return "greatest"
	case zpgparse.MinMaxOp_IS_LEAST:
		return "least"
	default:
		return ""
	}
}

func (b *binder) minMax(n *zpgparse.MinMaxExpr, s *scope) (expression, error) {
	pos := int(n.Location) + 1
	kind, operator := ir.Least, "<"
	switch n.Op {
	case zpgparse.MinMaxOp_IS_GREATEST:
		kind, operator = ir.Greatest, ">"
	case zpgparse.MinMaxOp_IS_LEAST:
	default:
		return expression{}, bindError("42601", "invalid min/max expression", pos)
	}
	args, err := b.scalarList(n.Args, s)
	if err != nil {
		return expression{}, err
	}
	if len(args) == 0 {
		return expression{}, bindError("42601", "min/max expression requires an argument", pos)
	}
	typ, err := b.commonType(args)
	if err != nil {
		return expression{}, err
	}
	nullable := true
	for i := range args {
		args[i], err = b.coerceCommon(s, args[i], typ)
		if err != nil {
			return expression{}, err
		}
		nullable = nullable && args[i].nullable
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, err
	}
	typ, state = b.resultCollation(typ, state)
	comparison := []expression{args[0], args[0]}
	selected, err := b.resolveOperator(operator, comparison, pos)
	if err != nil {
		return expression{}, err
	}
	sig, err := b.specialize(selected.signature, comparison, pos)
	if err != nil {
		return expression{}, err
	}
	var parameters [2]model.TypeLayout
	for i := range parameters {
		parameters[i] = typ
		if sig.Arguments[i] == typ.Type.ID {
			continue
		}
		cast, ok := builtin.LookupCast(typ.Type.ID, sig.Arguments[i])
		if !ok || cast.Context != 'i' || cast.Method != 'b' {
			return expression{}, missing("min/max comparison without value conversion", pos)
		}
		// Binary-compatible comparison identities must not change the selected value's SQL type.
		parameters[i].Type.ID = sig.Arguments[i]
	}
	id, err := b.signature(sig, selected.operator, parameters[:], b.primitive(16), state.id, false)
	if err != nil {
		return expression{}, err
	}
	return b.addScalar(s, expression{typ: typ, collation: state, nullable: nullable, effects: signatureEffects(sig), position: pos}, ir.Scalar{Kind: kind, Signature: id, Operator: selected.operator, Strict: sig.Strict}, args)
}
