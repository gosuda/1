package bind

import (
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) setOperation(n *zpgparse.SelectStmt, s *scope) (relation, error) {
	left, err := b.selectStatement(n.Larg, s, nil)
	if err != nil {
		return left, err
	}
	right, err := b.selectStatement(n.Rarg, s, nil)
	if err != nil {
		return right, err
	}
	if len(left.columns) != len(right.columns) {
		return relation{}, bindError("42601", "set-operation inputs have different column counts", 0)
	}
	types := make([]model.TypeLayout, len(left.columns))
	columns := slices.Clone(left.columns)
	for i, c := range left.columns {
		args := []expression{{typ: c.Layout, collation: c.collation}, {typ: right.columns[i].Layout, collation: right.columns[i].collation}}
		typ, err := b.commonType(args)
		if err != nil {
			return relation{}, err
		}
		state, err := b.commonCollation(args)
		if err != nil {
			return relation{}, err
		}
		typ.Type.Collation = state.id
		if n.Op != zpgparse.SetOperation_SETOP_UNION || !n.All {
			if err := b.requireKeyCollation(expression{typ: typ, collation: state}); err != nil {
				return relation{}, err
			}
		}
		columns[i].collation = state
		types[i] = typ
	}
	left, err = b.coerceRelation(left, s, types)
	if err != nil {
		return left, err
	}
	right, err = b.coerceRelation(right, s, types)
	if err != nil {
		return right, err
	}
	// Coercion can widen a branch column's nullability, so the result slot is
	// derived from the coerced branches. Deriving it earlier would publish an
	// output that is less nullable than a branch, which the graph rejects.
	for i := range columns {
		nullable := left.columns[i].Nullable || right.columns[i].Nullable
		slot, err := b.slot(s, types[i], nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i].Layout = types[i]
		columns[i].Nullable = nullable
		columns[i].Slot = slot
		columns[i].qualifier = ""
		columns[i].namespace = ""
		columns[i].hidden = false
	}
	kind := ir.Union
	switch n.Op {
	case zpgparse.SetOperation_SETOP_UNION:
	case zpgparse.SetOperation_SETOP_INTERSECT:
		kind = ir.Intersect
	case zpgparse.SetOperation_SETOP_EXCEPT:
		kind = ir.Except
	default:
		return relation{}, bindError("42601", "invalid set operation", 0)
	}
	effects := left.effects.Union(right.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: s.id, SetKind: kind, All: n.All, Effects: effects}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	return b.selectTail(n, s, relation{id: id, columns: columns, effects: effects})
}
func (b *binder) coerceRelation(input relation, s *scope, types []model.TypeLayout) (relation, error) {
	columns := slices.Clone(input.columns)
	bindings := make([]ir.Binding, len(columns))
	effects := input.effects
	for i, c := range columns {
		e, err := b.reference(s, c, 0)
		if err != nil {
			return relation{}, err
		}
		e, err = b.coerceCommon(s, e, types[i])
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i].Layout = e.typ
		columns[i].Nullable = e.nullable
		columns[i].Slot = slot
		bindings[i] = ir.Binding{Slot: slot, Expr: e.id}
		effects = effects.Union(e.effects)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: effects}, err
}
func (b *binder) with(n *zpgparse.WithClause, s *scope) error {
	namesSeen := make(map[string]bool)
	for _, node := range n.Ctes {
		c := node.GetCommonTableExpr()
		if c == nil {
			return bindError("42601", "invalid CTE", 0)
		}
		pos := int(c.Location) + 1
		if namesSeen[c.Ctename] {
			return bindError("42712", "WITH query name specified more than once", pos)
		}
		namesSeen[c.Ctename] = true
		writable := c.Ctequery.GetInsertStmt() != nil || c.Ctequery.GetUpdateStmt() != nil || c.Ctequery.GetDeleteStmt() != nil || c.Ctequery.GetMergeStmt() != nil
		if writable && n != b.topWith {
			return bindError("0A000", "WITH clause containing a data-modifying statement must be at the top level", pos)
		}
		recursive := n.Recursive && referencesCTE(c.Ctequery, c.Ctename)
		if (c.SearchClause != nil || c.CycleClause != nil) && !recursive {
			clausePosition := pos
			if c.SearchClause != nil {
				clausePosition = int(c.SearchClause.Location) + 1
			} else if c.CycleClause != nil {
				clausePosition = int(c.CycleClause.Location) + 1
			}
			return bindError("42601", "WITH query is not recursive", clausePosition)
		}
		if recursive {
			if writable {
				return bindError("42P19", "recursive query must not contain data-modifying statements", pos)
			}
			if err := b.recursiveCTE(c, s); err != nil {
				return err
			}
			continue
		}
		isolated := &scope{id: s.id, parent: s.parent, ctes: s.ctes}
		body, err := b.statement(c.Ctequery, isolated)
		if err != nil {
			return err
		}
		if len(c.Aliascolnames) > len(body.columns) {
			return bindError("42P10", "too many CTE column aliases", pos)
		}
		for i, node := range c.Aliascolnames {
			body.columns[i].Name = node.GetString_().GetSval()
		}
		materialization := ir.MaterializeDefault
		switch c.Ctematerialized {
		case zpgparse.CTEMaterialize_CTEMaterializeAlways:
			materialization = ir.MaterializeAlways
		case zpgparse.CTEMaterialize_CTEMaterializeNever:
			materialization = ir.MaterializeNever
		}
		id, err := b.builder.DeclareCTE(b.ctx, ir.CTESpec{Scope: s.id, Outputs: outputSlots(body.columns), Materialization: materialization, Effects: body.effects})
		if err != nil {
			return err
		}
		if err := b.builder.BindCTE(b.ctx, id, body.id); err != nil {
			return err
		}
		s.ctes[c.Ctename] = cteBinding{id: id, columns: body.columns, effects: body.effects, noReturning: writable && c.Ctequery.GetInsertStmt().GetReturningClause() == nil && c.Ctequery.GetUpdateStmt().GetReturningClause() == nil && c.Ctequery.GetDeleteStmt().GetReturningClause() == nil && c.Ctequery.GetMergeStmt().GetReturningClause() == nil}
	}
	return nil
}

func statementWith(node *zpgparse.Node) *zpgparse.WithClause {
	switch {
	case node.GetSelectStmt() != nil:
		return node.GetSelectStmt().WithClause
	case node.GetInsertStmt() != nil:
		return node.GetInsertStmt().WithClause
	case node.GetUpdateStmt() != nil:
		return node.GetUpdateStmt().WithClause
	case node.GetDeleteStmt() != nil:
		return node.GetDeleteStmt().WithClause
	case node.GetMergeStmt() != nil:
		return node.GetMergeStmt().WithClause
	default:
		return nil
	}
}
func referencesCTE(node *zpgparse.Node, name string) bool {
	if node == nil {
		return false
	}
	return referencesCTEMessage(node.ProtoReflect(), name)
}

func referencesCTEMessage(message protoreflect.Message, name string) bool {
	// Feature validation has already bounded this AST's traversal depth.
	if !message.IsValid() {
		return false
	}
	var clause *zpgparse.WithClause
	mutation := false
	switch n := message.Interface().(type) {
	case *zpgparse.RangeVar:
		return n.Schemaname == "" && n.Relname == name
	case *zpgparse.SelectStmt:
		clause = n.WithClause
	case *zpgparse.InsertStmt:
		clause, mutation = n.WithClause, true
	case *zpgparse.UpdateStmt:
		clause, mutation = n.WithClause, true
	case *zpgparse.DeleteStmt:
		clause, mutation = n.WithClause, true
	}
	if found, shadowed := referencesWithCTE(clause, name); found || shadowed {
		return found
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		targetRelation := mutation && field.Name() == "relation"
		if field.Kind() != protoreflect.MessageKind || field.Name() == "with_clause" || targetRelation {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len() && !found; i++ {
				found = referencesCTEMessage(list.Get(i).Message(), name)
			}
		} else {
			found = referencesCTEMessage(value.Message(), name)
		}
		return !found
	})
	return found
}

func referencesWithCTE(clause *zpgparse.WithClause, name string) (found, shadowed bool) {
	if clause == nil {
		return false, false
	}
	if clause.Recursive {
		for _, node := range clause.Ctes {
			if c := node.GetCommonTableExpr(); c != nil && c.Ctename == name {
				return false, true
			}
		}
	}
	for _, node := range clause.Ctes {
		c := node.GetCommonTableExpr()
		if c == nil {
			continue
		}
		if referencesCTE(c.Ctequery, name) {
			return true, false
		}
		if c.Ctename == name {
			return false, true
		}
	}
	return false, false
}
