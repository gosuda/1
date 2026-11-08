package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) groupedExpression(node *zpgparse.Node, s *scope, arrayTarget *model.TypeLayout) (expression, bool, error) {
	if s.inAggregate {
		return expression{}, false, nil
	}
	for i, group := range s.groupNodes {
		if group == nil {
			continue
		}
		if arrayTarget != nil {
			if !sameArrayConstructorType(*arrayTarget, s.groupColumns[i].Layout) {
				continue
			}
			if cast := group.GetTypeCast(); cast != nil {
				group = cast.Arg
			}
		}
		if !sameGroupingTarget(node, group, s) {
			continue
		}
		value, err := b.reference(s, s.groupColumns[i], 0)
		value.collation = s.groupColumns[i].collation
		return value, true, err
	}
	return expression{}, false, nil
}

func sameArrayConstructorType(target, grouped model.TypeLayout) bool {
	if target.Type.Storage != model.TypeArray || grouped.Type.Storage != model.TypeArray || len(target.Children) != 1 || len(grouped.Children) != 1 {
		return false
	}
	// A type cast preserves SQL collation derivation; physical child relabels
	// do not make an otherwise identical grouped array expression different.
	target.Type.Collation = grouped.Type.Collation
	if target.Type != grouped.Type {
		return false
	}
	element := target.Children[0]
	element.Type.Collation = grouped.Children[0].Type.Collation
	return sameType(element, grouped.Children[0])
}
