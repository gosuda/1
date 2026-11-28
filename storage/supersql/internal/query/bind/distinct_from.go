package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) distinctFrom(node *zpgparse.A_Expr, s *scope, name string) (expression, error) {
	position := int(node.Location) + 1
	args, err := b.scalarList([]*zpgparse.Node{node.Lexpr, node.Rexpr}, s)
	if err != nil {
		return expression{}, err
	}
	var result expression
	if args[0].rowSyntax && args[1].rowSyntax {
		result, err = b.rowDistinct(s, name, args[0].rowArgs, args[1].rowArgs, position)
	} else {
		result, err = b.scalarDistinct(s, name, args, position)
	}
	if err != nil {
		return expression{}, err
	}
	if node.Kind == zpgparse.A_Expr_Kind_AEXPR_NOT_DISTINCT {
		return b.boolean(s, ir.Not, []expression{result}, position)
	}
	return result, nil
}

func (b *binder) rowDistinct(s *scope, name string, left, right []expression, position int) (expression, error) {
	if len(left) != len(right) {
		return expression{}, bindError("42601", "unequal number of entries in row expressions", position)
	}
	if len(left) == 0 {
		return b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(false), position: position}, ir.Scalar{Kind: ir.Const}, nil)
	}
	if err := b.checkStarExpansion(len(left), position); err != nil {
		return expression{}, err
	}
	comparisons := make([]expression, len(left))
	for i := range left {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		var err error
		comparisons[i], err = b.scalarDistinct(s, name, []expression{left[i], right[i]}, position)
		if err != nil {
			return expression{}, err
		}
	}
	if len(comparisons) == 1 {
		return comparisons[0], nil
	}
	return b.boolean(s, ir.Or, comparisons, position)
}

func (b *binder) scalarDistinct(s *scope, name string, args []expression, position int) (expression, error) {
	comparison, err := b.resolveOperator(name, args, position)
	if err != nil {
		return expression{}, err
	}
	signature, err := b.specialize(comparison.signature, args, position)
	if err != nil {
		return expression{}, err
	}
	if signature.Result != 16 || signature.ReturnsSet {
		return expression{}, bindError("42804", "DISTINCT requires a scalar boolean equality operator", position)
	}
	targets, err := b.functionArguments(signature, args, position)
	if err != nil {
		return expression{}, err
	}
	for i := range args {
		args[i], err = b.coerce(s, args[i], targets[i], ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		args[i], err = b.once(s, args[i])
		if err != nil {
			return expression{}, err
		}
	}
	equal, err := b.invoke(s, signature, comparison.operator, args, ir.Call, 0, position)
	if err != nil {
		return expression{}, err
	}
	different, err := b.boolean(s, ir.Not, []expression{equal}, position)
	if err != nil {
		return expression{}, err
	}
	boolean := b.primitive(16)
	leftNull, err := b.addScalar(s, expression{typ: boolean, position: position}, ir.Scalar{Kind: ir.NullTest}, []expression{args[0]})
	if err != nil {
		return expression{}, err
	}
	rightNull, err := b.addScalar(s, expression{typ: boolean, position: position}, ir.Scalar{Kind: ir.NullTest}, []expression{args[1]})
	if err != nil {
		return expression{}, err
	}
	rightPresent, err := b.boolean(s, ir.Not, []expression{rightNull}, position)
	if err != nil {
		return expression{}, err
	}
	truth, err := b.addScalar(s, expression{typ: boolean, value: model.BoolVal(true), position: position}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return expression{}, err
	}
	nonnullLeft, err := b.addScalar(s, expression{typ: boolean, nullable: different.nullable, position: position}, ir.Scalar{Kind: ir.Case}, []expression{rightNull, truth, different})
	if err != nil {
		return expression{}, err
	}
	return b.addScalar(s, expression{typ: boolean, nullable: nonnullLeft.nullable, position: position}, ir.Scalar{Kind: ir.Case}, []expression{leftNull, rightPresent, nonnullLeft})
}
