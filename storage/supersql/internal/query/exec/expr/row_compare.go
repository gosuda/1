package expr

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *compiler) rowCompare(ctx context.Context, id ir.ScalarID, n ir.Scalar, dst, depth int) error {
	if err := c.p.emit(instruction{op: opEnter, mask: maskAll}); err != nil {
		return err
	}
	for i := range n.OperandCount() {
		call, _ := c.graph.Operand(id, i)
		comparison, err := c.scalar(ctx, call, depth+1)
		if err != nil {
			return err
		}
		if err := c.p.emit(instruction{op: opRowCompare, dst: dst, args: []int{comparison}, rowComparison: n.RowComparison}); err != nil {
			return err
		}
		if i+1 < n.OperandCount() {
			if err := c.p.emit(instruction{op: opFilter, args: []int{comparison}, mask: maskZero}); err != nil {
				return err
			}
		}
	}
	return c.p.emit(instruction{op: opLeave})
}

func (p *Program) verifyRowCompare(in instruction) error {
	switch in.rowComparison {
	case ir.RowCompareLess, ir.RowCompareLessEqual, ir.RowCompareGreaterEqual, ir.RowCompareGreater:
	default:
		return invalid("invalid row comparison strategy")
	}
	if len(in.args) != 1 || p.registers[in.dst].layout.Type != (model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}) {
		return invalid("row comparison requires int4 input and boolean output")
	}
	input := p.registers[in.args[0]]
	if input.layout.Type != (model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}) || (input.nullable && !p.registers[in.dst].nullable) {
		return invalid("row comparison input type or nullability mismatch")
	}
	return nil
}

func evalRowCompare(e *evaluation, in instruction) error {
	input := &e.state.registers[in.args[0]]
	output := &e.state.registers[in.dst]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		if input.null(lane) {
			output.put(lane, model.NullVal(model.TypeBoolean))
			continue
		}
		comparison := input.vector.Int64[lane]
		var result bool
		switch in.rowComparison {
		case ir.RowCompareLess:
			result = comparison < 0
		case ir.RowCompareLessEqual:
			result = comparison <= 0
		case ir.RowCompareGreaterEqual:
			result = comparison >= 0
		case ir.RowCompareGreater:
			result = comparison > 0
		}
		output.put(lane, model.BoolVal(result))
	}
	return e.ctx.Err()
}
