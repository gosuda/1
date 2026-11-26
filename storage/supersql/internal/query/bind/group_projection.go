package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) projectGrouping(input relation, s *scope, expressions []expression) (relation, error) {
	var err error
	for i := range expressions {
		expressions[i], err = b.defaultType(s, expressions[i])
		if err != nil {
			return input, err
		}
	}
	lowered, rewritten, _, err := b.projectSetExpressions(input, s, expressions)
	if err != nil {
		return input, err
	}
	input = lowered
	copy(expressions, rewritten)
	computed := false
	for _, expression := range expressions {
		computed = computed || expression.sourceSlot == 0
	}
	if !computed {
		return input, nil
	}
	columns := slices.Clone(input.columns)
	bindings := make([]ir.Binding, len(columns), len(columns)+len(expressions))
	for i, c := range columns {
		reference, err := b.reference(s, c, 0)
		if err != nil {
			return input, err
		}
		bindings[i] = ir.Binding{Slot: c.Slot, Expr: reference.id}
	}
	effects := input.effects
	for i, expression := range expressions {
		if expression.sourceSlot != 0 {
			continue
		}
		value := expressions[i]
		slot, err := b.slot(s, value.typ, value.nullable, 0, 0)
		if err != nil {
			return input, err
		}
		c := column{ResultColumn: ResultColumn{Layout: value.typ, Nullable: value.nullable, Slot: slot}, hidden: true, record: value.record, recordFields: projectedRecordFields(value)}
		columns = append(columns, c)
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
		effects = effects.Union(value.effects)
		expressions[i], err = b.reference(s, c, value.position)
		expressions[i].collation = b.expressionCollation(value)
		if err != nil {
			return input, err
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: effects, namespaces: input.namespaces}, err
}

func (b *binder) tailExpression(node *zpgparse.Node, statement *zpgparse.SelectStmt, s *scope, columns []column) (expression, error) {
	if c, ok := s.tailColumns[node]; ok {
		return b.targetReference(s, c, 0)
	}
	// SQL99 sort expressions reuse a projected target rather than reaching through
	// its output boundary to unavailable input slots or reevaluating the target.
	if len(statement.TargetList) == len(columns) {
		for i, target := range statement.TargetList {
			if err := b.ctx.Err(); err != nil {
				return expression{}, err
			}
			value := target.GetResTarget()
			if value != nil && value.Val != nil && equivalentAST(node.ProtoReflect(), value.Val.ProtoReflect()) {
				return b.targetReference(s, columns[i], 0)
			}
		}
	}
	return b.outputExpression(node, s, columns)
}
