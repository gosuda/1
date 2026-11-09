package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) coerceArray(s *scope, input expression, target model.TypeLayout, context ir.CastContext) (expression, error) {
	element, err := b.addScalar(s, expression{
		typ: input.typ.Children[0], nullable: true, position: input.position,
		collation: b.expressionCollation(input),
	}, ir.Scalar{Kind: ir.ArrayElement}, nil)
	if err != nil {
		return input, err
	}
	converted, err := b.coerce(s, element, target.Children[0], context)
	if err != nil {
		return input, err
	}
	converted, err = b.relabelCollation(s, converted, target.Children[0].Type.Collation)
	if err != nil {
		return input, err
	}
	target.Children = []model.TypeLayout{converted.typ}
	return b.addScalar(s, expression{
		typ: target, nullable: input.nullable, position: input.position,
		collation: b.expressionCollation(converted),
	}, ir.Scalar{Kind: ir.ArrayCoerce, CastContext: context}, []expression{input, converted})
}
