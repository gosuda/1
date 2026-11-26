package bind

import (
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) syntaxOperator(s *scope, name string, args []expression, pos int) (expression, error) {
	if len(args) != 2 || !args[0].rowSyntax || !args[1].rowSyntax {
		return b.operator(s, name, args, pos)
	}
	return b.rowComparison(s, name, args[0].rowArgs, args[1].rowArgs, pos)
}

func (b *binder) onceComparisonArgument(s *scope, input expression) (expression, error) {
	if !input.rowSyntax {
		return b.once(s, input)
	}
	fields := make([]expression, len(input.rowArgs))
	rowFields := fields
	for _, field := range input.rowArgs {
		if field.id == 0 {
			rowFields = make([]expression, len(fields))
			break
		}
	}
	for i, field := range input.rowArgs {
		value, err := b.coerce(s, field, input.typ.Children[i], ir.ImplicitCast)
		if err != nil {
			return input, err
		}
		fields[i], err = b.once(s, value)
		if err != nil {
			return input, err
		}
		rowFields[i] = fields[i]
		if field.id == 0 {
			fields[i] = field
		}
	}
	row, err := b.addScalar(s, expression{typ: input.typ, position: input.position}, ir.Scalar{Kind: ir.Row}, rowFields)
	if err != nil {
		return input, err
	}
	result, err := b.once(s, row)
	result.record, result.rowArgs, result.rowSyntax = recordConstructor, fields, true
	return result, err
}
