package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) groupingFunction(node *zpgparse.GroupingFunc, s *scope) (expression, error) {
	position := int(node.Location) + 1
	if !s.allowAggregate || s.inAggregate {
		return expression{}, bindError("42803", "grouping operations are not allowed in this scope", position)
	}
	if len(node.Args) == 0 {
		return expression{}, bindError("42601", "GROUPING requires an argument", position)
	}
	if len(node.Args) > 31 {
		return expression{}, bindError("54000", "GROUPING must have fewer than 32 arguments", position)
	}
	indices := make([]uint32, len(node.Args))
	for i, argument := range node.Args {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		value, err := b.scalar(argument, s)
		if err != nil {
			return expression{}, err
		}
		found := false
		for index, column := range s.groupColumns {
			if value.sourceSlot != 0 && value.sourceSlot == column.Slot {
				indices[i], found = uint32(index), true
				break
			}
		}
		if !found {
			return expression{}, bindError("42803", "arguments to GROUPING must be grouping expressions of the associated query level", position)
		}
	}
	for i, call := range s.groupings {
		if slices.Equal(call.Args, indices) {
			return b.reference(s, s.groupingColumns[i], position)
		}
	}
	layout := b.primitive(23)
	slot, err := b.slot(s, layout, false, 0, 0)
	if err != nil {
		return expression{}, err
	}
	column := column{ResultColumn: ResultColumn{Name: "grouping", Layout: layout, Slot: slot}}
	s.groupings = append(s.groupings, ir.GroupingSpec{GroupingCall: ir.GroupingCall{Result: slot}, Args: indices})
	s.groupingColumns = append(s.groupingColumns, column)
	return b.reference(s, column, position)
}
