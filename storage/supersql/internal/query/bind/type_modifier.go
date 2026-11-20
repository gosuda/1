package bind

import (
	"errors"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) scalarTypeModifier(id model.TypeID, nodes []*zpgparse.Node, pos int) (int32, error) {
	switch id {
	case 1700, 1186:
		if len(nodes) < 1 || len(nodes) > 2 {
			return 0, bindError("22023", "invalid type modifier", pos)
		}
	case 1083, 1114, 1184, 1266:
		if len(nodes) != 1 {
			return 0, bindError("22023", "invalid temporal type modifier", pos)
		}
	case 1042, 1043:
		if len(nodes) != 1 {
			return 0, bindError("22023", "invalid character type modifier", pos)
		}
	case 1560, 1562:
		if len(nodes) != 1 {
			return 0, bindError("22023", "invalid bit string type modifier", pos)
		}
	default:
		return 0, missing("type modifiers for resolved type", pos)
	}
	var values [2]int32
	for i, node := range nodes {
		constant := node.GetAConst()
		if constant == nil || constant.Isnull || constant.GetIval() == nil {
			return 0, bindError("42601", "type modifiers must be simple integer constants", pos)
		}
		values[i] = constant.GetIval().Ival
	}
	if id == 1083 || id == 1114 || id == 1184 || id == 1266 {
		var modifier int32
		var warning *model.Error
		var err error
		if id == 1083 || id == 1266 {
			modifier, warning, err = builtin.TimePrecision(values[0])
		} else {
			modifier, warning, err = builtin.TimestampPrecision(values[0], id == 1184)
		}
		if err != nil {
			var sqlErr *model.Error
			if errors.As(err, &sqlErr) {
				positioned := *sqlErr
				positioned.Position = pos
				return 0, &positioned
			}
			return 0, err
		}
		if warning != nil {
			warning.Position = pos
			if err := b.addWarning(*warning); err != nil {
				return 0, err
			}
		}
		return modifier, nil
	}
	if id == 1186 {
		modifier, warning, err := builtin.IntervalTypeModifier(values[:len(nodes)])
		if err != nil {
			return 0, err
		}
		if warning != nil {
			warning.Position = pos
			if err := b.addWarning(*warning); err != nil {
				return 0, err
			}
		}
		return modifier, nil
	}
	if id == 1700 {
		precision, scale := values[0], values[1]
		if precision < 1 || precision > 1000 {
			return 0, bindError("22023", "NUMERIC precision must be between 1 and 1000", pos)
		}
		if scale < -1000 || scale > 1000 {
			return 0, bindError("22023", "NUMERIC scale must be between -1000 and 1000", pos)
		}
		return (precision<<16 | scale&2047) + 4, nil
	}
	if id == 1560 || id == 1562 {
		if values[0] < 1 || values[0] > 83886080 {
			return 0, bindError("22023", "bit string length must be between 1 and 83886080", pos)
		}
		return values[0], nil
	}
	if values[0] < 1 || values[0] > 10485760 {
		return 0, bindError("22023", "character length must be between 1 and 10485760", pos)
	}
	return values[0] + 4, nil
}

func (b *binder) castFunction(s *scope, value expression, target model.TypeLayout, function model.FunctionID, context ir.CastContext) (expression, error) {
	sig, ok := builtin.LookupSignature(function)
	if !ok {
		return value, missing("cast function signature metadata", value.position)
	}
	invalidArity := sig.ArgumentCount < 1 || sig.ArgumentCount > 3
	invalidModifier := sig.ArgumentCount >= 2 && sig.Arguments[1] != 23
	invalidExplicitness := sig.ArgumentCount == 3 && sig.Arguments[2] != 16
	if invalidArity || invalidModifier || invalidExplicitness {
		return value, missing("cast function signature form", value.position)
	}
	args := []expression{value}
	if sig.ArgumentCount >= 2 {
		modifier, err := b.addScalar(s, expression{typ: b.primitive(23), value: model.Int64Val(int64(target.Type.Modifier))}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return value, err
		}
		args = append(args, modifier)
	}
	if sig.ArgumentCount == 3 {
		explicit, err := b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(context == ir.ExplicitCast)}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return value, err
		}
		args = append(args, explicit)
	}
	resultType := target
	if sig.Result != target.Type.ID {
		cast, ok := builtin.LookupCast(sig.Result, target.Type.ID)
		if !ok || cast.Method != 'b' {
			return value, missing("cast function result compatibility", value.position)
		}
		resultType = b.primitive(sig.Result)
	}
	kind := ir.Call
	var castContext ir.CastContext
	if sig.ArgumentCount == 1 {
		kind = ir.Cast
		castContext = context
	}
	result, err := b.invokeResult(s, sig, 0, args, kind, castContext, value.position, resultType)
	if err != nil || sig.Result == target.Type.ID {
		return result, err
	}
	target.Type.Collation = result.typ.Type.Collation
	return b.coerceBase(s, result, target, context)
}

func castAcceptsModifier(source, target model.TypeID) bool {
	cast, ok := builtin.LookupCast(source, target)
	if !ok || cast.Method != 'f' || cast.Function == 0 {
		return false
	}
	signature, ok := builtin.LookupSignature(cast.Function)
	if !ok || signature.ArgumentCount < 2 {
		return false
	}
	return signature.Arguments[1] == 23
}
