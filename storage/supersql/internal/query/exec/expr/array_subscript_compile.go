package expr

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *compiler) arrayFetch(ctx context.Context, id ir.ScalarID, node ir.Scalar, destination, depth int) error {
	sourceID, _ := c.graph.Operand(id, 0)
	source, err := c.scalar(ctx, sourceID, depth+1)
	if err != nil {
		return err
	}
	guarded := c.p.registers[source].nullable
	if guarded {
		if err := c.p.emit(instruction{op: opEnter, args: []int{source}, mask: maskNotNull}); err != nil {
			return err
		}
	}
	in := instruction{op: opArrayIndex, dst: destination, args: []int{source}}
	if node.Kind == ir.ArraySlice {
		in.op, in.subscripts = opArraySlice, node.Subscripts
	}
	for i := uint32(1); i < node.OperandCount(); i++ {
		operand, _ := c.graph.Operand(id, i)
		register, err := c.scalar(ctx, operand, depth+1)
		if err != nil {
			return err
		}
		in.args = append(in.args, register)
	}
	if err := c.p.emit(in); err != nil {
		return err
	}
	if guarded {
		return c.p.emit(instruction{op: opLeave})
	}
	return nil
}
