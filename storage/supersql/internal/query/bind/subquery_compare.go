package bind

import (
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type subqueryComparisonPair struct {
	left      expression
	right     expression
	signature builtin.Signature
	operator  model.OperatorID
}

func (b *binder) quantifiedSubquery(n *zpgparse.SubLink, s, child *scope, rel relation) (expression, error) {
	left, err := b.scalar(n.Testexpr, s)
	if err != nil {
		return expression{}, err
	}
	name, err := subqueryOperatorName(n.OperName)
	if err != nil {
		return expression{}, err
	}
	kind := ir.AnySubquery
	if n.SubLinkType == zpgparse.SubLinkType_ALL_SUBLINK {
		kind = ir.AllSubquery
	}
	return b.comparisonSubquery(s, child, rel, left, name, kind, int(n.Location)+1)
}

func (b *binder) comparisonSubquery(s, child *scope, rel relation, left expression, name string, kind ir.SubqueryKind, pos int) (expression, error) {
	leftArgs := []expression{left}
	rowSyntax := left.rowSyntax
	if rowSyntax {
		leftArgs = left.rowArgs
	}
	if len(leftArgs) != len(rel.columns) {
		return expression{}, bindError("42601", "unequal number of entries in row expressions", pos)
	}
	if len(leftArgs) == 0 {
		return expression{}, bindError("0A000", "cannot compare rows of zero length", pos)
	}
	if len(rel.columns) > 1 && !rowSyntax {
		return expression{}, bindError("42601", "subquery has too many columns", pos)
	}
	pairs := make([]subqueryComparisonPair, len(leftArgs))
	commonStrategies := uint8(0x7e)
	for i := range pairs {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		args := []expression{leftArgs[i], {typ: rel.columns[i].Layout, nullable: rel.columns[i].Nullable}}
		selected, err := b.resolveOperator(name, args, pos)
		if err != nil {
			return expression{}, err
		}
		if selected.signature.Result != 16 || selected.signature.ReturnsSet {
			return expression{}, bindError("42804", "subquery comparison operator must return scalar boolean", pos)
		}
		selected.signature, err = b.specialize(selected.signature, args, pos)
		if err != nil {
			return expression{}, err
		}
		pairs[i] = subqueryComparisonPair{left: args[0], right: args[1], signature: selected.signature, operator: selected.operator}
		if rowSyntax {
			strategies := uint8(0)
			for interpretation := range builtin.RowComparisonInterpretations(selected.operator) {
				strategies |= 1 << interpretation.Strategy
			}
			commonStrategies &= strategies
		}
	}
	strategy := ir.RowComparisonKind(0)
	if rowSyntax {
		if commonStrategies == 0 {
			return expression{}, bindError("0A000", "row comparison operators must belong to compatible btree families", pos)
		}
		strategy = ir.RowComparisonKind(bits.TrailingZeros8(commonStrategies))
	}
	for i := range pairs {
		if strategy != 0 && strategy != ir.RowCompareEqual && strategy != ir.RowCompareNotEqual {
			var function model.FunctionID
			for interpretation := range builtin.RowComparisonInterpretations(pairs[i].operator) {
				if ir.RowComparisonKind(interpretation.Strategy) == strategy {
					function = interpretation.CompareFunction
					break
				}
			}
			signature, known := builtin.LookupSignature(function)
			if !known || signature.ArgumentCount != 2 || signature.Result != 23 || signature.ReturnsSet {
				return expression{}, bindError("42883", "row comparison family has no matching support function", pos)
			}
			pairs[i].signature, pairs[i].operator = signature, 0
		}
		args := []expression{pairs[i].left, pairs[i].right}
		signature, err := b.specialize(pairs[i].signature, args, pos)
		if err != nil {
			return expression{}, err
		}
		targets, err := b.functionArguments(signature, args, pos)
		if err != nil {
			return expression{}, err
		}
		pairs[i].left, err = b.coerce(s, pairs[i].left, targets[0], ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		pairs[i].right.typ = targets[1]
		pairs[i].signature = signature
	}
	targets := make([]model.TypeLayout, len(pairs))
	for i := range pairs {
		targets[i] = pairs[i].right.typ
	}
	coerced, err := b.coerceRelation(rel, child, targets)
	if err != nil {
		return expression{}, err
	}
	rel = coerced
	comparisons := make([]ir.SignatureID, len(pairs))
	args := make([]expression, len(pairs))
	effects := rel.effects
	for i := range pairs {
		pairs[i].right.typ = rel.columns[i].Layout
		state, err := b.commonCollation([]expression{pairs[i].left, pairs[i].right})
		if err != nil {
			return expression{}, err
		}
		result := b.primitive(16)
		if strategy != 0 && strategy != ir.RowCompareEqual && strategy != ir.RowCompareNotEqual {
			result = b.primitive(23)
		}
		comparisons[i], err = b.signature(pairs[i].signature, pairs[i].operator, []model.TypeLayout{pairs[i].left.typ, pairs[i].right.typ}, result, state.id, false)
		if err != nil {
			return expression{}, err
		}
		args[i] = pairs[i].left
		effects = effects.Union(pairs[i].left.effects).Union(signatureEffects(pairs[i].signature))
	}
	if kind == ir.RowSubquery {
		effects.MayError = true
	}
	result := expression{typ: b.primitive(16), nullable: true, effects: effects, position: pos, reads: b.captures[child.id]}
	payload := ir.Scalar{Kind: ir.Subquery, SubqueryKind: kind, Relation: rel.id, RowComparison: strategy}
	return b.addScalarComparisons(s, result, payload, args, comparisons)
}

func (b *binder) quantifiedArray(n *zpgparse.A_Expr, s *scope, name string, pos int) (expression, error) {
	left, err := b.scalar(n.Lexpr, s)
	if err != nil {
		return expression{}, err
	}
	array, err := b.scalar(n.Rexpr, s)
	if err != nil {
		return expression{}, err
	}
	var element expression
	if array.typ.Type.ID != 0 {
		if array.typ.Type.Storage != model.TypeArray || len(array.typ.Children) != 1 {
			return expression{}, bindError("42809", "op ANY/ALL requires array on right side", pos)
		}
		element.typ = array.typ.Children[0]
	}
	arguments := []expression{left, element}
	selected, err := b.resolveOperator(name, arguments, pos)
	if err != nil {
		return expression{}, err
	}
	if selected.signature.Result != 16 || selected.signature.ReturnsSet {
		return expression{}, bindError("42804", "ANY/ALL comparison operator must return scalar boolean", pos)
	}
	selected.signature, err = b.specialize(selected.signature, arguments, pos)
	if err != nil {
		return expression{}, err
	}
	targets, err := b.functionArguments(selected.signature, arguments, pos)
	if err != nil {
		return expression{}, err
	}
	left, err = b.coerce(s, left, targets[0], ir.ImplicitCast)
	if err != nil {
		return expression{}, err
	}
	arrayTarget := array.typ
	if arrayTarget.Type.ID == 0 || !sameType(arrayTarget.Children[0], targets[1]) {
		arrayTarget, err = b.companionArrayLayout(targets[1], pos)
		if err != nil {
			return expression{}, err
		}
	}
	array, err = b.coerce(s, array, arrayTarget, ir.ImplicitCast)
	if err != nil {
		return expression{}, err
	}
	state, err := b.commonCollation([]expression{left, {typ: targets[1]}})
	if err != nil {
		return expression{}, err
	}
	signature, err := b.signature(selected.signature, selected.operator, []model.TypeLayout{left.typ, targets[1]}, b.primitive(16), state.id, false)
	if err != nil {
		return expression{}, err
	}
	kind := ir.AnySubquery
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_OP_ALL {
		kind = ir.AllSubquery
	}
	effects := left.effects.Union(array.effects).Union(signatureEffects(selected.signature))
	result := expression{typ: b.primitive(16), nullable: true, effects: effects, position: pos}
	payload := ir.Scalar{Kind: ir.ArrayQuantified, SubqueryKind: kind, Signature: signature, Operator: selected.operator, Strict: selected.signature.Strict}
	return b.addScalar(s, result, payload, []expression{left, array})
}

func (b *binder) companionArrayLayout(element model.TypeLayout, pos int) (model.TypeLayout, error) {
	id := b.arrayTypeID(element.Type.ID)
	if id != 0 {
		return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: model.TypeArray, Collation: element.Type.Collation}, Children: []model.TypeLayout{element}}, nil
	}
	descriptor, err := b.findNamedType(element.Type.ID, pos)
	if err != nil || descriptor == nil {
		if err != nil {
			return model.TypeLayout{}, err
		}
		return model.TypeLayout{}, bindError("42704", "could not find array type for input data type", pos)
	}
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return model.TypeLayout{}, err
	}
	if spec.Type == nil || spec.Type.Array == 0 || b.catalog == nil {
		return model.TypeLayout{}, bindError("42704", "could not find array type for input data type", pos)
	}
	published, err := b.catalog.Descriptor(b.ctx, spec.Type.Array)
	if err != nil {
		return model.TypeLayout{}, err
	}
	arraySpec, err := b.descriptorSpec(published.Descriptor)
	if err != nil {
		return model.TypeLayout{}, err
	}
	if arraySpec.Type == nil || arraySpec.Type.Layout.Type.Storage != model.TypeArray || len(arraySpec.Type.Layout.Children) != 1 || !sameType(arraySpec.Type.Layout.Children[0], element) {
		return model.TypeLayout{}, model.ErrInvalidDescriptor
	}
	b.dependency(descriptor, 0)
	b.dependency(published.Descriptor, 0)
	return arraySpec.Type.Layout, nil
}

func subqueryOperatorName(nodes []*zpgparse.Node) (string, error) {
	name := "="
	if len(nodes) == 0 {
		return name, nil
	}
	parts, err := names(nodes)
	if err != nil {
		return "", err
	}
	if len(parts) == 0 || len(parts) > 2 || len(parts) == 2 && parts[0] != "pg_catalog" {
		return "", bindError("42601", "invalid operator name", 0)
	}
	return parts[len(parts)-1], nil
}
