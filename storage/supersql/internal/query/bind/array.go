package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) arrayConstructor(n *zpgparse.A_ArrayExpr, s *scope, target *model.TypeLayout) (expression, error) {
	position := int(n.Location) + 1
	if uint64(len(n.Elements)) > uint64(b.options.Limits.MaxOperands) {
		return expression{}, bindError("54000", "expression operand limit exceeded", position)
	}
	if target != nil {
		node := &zpgparse.Node{Node: &zpgparse.Node_AArrayExpr{AArrayExpr: n}}
		if grouped, matched, err := b.groupedExpression(node, s, target); matched {
			return grouped, err
		}
	}
	args := make([]expression, len(n.Elements))
	nested := false
	var err error
	for i, node := range n.Elements {
		if array := node.GetAArrayExpr(); array != nil && target != nil {
			if err := b.enter(); err != nil {
				return expression{}, err
			}
			args[i], err = b.arrayConstructor(array, s, target)
			b.depth--
		} else {
			args[i], err = b.scalar(node, s)
		}
		if err != nil {
			return expression{}, err
		}
		// PostgreSQL vectors have array storage but remain scalar elements.
		typ := args[i].typ.Type
		nested = nested || node.GetAArrayExpr() != nil || (typ.Storage == model.TypeArray && typ.ID != 22 && typ.ID != 30)
	}
	var operandType, result model.TypeLayout
	if target != nil {
		result = *target
		if nested {
			operandType = result
		} else {
			operandType = result.Children[0]
		}
	} else {
		if len(args) == 0 {
			return expression{}, bindError("42P18", "cannot determine type of empty array", position)
		}
		operandType, err = b.commonType(args)
		if err != nil {
			return expression{}, err
		}
		if nested {
			result = operandType
		} else {
			id := b.arrayTypeID(operandType.Type.ID)
			if id == 0 {
				return expression{}, missing("array type identity", position)
			}
			result = model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: model.TypeArray, Collation: operandType.Type.Collation}, Children: []model.TypeLayout{operandType}}
		}
	}
	for i := range args {
		if target != nil {
			args[i], err = b.coerce(s, args[i], operandType, ir.ExplicitCast)
		} else {
			args[i], err = b.coerceCommon(s, args[i], operandType)
		}
		if err == nil && target != nil {
			args[i], err = b.relabelCollation(s, args[i], operandType.Type.Collation)
		}
		if err != nil {
			return expression{}, err
		}
	}
	if nested {
		state, err := b.commonCollation(args)
		if err != nil {
			return expression{}, err
		}
		result, _ = b.resultCollation(result, state)
		for i := range args {
			args[i], err = b.relabelCollation(s, args[i], result.Type.Collation)
			if err != nil {
				return expression{}, err
			}
		}
		return b.addScalar(s, expression{typ: result, position: position, effects: ir.Effects{MayError: true}}, ir.Scalar{Kind: ir.ArrayStack}, args)
	}
	return b.addScalar(s, expression{typ: result, position: position}, ir.Scalar{Kind: ir.Array}, args)
}
