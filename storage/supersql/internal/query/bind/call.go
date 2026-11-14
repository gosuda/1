package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) callStatement(statement *zpgparse.CallStmt, scope *scope) (relation, error) {
	call := statement.GetFunccall()
	if call == nil || statement.Funcexpr != nil || len(statement.Outargs) != 0 {
		return relation{}, bindError("42601", "expected untransformed CALL statement", 0)
	}
	if call.AggOrder != nil || call.AggFilter != nil || call.Over != nil || call.AggStar || call.AggDistinct || call.AggWithinGroup {
		return relation{}, bindError("42809", "aggregate syntax is not allowed in CALL", int(call.Location)+1)
	}
	parts, err := names(call.Funcname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return relation{}, bindError("42601", "invalid procedure name", int(call.Location)+1)
	}
	arguments, err := b.callArguments(call.Args, scope, int(call.Location)+1)
	if err != nil {
		return relation{}, err
	}
	b.procedureCall = true
	name := parts[len(parts)-1]
	for _, schema := range b.functionPath(parts) {
		value, found, err := b.userRoutine(schema, name, arguments, scope, int(call.Location)+1, call.FuncVariadic, true, false, nil)
		if err != nil {
			return relation{}, err
		}
		if !found {
			continue
		}
		slot, err := b.slot(scope, value.typ, true, 0, 0)
		if err != nil {
			return relation{}, err
		}
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 1, Effects: value.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{value.id}})
		if err != nil {
			return relation{}, err
		}
		outputs := value.routineOutputs
		if len(outputs) == 0 {
			id, err = b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scope.id, Effects: value.effects}, Inputs: []ir.RelID{id}})
			return relation{id: id, effects: value.effects}, err
		}
		if len(outputs) == 1 {
			columnName := outputs[0].Name
			if columnName == "" {
				columnName = name
			}
			resultColumn := column{ResultColumn: ResultColumn{Name: columnName, Layout: value.typ, Nullable: true, Slot: slot}}
			return relation{id: id, columns: []column{resultColumn}, effects: value.effects}, nil
		}
		parent, err := b.addScalar(scope, expression{typ: value.typ, nullable: true, sourceSlot: slot, reads: []model.SlotID{slot}}, ir.Scalar{Kind: ir.Slot, Slot: slot}, nil)
		if err != nil {
			return relation{}, err
		}
		columns := make([]column, len(outputs))
		bindings := make([]ir.Binding, len(outputs))
		for i, output := range outputs {
			field, fieldErr := b.compositeFieldAt(scope, parent, i, int(call.Location)+1)
			if fieldErr != nil {
				return relation{}, fieldErr
			}
			outputSlot, slotErr := b.slot(scope, output.Layout, true, 0, 0)
			if slotErr != nil {
				return relation{}, slotErr
			}
			columnName := output.Name
			if columnName == "" {
				columnName = name
			}
			columns[i] = column{ResultColumn: ResultColumn{Name: columnName, Layout: output.Layout, Nullable: true, Slot: outputSlot}}
			bindings[i] = ir.Binding{Slot: outputSlot, Expr: field.id}
		}
		id, err = b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scope.id, Effects: value.effects}, Inputs: []ir.RelID{id}, Outputs: outputSlots(columns), Bindings: bindings})
		return relation{id: id, columns: columns, effects: value.effects}, err
	}
	return relation{}, bindError("42883", "procedure does not exist", int(call.Location)+1)
}
