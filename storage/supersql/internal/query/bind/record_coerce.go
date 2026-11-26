package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) coerceRecord(s *scope, source expression, target model.TypeLayout, context ir.CastContext) (expression, error) {
	if source.record != recordConstructor {
		return source, bindError("42846", "cannot cast record expression to composite type", source.position)
	}
	if err := b.validateIOLayout(target, source.position); err != nil {
		return source, err
	}
	if err := b.checkStarExpansion(len(target.Children), source.position); err != nil {
		return source, err
	}
	fields := make([]expression, len(target.Children))
	for i, child := range target.Children {
		if i >= len(source.rowArgs) {
			return source, bindError("42846", "cannot cast record: input has too few columns", source.position)
		}
		field := source.rowArgs[i]
		if !b.canCoerceRecordField(field.typ, child, context) {
			return source, bindError("42846", "cannot cast record field to declared type", field.position)
		}
		converted, err := b.coerce(s, field, child, context)
		if err != nil {
			return source, err
		}
		// The field declaration, not its expression, owns the result collation.
		if !sameType(converted.typ, child) {
			declared := converted.typ
			declared.Type.Collation = child.Type.Collation
			if !sameType(declared, child) {
				return source, bindError("42846", "record field differs from declared type", field.position)
			}
			converted, err = b.relabelCollation(s, converted, child.Type.Collation)
			if err != nil {
				return source, err
			}
		}
		fields[i] = converted
	}
	if len(source.rowArgs) > len(fields) {
		return source, bindError("42846", "cannot cast record: input has too many columns", source.position)
	}
	return b.addScalar(s, expression{typ: target, position: source.position}, ir.Scalar{Kind: ir.Row}, fields)
}

func (b *binder) canCoerceRecordField(source, target model.TypeLayout, context ir.CastContext) bool {
	if source.Type.ID == 0 || source.Type.ID == 705 || source.Type.ID == target.Type.ID {
		return true
	}
	compositePair := source.Type.Storage == model.TypeComposite && target.Type.Storage == model.TypeComposite
	if compositePair && (source.Type.ID == 2249 || target.Type.ID == 2249) {
		return true
	}
	if cast, ok := builtin.LookupCast(source.Type.ID, target.Type.ID); ok {
		return context == ir.ExplicitCast || cast.Context == 'i' || context == ir.AssignmentCast && cast.Context == 'a'
	}
	arrayPair := source.Type.Storage == model.TypeArray && target.Type.Storage == model.TypeArray
	arrayLayouts := len(source.Children) == 1 && len(target.Children) == 1
	if arrayPair && arrayLayouts && target.Type.ID != 22 && target.Type.ID != 30 {
		return b.canCoerceRecordField(source.Children[0], target.Children[0], context)
	}
	return b.canCoerceViaIO(source, target, context)
}
