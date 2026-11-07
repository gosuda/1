package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func regexpRowsFunction(id model.FunctionID) bool {
	switch id {
	case 2763, 2764, 2765, 2766:
		return true
	default:
		return false
	}
}

func (b *binder) regexpRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, signature builtin.Signature, arguments []expression) (relation, error) {
	position := int(item.call.Location) + 1
	if len(item.definitions) != 0 {
		return relation{}, bindError("42601", "column definitions require a record-returning function", position)
	}
	targets, err := b.functionArguments(signature, arguments, position)
	if err != nil {
		return relation{}, err
	}
	expressions := make([]ir.ScalarID, len(arguments))
	effects := signatureEffects(signature)
	effects.MayError = true
	for index, argument := range arguments {
		value, err := b.coerce(s, argument, targets[index], ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
		expressions[index] = value.id
		arguments[index] = value
		effects = effects.Union(value.effects)
	}
	collation, err := b.commonCollation(arguments)
	if err != nil {
		return relation{}, err
	}
	result := b.primitive(signature.Result)
	name := item.parts[len(item.parts)-1]
	if alias != nil {
		name = alias.Aliasname
	}
	columns, err := b.rowSourceColumns(s, []string{name}, []model.TypeLayout{result}, position)
	if err != nil {
		return relation{}, err
	}
	resultLayout, err := b.layout(result)
	if err != nil {
		return relation{}, err
	}
	if ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{
		Kind: ir.Unnest, Scope: s.id, Span: sourceSpan(position), Effects: effects,
		RowFunction: signature.ID, RowResult: resultLayout, RowCollation: collation.id, RowStrict: signature.Strict, UnnestOrdinality: ordinality,
	}, Outputs: outputSlots(columns), Expressions: expressions})
	return relation{id: id, columns: columns, effects: effects}, err
}
