package bind

import (
	"math/bits"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type rowComparisonPair struct {
	operator  resolvedOperator
	arguments [2]expression
}

func (b *binder) rowComparison(s *scope, name string, left, right []expression, pos int) (expression, error) {
	if len(left) != len(right) {
		return expression{}, bindError("42601", "unequal number of entries in row expressions", pos)
	}
	if len(left) == 0 {
		return expression{}, bindError("0A000", "cannot compare rows of zero length", pos)
	}
	bytesPerPair := uint64(unsafe.Sizeof(rowComparisonPair{})) + uint64(unsafe.Sizeof(expression{}))
	if uint64(len(left)) > b.options.Limits.MaxBytes/bytesPerPair {
		return expression{}, bindError("54000", "row comparison metadata limit exceeded", pos)
	}
	pairs := make([]rowComparisonPair, len(left))
	for i := range pairs {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		var err error
		pairs[i], err = b.rowComparisonPair(s, name, left[i], right[i], pos)
		if err != nil {
			return expression{}, err
		}
	}
	if len(pairs) == 1 {
		return b.invoke(s, pairs[0].operator.signature, pairs[0].operator.operator, pairs[0].arguments[:], ir.Call, 0, pos)
	}
	common := uint8(0x7e)
	for i := range pairs {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		var strategies uint8
		for interpretation := range builtin.RowComparisonInterpretations(pairs[i].operator.operator) {
			strategies |= 1 << interpretation.Strategy
		}
		common &= strategies
	}
	if common == 0 {
		return expression{}, bindError("0A000", "row comparison operators must belong to compatible btree families", pos)
	}
	strategy := uint8(bits.TrailingZeros8(common))
	comparisons := make([]expression, len(pairs))
	if strategy == 3 || strategy == 6 {
		for i := range pairs {
			var err error
			comparisons[i], err = b.invoke(s, pairs[i].operator.signature, pairs[i].operator.operator, pairs[i].arguments[:], ir.Call, 0, pos)
			if err != nil {
				return expression{}, err
			}
		}
		operation := ir.And
		if strategy == 6 {
			operation = ir.Or
		}
		return b.boolean(s, operation, comparisons, pos)
	}
	nullable := false
	for i := range pairs {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		var function model.FunctionID
		for interpretation := range builtin.RowComparisonInterpretations(pairs[i].operator.operator) {
			if interpretation.Strategy == strategy {
				function = interpretation.CompareFunction
				break
			}
		}
		if function == 0 {
			return expression{}, bindError("42883", "row comparison family has no matching support function", pos)
		}
		signature, known := builtin.LookupSignature(function)
		if !known || signature.ArgumentCount != 2 || signature.Result != 23 || signature.ReturnsSet {
			return expression{}, missing("row comparison support signature", pos)
		}
		var err error
		comparisons[i], err = b.invoke(s, signature, 0, pairs[i].arguments[:], ir.Call, 0, pos)
		if err != nil {
			return expression{}, err
		}
		nullable = nullable || comparisons[i].nullable
	}
	return b.addScalar(s, expression{typ: b.primitive(16), nullable: nullable, position: pos, effects: ir.Effects{MayError: true}}, ir.Scalar{Kind: ir.RowCompare, RowComparison: ir.RowComparisonKind(strategy)}, comparisons)
}

func (b *binder) rowComparisonPair(s *scope, name string, left, right expression, pos int) (rowComparisonPair, error) {
	pair := rowComparisonPair{arguments: [2]expression{left, right}}
	selected, err := b.resolveOperator(name, pair.arguments[:], pos)
	if err != nil {
		return pair, err
	}
	if selected.signature.Result != 16 || selected.signature.ReturnsSet {
		return pair, bindError("42804", "row comparison operator must return scalar boolean", pos)
	}
	selected.signature, err = b.specialize(selected.signature, pair.arguments[:], pos)
	if err != nil {
		return pair, err
	}
	pair.operator = selected
	targets, err := b.functionArguments(selected.signature, pair.arguments[:], pos)
	if err != nil {
		return pair, err
	}
	for i := range pair.arguments {
		pair.arguments[i], err = b.coerce(s, pair.arguments[i], targets[i], ir.ImplicitCast)
		if err != nil {
			return pair, err
		}
	}
	return pair, nil
}
