package bind

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) unnestElement(argument model.TypeLayout, pos int) (model.TypeLayout, error) {
	if argument.Type.Storage == model.TypeArray && len(argument.Children) == 1 {
		return argument.Children[0], nil
	}
	if family, ok := builtin.LookupRangeFamily(argument.Type.ID); ok && argument.Type.ID == family.Multirange && argument.Type.Storage == model.TypeBytes && len(argument.Children) == 0 {
		return b.primitive(family.Range), nil
	}
	return model.TypeLayout{}, bindError("42804", "unnest requires an array or multirange argument", pos)
}

func (b *binder) unnestColumns(s *scope, element model.TypeLayout, definitions []*zpgparse.Node, alias *zpgparse.Alias, name string, pos int) ([]column, error) {
	if element.Type.Storage != model.TypeComposite {
		if len(definitions) != 0 {
			return nil, bindError("42601", "column definitions require a record-returning function", pos)
		}
		columns, err := b.unnestColumnBuffer(1, pos)
		if err != nil {
			return nil, err
		}
		if alias != nil {
			name = alias.Aliasname
		}
		columns[0].ResultColumn = ResultColumn{Name: name, Layout: element, Nullable: true}
		columns[0].Slot, err = b.slot(s, element, true, 0, 0)
		return columns, err
	}
	if element.Type.ID == 2249 {
		if len(definitions) == 0 {
			return nil, bindError("42601", "a column definition list is required for functions returning record", pos)
		}
		return b.unnestRecordDefinitions(s, definitions, pos)
	}
	if len(definitions) != 0 {
		return nil, bindError("42601", "column definitions are redundant for a named composite result", pos)
	}
	return b.unnestNamedColumns(s, element, pos)
}

func (b *binder) unnestColumnBuffer(count int, pos int) ([]column, error) {
	capacity := uint64(count) + 1
	if capacity > uint64(b.options.Limits.MaxOperands) || capacity > uint64(b.options.Limits.MaxSlots) || capacity > b.options.Limits.MaxBytes/uint64(unsafe.Sizeof(column{})) {
		return nil, bindError("54000", "unnest column limit exceeded", pos)
	}
	return make([]column, count, count+1), nil
}

func (b *binder) unnestRecordDefinitions(s *scope, definitions []*zpgparse.Node, pos int) ([]column, error) {
	columns, err := b.unnestColumnBuffer(len(definitions), pos)
	if err != nil {
		return nil, err
	}
	for i, node := range definitions {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		definition := node.GetColumnDef()
		if definition == nil || definition.Colname == "" {
			return nil, bindError("42601", "invalid record column definition", pos)
		}
		for _, previous := range columns[:i] {
			if b.nodes >= b.options.Limits.MaxNodes {
				return nil, bindError("54000", "record definition work limit exceeded", pos)
			}
			b.nodes++
			if previous.Name == definition.Colname {
				return nil, bindError("42701", "record column name specified more than once", int(definition.Location)+1)
			}
		}
		layout, err := b.typeName(definition.TypeName)
		if err != nil {
			return nil, err
		}
		if definition.CollClause != nil {
			layout.Type.Collation, err = b.columnCollation(definition.CollClause, layout)
			if err != nil {
				return nil, err
			}
		}
		slot, err := b.slot(s, layout, true, 0, 0)
		if err != nil {
			return nil, err
		}
		columns[i].ResultColumn = ResultColumn{Name: definition.Colname, Layout: layout, Nullable: true, Slot: slot}
	}
	return columns, nil
}
