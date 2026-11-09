package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func jsonRowsFunction(id model.FunctionID) bool {
	switch id {
	case 1179, 4006, 3208, 3219, 3465, 3931, 3932, 3955, 3957, 3958, 3959, 3969, 3204, 3205, 3209, 3475, 3490, 3491, 3960, 3961, 6338:
		return true
	}
	return false
}

func (b *binder) jsonRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, signature builtin.Signature, args []expression) (relation, error) {
	pos := int(item.call.Location) + 1
	targets, err := b.functionArguments(signature, args, pos)
	if err != nil {
		return relation{}, err
	}
	expressions := make([]ir.ScalarID, len(args))
	effects := signatureEffects(signature)
	effects.MayError = true
	for i, arg := range args {
		value, err := b.coerce(s, arg, targets[i], ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
		expressions[i] = value.id
		effects = effects.Union(value.effects)
	}
	name := item.parts[len(item.parts)-1]
	var columns []column
	var result model.TypeLayout
	switch signature.ID {
	case 3204, 3205, 3490, 3491:
		if len(item.definitions) == 0 {
			return relation{}, bindError("42601", "a column definition list is required for functions returning record", pos)
		}
		columns, err = b.unnestRecordDefinitions(s, item.definitions, pos)
		result = b.primitive(2249)
	case 3209, 3475, 3960, 3961:
		result = targets[0]
		if result.Type.Storage != model.TypeComposite {
			return relation{}, bindError("42804", "first argument must be a row type", pos)
		}
		if len(item.definitions) != 0 {
			return relation{}, bindError("42601", "column definitions are redundant for a composite result", pos)
		}
		if result.Type.ID != 2249 {
			columns, err = b.unnestNamedColumns(s, result, pos)
		} else {
			if len(result.FieldNames) != len(result.Children) {
				return relation{}, bindError("42804", "record argument must have declared field names", pos)
			}
			columns, err = b.rowSourceColumns(s, result.FieldNames, result.Children, pos)
		}
	case 3208, 3932, 3958, 3959:
		if len(item.definitions) != 0 {
			return relation{}, bindError("42601", "a column definition list is redundant for a function with OUT parameters", pos)
		}
		value := b.primitive(25)
		if signature.ID == 3208 {
			value = b.primitive(3802)
		}
		if signature.ID == 3958 {
			value = b.primitive(114)
		}
		columns, err = b.rowSourceColumns(s, []string{"key", "value"}, []model.TypeLayout{b.primitive(25), value}, pos)
		result = b.primitive(2249)
	default:
		if len(item.definitions) != 0 {
			return relation{}, bindError("42601", "column definitions require a record-returning function", pos)
		}
		result = b.primitive(signature.Result)
		if signature.ID == 3219 || signature.ID == 3465 || signature.ID == 3955 || signature.ID == 3969 {
			name = "value"
		} else if alias != nil {
			name = alias.Aliasname
		}
		columns, err = b.rowSourceColumns(s, []string{name}, []model.TypeLayout{result}, pos)
	}
	if err != nil {
		return relation{}, err
	}
	if result.Type.Storage == model.TypeComposite {
		result.Children = make([]model.TypeLayout, len(columns))
		result.FieldNames = make([]string, len(columns))
		for i, c := range columns {
			result.Children[i], result.FieldNames[i] = c.Layout, c.Name
		}
	}
	layout, err := b.layout(result)
	if err != nil {
		return relation{}, err
	}
	if ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Unnest, Scope: s.id, Span: sourceSpan(pos), Effects: effects, RowFunction: signature.ID, RowResult: layout, RowStrict: signature.Strict, UnnestOrdinality: ordinality}, Outputs: outputSlots(columns), Expressions: expressions})
	return relation{id: id, columns: columns, effects: effects}, err
}

func (b *binder) rowSourceColumns(s *scope, names []string, layouts []model.TypeLayout, pos int) ([]column, error) {
	columns, err := b.unnestColumnBuffer(len(layouts), pos)
	if err != nil {
		return nil, err
	}
	for i, layout := range layouts {
		slot, err := b.slot(s, layout, true, 0, 0)
		if err != nil {
			return nil, err
		}
		columns[i].ResultColumn = ResultColumn{Name: names[i], Layout: layout, Slot: slot, Nullable: true}
	}
	return columns, nil
}
