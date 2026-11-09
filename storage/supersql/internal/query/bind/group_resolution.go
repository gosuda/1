package bind

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func starReference(node *zpgparse.Node) *zpgparse.ColumnRef {
	ref := node.GetColumnRef()
	if ref == nil || len(ref.Fields) == 0 || ref.Fields[len(ref.Fields)-1].GetAStar() == nil {
		return nil
	}
	return ref
}

func starQualifier(ref *zpgparse.ColumnRef, hasFrom bool) (relationQualifier, error) {
	if len(ref.Fields) == 1 {
		if !hasFrom {
			return relationQualifier{}, bindError("42601", "SELECT star requires a FROM relation", int(ref.Location)+1)
		}
		return relationQualifier{}, nil
	}
	if len(ref.Fields) > 3 || ref.Fields[len(ref.Fields)-2].GetString_() == nil {
		return relationQualifier{}, bindError("42601", "invalid star qualification", int(ref.Location)+1)
	}
	qualifier := relationQualifier{name: ref.Fields[len(ref.Fields)-2].GetString_().GetSval()}
	if len(ref.Fields) == 3 {
		if ref.Fields[0].GetString_() == nil {
			return relationQualifier{}, bindError("42601", "invalid star qualification", int(ref.Location)+1)
		}
		qualifier.namespace = ref.Fields[0].GetString_().GetSval()
	}
	return qualifier, nil
}

func (b *binder) groupingExpression(node *zpgparse.Node, statement *zpgparse.SelectStmt, s *scope, input []column, work *uint64) (*zpgparse.Node, expression, error) {
	if constant := node.GetAConst(); constant != nil {
		if constant.GetIval() == nil {
			return nil, expression{}, bindError("42601", "non-integer constant in GROUP BY", int(constant.Location)+1)
		}
		return b.groupingOrdinal(int(constant.GetIval().Ival), int(constant.Location)+1, statement, s, input, work)
	}
	resolved := node
	if ref := node.GetColumnRef(); ref != nil && len(ref.Fields) == 1 && ref.Fields[0].GetString_() != nil {
		// GROUP BY prefers local input names; unlike ORDER BY, an output alias
		// cannot shadow a column of the FROM relation. Outer captures come later.
		local := *s
		local.parent = nil
		_, err := local.lookup([]string{ref.Fields[0].GetString_().GetSval()}, int(ref.Location)+1)
		if err != nil {
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "42703" {
				return nil, expression{}, err
			}
			alias, err := b.groupingAlias(ref, statement, s, work)
			if err != nil {
				return nil, expression{}, err
			}
			if alias != nil {
				resolved = alias
			}
		}
	}
	value, err := b.targetScalar(resolved, s)
	return resolved, value, err
}

func (b *binder) groupingOrdinal(ordinal, position int, statement *zpgparse.SelectStmt, s *scope, input []column, work *uint64) (*zpgparse.Node, expression, error) {
	if ordinal < 1 {
		return nil, expression{}, bindError("42P10", "GROUP BY position is not in select list", position)
	}
	for _, node := range statement.TargetList {
		if err := b.groupingStep(work); err != nil {
			return nil, expression{}, err
		}
		target := node.GetResTarget()
		if target == nil {
			return nil, expression{}, bindError("42601", "invalid SELECT target", position)
		}
		star := starReference(target.Val)
		if star == nil {
			ordinal--
			if ordinal == 0 {
				value, err := b.targetScalar(target.Val, s)
				return target.Val, value, err
			}
			continue
		}
		qualifier, err := starQualifier(star, len(statement.FromClause) != 0)
		if err != nil {
			return nil, expression{}, err
		}
		owner, candidates := s, input
		if qualifier.name != "" {
			owner, err = s.starScope(qualifier, int(star.Location)+1)
			if err != nil {
				return nil, expression{}, err
			}
			candidates = owner.columns
		}
		for _, column := range candidates {
			if err := b.groupingStep(work); err != nil {
				return nil, expression{}, err
			}
			if !column.visibleToStar(qualifier) {
				continue
			}
			ordinal--
			if ordinal == 0 {
				value, err := b.reference(s, owner.starColumn(s, column), position)
				return nil, value, err
			}
		}
	}
	return nil, expression{}, bindError("42P10", "GROUP BY position is not in select list", position)
}

func (b *binder) groupingAlias(ref *zpgparse.ColumnRef, statement *zpgparse.SelectStmt, s *scope, work *uint64) (*zpgparse.Node, error) {
	name := ref.Fields[0].GetString_().GetSval()
	var selected *zpgparse.Node
	for _, node := range statement.TargetList {
		if err := b.groupingStep(work); err != nil {
			return nil, err
		}
		target := node.GetResTarget()
		if target == nil || starReference(target.Val) != nil || targetName(target) != name {
			continue
		}
		if selected != nil && !sameGroupingTarget(selected, target.Val, s) {
			return nil, bindError("42702", "GROUP BY column is ambiguous", int(ref.Location)+1)
		}
		selected = target.Val
	}
	return selected, nil
}

func sameGroupingTarget(left, right *zpgparse.Node, s *scope) bool {
	if equivalentAST(left.ProtoReflect(), right.ProtoReflect()) {
		return true
	}
	return equivalentScopedAST(left.ProtoReflect(), right.ProtoReflect(), s)
}

func sameGroupingColumns(a, b *zpgparse.ColumnRef, s *scope) bool {
	leftNames, err := names(a.Fields)
	if err != nil {
		return false
	}
	rightNames, err := names(b.Fields)
	if err != nil {
		return false
	}
	leftColumn, err := s.lookup(leftNames, int(a.Location)+1)
	if err != nil {
		return false
	}
	rightColumn, err := s.lookup(rightNames, int(b.Location)+1)
	return err == nil && leftColumn.field == nil && rightColumn.field == nil && leftColumn.Slot == rightColumn.Slot
}

func (b *binder) groupingStep(work *uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if *work == 0 {
		return bindError("54000", "grouping resolution limit exceeded", 0)
	}
	*work -= 1
	return nil
}
