package bind

import (
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) betweenExpression(n *zpgparse.A_Expr, s *scope) (expression, error) {
	pos := int(n.Location) + 1
	bounds := n.Rexpr.GetList()
	if n.Lexpr == nil || bounds == nil || len(bounds.Items) != 2 || bounds.Items[0] == nil || bounds.Items[1] == nil {
		return expression{}, bindError("42601", "invalid BETWEEN expression", pos)
	}

	operators := [2]string{">=", "<="}
	inner, outer := ir.And, ir.Or
	switch n.Kind {
	case zpgparse.A_Expr_Kind_AEXPR_BETWEEN, zpgparse.A_Expr_Kind_AEXPR_BETWEEN_SYM:
	case zpgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN, zpgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		operators = [2]string{"<", ">"}
		inner, outer = ir.Or, ir.And
	default:
		return expression{}, bindError("42601", "invalid BETWEEN expression", pos)
	}

	bindBounds := func(lower, upper *zpgparse.Node) (expression, error) {
		var comparisons [2]expression
		for i, bound := range [2]*zpgparse.Node{lower, upper} {
			// PostgreSQL expands each occurrence separately, including volatile operands.
			left, err := b.scalar(n.Lexpr, s)
			if err != nil {
				return expression{}, err
			}
			right, err := b.scalar(bound, s)
			if err != nil {
				return expression{}, err
			}
			comparisons[i], err = b.syntaxOperator(s, operators[i], []expression{left, right}, pos)
			if err != nil {
				return expression{}, err
			}
		}
		return b.boolean(s, inner, comparisons[:], pos)
	}

	forward, err := bindBounds(bounds.Items[0], bounds.Items[1])
	if err != nil {
		return expression{}, err
	}
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_BETWEEN || n.Kind == zpgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN {
		return forward, nil
	}
	reverse, err := bindBounds(bounds.Items[1], bounds.Items[0])
	if err != nil {
		return expression{}, err
	}
	return b.boolean(s, outer, []expression{forward, reverse}, pos)
}
