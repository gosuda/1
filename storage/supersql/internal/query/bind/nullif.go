package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) nullIfExpression(node *zpgparse.A_Expr, s *scope, name string) (expression, error) {
	position := int(node.Location) + 1
	args, err := b.scalarList([]*zpgparse.Node{node.Lexpr, node.Rexpr}, s)
	if err != nil {
		return expression{}, err
	}
	comparison, err := b.resolveOperator(name, args, position)
	if err != nil {
		return expression{}, err
	}
	signature, err := b.specialize(comparison.signature, args, position)
	if err != nil {
		return expression{}, err
	}
	if signature.Result != 16 || signature.ReturnsSet {
		return expression{}, bindError("42804", "NULLIF requires a scalar boolean equality operator", position)
	}
	first, err := b.coerce(s, args[0], b.argumentType(signature.Arguments[0], args[0]), ir.ImplicitCast)
	if err != nil {
		return expression{}, err
	}
	first, err = b.once(s, first)
	if err != nil {
		return expression{}, err
	}
	args[0] = first
	equal, err := b.invoke(s, signature, comparison.operator, args, ir.Call, 0, position)
	if err != nil {
		return expression{}, err
	}
	null, err := b.addScalar(s, expression{typ: first.typ, nullable: true, value: model.NullVal(first.typ.Type.Storage), position: position, collation: b.expressionCollation(first)}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return expression{}, err
	}
	return b.addScalar(s, expression{typ: first.typ, nullable: true, position: position}, ir.Scalar{Kind: ir.Case}, []expression{equal, null, first})
}
