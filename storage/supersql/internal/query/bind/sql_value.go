package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) sqlValueFunction(node *zpgparse.SQLValueFunction, s *scope) (expression, error) {
	position := int(node.Location) + 1
	var target model.TypeID
	precision := false
	switch node.Op {
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_USER,
		zpgparse.SQLValueFunctionOp_SVFOP_USER,
		zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_ROLE:
		name := (model.Principal{}).Name()
		if b.options.CurrentRole != "" && b.options.CurrentRole != "none" {
			name = b.options.CurrentRole
		}
		value := model.StringVal(name)
		return b.addScalar(s, expression{typ: b.primitive(19), value: value, position: position}, ir.Scalar{Kind: ir.Const}, nil)
	case zpgparse.SQLValueFunctionOp_SVFOP_SESSION_USER:
		value := model.StringVal((model.Principal{}).Name())
		return b.addScalar(s, expression{typ: b.primitive(19), value: value, position: position}, ir.Scalar{Kind: ir.Const}, nil)
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
		target = 1082
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME, zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
		target = 1266
		precision = node.Op == zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
		target = 1184
		precision = node.Op == zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N
	case zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIME, zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
		target = 1083
		precision = node.Op == zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIME_N
	case zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP, zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
		target = 1114
		precision = node.Op == zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N
	default:
		return expression{}, missing("SQL session or temporal value expression", position)
	}
	layout := b.primitive(target)
	if precision {
		modifier, err := b.scalarTypeModifier(target, []*zpgparse.Node{{Node: &zpgparse.Node_AConst{AConst: &zpgparse.A_Const{Val: &zpgparse.A_Const_Ival{Ival: &zpgparse.Integer{Ival: node.Typmod}}}}}}, position)
		if err != nil {
			return expression{}, err
		}
		layout.Type.Modifier = modifier
	}
	signature, ok := builtin.LookupSignature(1299)
	if !ok {
		return expression{}, missing("transaction timestamp signature", position)
	}
	value, err := b.invoke(s, signature, 0, nil, ir.Call, 0, position)
	if err != nil {
		return expression{}, err
	}
	return b.coerce(s, value, layout, ir.ExplicitCast)
}

func sqlValueFunctionName(op zpgparse.SQLValueFunctionOp) string {
	switch op {
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
		return "current_date"
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME, zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
		return "current_time"
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
		return "current_timestamp"
	case zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIME, zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
		return "localtime"
	case zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP, zpgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
		return "localtimestamp"
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_USER:
		return "current_user"
	case zpgparse.SQLValueFunctionOp_SVFOP_SESSION_USER:
		return "session_user"
	case zpgparse.SQLValueFunctionOp_SVFOP_USER:
		return "user"
	case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_ROLE:
		return "current_role"
	default:
		return "?column?"
	}
}
