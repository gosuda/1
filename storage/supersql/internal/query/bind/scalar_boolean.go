package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) booleanTest(n *zpgparse.BooleanTest, s *scope) (expression, error) {
	pos := int(n.Location) + 1
	arg, err := b.scalar(n.Arg, s)
	if err != nil {
		return arg, err
	}
	arg, err = b.coerce(s, arg, b.primitive(16), ir.ImplicitCast)
	if err != nil {
		return arg, err
	}
	if n.Booltesttype == zpgparse.BoolTestType_IS_UNKNOWN || n.Booltesttype == zpgparse.BoolTestType_IS_NOT_UNKNOWN {
		return b.addScalar(s, expression{typ: b.primitive(16), position: pos}, ir.Scalar{Kind: ir.NullTest, Negated: n.Booltesttype == zpgparse.BoolTestType_IS_NOT_UNKNOWN}, []expression{arg})
	}
	if n.Booltesttype == zpgparse.BoolTestType_IS_FALSE || n.Booltesttype == zpgparse.BoolTestType_IS_NOT_FALSE {
		arg, err = b.boolean(s, ir.Not, []expression{arg}, pos)
		if err != nil {
			return arg, err
		}
	}
	fallback, err := b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(false)}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return arg, err
	}
	result, err := b.addScalar(s, expression{typ: b.primitive(16), position: pos}, ir.Scalar{Kind: ir.Coalesce}, []expression{arg, fallback})
	if err != nil {
		return result, err
	}
	if n.Booltesttype == zpgparse.BoolTestType_IS_NOT_TRUE || n.Booltesttype == zpgparse.BoolTestType_IS_NOT_FALSE {
		return b.boolean(s, ir.Not, []expression{result}, pos)
	}
	if n.Booltesttype != zpgparse.BoolTestType_IS_TRUE && n.Booltesttype != zpgparse.BoolTestType_IS_FALSE {
		return result, bindError("42601", "invalid boolean test", pos)
	}
	return result, nil
}
