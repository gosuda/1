package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) arraySubscript(source expression, indices []*zpgparse.Node, s *scope) (expression, error) {
	if source.typ.Type.ID == 3802 {
		return b.jsonbSubscript(source, indices, s)
	}
	fetch, _, _, err := b.arraySubscriptPlan(source, indices, s)
	return fetch, err
}

func (b *binder) jsonbSubscript(source expression, indices []*zpgparse.Node, s *scope) (expression, error) {
	text := b.primitive(25)
	keys := make([]expression, len(indices))
	for i, node := range indices {
		key, err := b.jsonbSubscriptIndex(node, s, source.position)
		if err != nil {
			return expression{}, err
		}
		keys[i], err = b.coerce(s, key, text, ir.ExplicitCast)
		if err != nil {
			return expression{}, err
		}
	}
	path := model.TypeLayout{
		Type:     model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray, Collation: text.Type.Collation},
		Children: []model.TypeLayout{text},
	}
	pathExpression, err := b.addScalar(s, expression{typ: path, position: source.position}, ir.Scalar{Kind: ir.Array}, keys)
	if err != nil {
		return expression{}, err
	}
	signature, found := builtin.LookupSignature(3217)
	if !found {
		return expression{}, bindError("42883", "jsonb_extract_path function does not exist", source.position)
	}
	return b.invoke(s, signature, 0, []expression{source, pathExpression}, ir.Call, 0, source.position)
}

func (b *binder) jsonbSubscriptIndex(node *zpgparse.Node, s *scope, position int) (expression, error) {
	index := node.GetAIndices()
	if index == nil || index.IsSlice || index.Uidx == nil || index.Lidx != nil {
		return expression{}, bindError("42601", "invalid jsonb subscript", position)
	}
	key, err := b.scalar(index.Uidx, s)
	if err != nil {
		return expression{}, err
	}
	target := b.primitive(25)
	sourceType := key.typ.Type.ID
	if sourceType == 0 && key.inferred != 0 {
		sourceType = key.inferred
	}
	if sourceType != 0 && sourceType != 705 {
		integer := implicitCast(sourceType, 23)
		text := implicitCast(sourceType, 25)
		if integer == text {
			return expression{}, bindError("42804", "jsonb subscript must be coercible to only one type, integer or text", key.position)
		}
		if integer {
			target = b.primitive(23)
		}
	}
	return b.coerce(s, key, target, ir.ImplicitCast)
}

func (b *binder) jsonbStoreAssignment(s *scope, source expression, indices []*zpgparse.Node, replacement expression, position int) (expression, error) {
	keys := make([]expression, len(indices))
	for i, node := range indices {
		var err error
		keys[i], err = b.jsonbSubscriptIndex(node, s, position)
		if err != nil {
			return expression{}, err
		}
	}
	replacement, err := b.coerce(s, replacement, source.typ, ir.AssignmentCast)
	if err != nil {
		return expression{}, err
	}
	operands := make([]expression, 1, len(keys)+2)
	operands[0] = source
	operands = append(operands, keys...)
	operands = append(operands, replacement)
	result := expression{typ: source.typ, position: position, effects: ir.Effects{MayError: true}}
	return b.addScalar(s, result, ir.Scalar{Kind: ir.JSONBStore}, operands)
}

func (b *binder) arraySubscriptPlan(source expression, indices []*zpgparse.Node, s *scope) (expression, ir.Scalar, []expression, error) {
	position := source.position
	var err error
	slice := false
	for _, node := range indices {
		index := node.GetAIndices()
		if index == nil {
			return expression{}, ir.Scalar{}, nil, missing("field or star indirection", position)
		}
		if !index.IsSlice && (index.Uidx == nil || index.Lidx != nil) {
			return expression{}, ir.Scalar{}, nil, bindError("42601", "invalid array subscript", position)
		}
		slice = slice || index.IsSlice
	}
	if source.typ.Type.Storage != model.TypeArray || len(source.typ.Children) != 1 {
		return expression{}, ir.Scalar{}, nil, missing("non-array subscripting", position)
	}
	if len(indices) > 6 {
		return expression{}, ir.Scalar{}, nil, bindError("54000", "number of array dimensions exceeds the maximum allowed (6)", position)
	}

	payload := ir.Scalar{Kind: ir.ArrayIndex}
	result := source.typ.Children[0]
	if slice {
		payload.Kind = ir.ArraySlice
		payload.Subscripts.Dimensions = uint8(len(indices))
		result = source.typ
	}
	var upper, lower [6]expression
	for i, node := range indices {
		index := node.GetAIndices()
		if slice {
			switch {
			case index.Lidx != nil:
				lower[i], err = b.arraySubscriptBound(index.Lidx, s)
				payload.Subscripts.Lower |= 1 << i
			case !index.IsSlice:
				// In a slice chain, [n] means [1:n], not [n:n].
				lower[i], err = b.addScalar(s, expression{typ: b.primitive(23), value: model.Int64Val(1), position: position}, ir.Scalar{Kind: ir.Const}, nil)
				payload.Subscripts.Lower |= 1 << i
			}
			if err != nil {
				return expression{}, ir.Scalar{}, nil, err
			}
		}
		if index.Uidx != nil {
			upper[i], err = b.arraySubscriptBound(index.Uidx, s)
			if err != nil {
				return expression{}, ir.Scalar{}, nil, err
			}
			if slice {
				payload.Subscripts.Upper |= 1 << i
			}
		}
	}
	var operands [13]expression
	args := operands[:1]
	for i := range indices {
		if upper[i].id != 0 {
			args = append(args, upper[i])
		}
	}
	for i := range indices {
		if lower[i].id != 0 {
			args = append(args, lower[i])
		}
	}
	args[0] = source
	fetch, err := b.addScalar(s, expression{typ: result, nullable: true, position: position, collation: b.expressionCollation(source)}, payload, args)
	if err != nil {
		return expression{}, ir.Scalar{}, nil, err
	}
	if !slice && b.collatable(result.Type) {
		fetch, err = b.relabelCollation(s, fetch, source.typ.Type.Collation)
		if err != nil {
			return expression{}, ir.Scalar{}, nil, err
		}
	}
	return fetch, payload, args, nil
}

func (b *binder) arraySubscriptBound(node *zpgparse.Node, s *scope) (expression, error) {
	bound, err := b.scalar(node, s)
	if err != nil {
		return expression{}, err
	}
	return b.coerce(s, bound, b.primitive(23), ir.AssignmentCast)
}
