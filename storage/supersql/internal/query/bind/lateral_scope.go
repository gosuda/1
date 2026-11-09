package bind

import "github.com/gosuda/supersql-alpha/internal/zpgparse"

// RIGHT/FULL LATERAL inputs may be independent of the left input, or refer to
// higher query levels. The immediate left namespace is visible only to diagnose
// forbidden references; it owns no IR scope and cannot become a capture owner.
func (b *binder) outerLateralSource(sub *zpgparse.RangeSubselect, s *scope, left relation) (relation, error) {
	forbidden := &scope{parent: s.parent, columns: left.columns, namespaces: left.namespaces, lateralForbidden: true}
	isolated := &scope{id: s.id, parent: forbidden, ctes: s.ctes}
	right, err := b.selectStatement(sub.Subquery.GetSelectStmt(), isolated, nil)
	if err != nil {
		return relation{}, err
	}
	return b.aliasRelation(right, isolated, sub.Alias, "")
}

func (s *scope) starScope(qualifier relationQualifier, position int) (*scope, error) {
	for current := s; current != nil; current = current.parent {
		_, found, err := current.relationNamespace(qualifier, position)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if current.lateralForbidden {
			return nil, bindError("42P10", "LATERAL reference requires INNER or LEFT join", position)
		}
		return current, nil
	}
	return nil, bindError("42P01", "star relation does not exist", position)
}

func (owner *scope) starColumn(current *scope, column column) column {
	if owner != current && owner.inAggregate {
		return column
	}
	if grouped, ok := owner.groupSlots[column.Slot]; ok {
		return grouped
	}
	return column
}
