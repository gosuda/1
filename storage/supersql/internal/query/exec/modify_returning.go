package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) returningSourceColumns(graph *ir.Graph, roots []ir.ScalarID, slots []physical.Slot) ([]int, error) {
	prepared := &c.prepared.nodes[c.current]
	if !c.preparing {
		return prepared.returningSources, nil
	}
	if err := c.charge(128); err != nil {
		return nil, err
	}
	seen := make(map[ir.ScalarID]bool)
	needed := make(map[model.SlotID]bool)
	var visit func(ir.ScalarID, int) error
	visit = func(id ir.ScalarID, depth int) error {
		if err := c.portal.ctx.Err(); err != nil {
			return err
		}
		if depth > c.portal.env.Limits.Expressions.MaxDepth {
			return expr.ErrLimit
		}
		if seen[id] {
			return nil
		}
		if err := c.charge(64); err != nil {
			return err
		}
		seen[id] = true
		node, ok := graph.Scalar(id)
		if !ok {
			return ErrInvalid
		}
		if node.Kind == ir.Slot && !needed[node.Slot] {
			if err := c.charge(64); err != nil {
				return err
			}
			needed[node.Slot] = true
		}
		if node.Kind == ir.Subquery {
			body, ok := graph.Rel(node.Relation)
			if !ok {
				return ErrInvalid
			}
			for i := range graph.CaptureCount(body.Scope) {
				slot, _ := graph.Capture(body.Scope, i)
				if needed[slot] {
					continue
				}
				if err := c.charge(64); err != nil {
					return err
				}
				needed[slot] = true
			}
		}
		for i := range node.OperandCount() {
			operand, _ := graph.Operand(id, i)
			if err := visit(operand, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := visit(root, 0); err != nil {
			return nil, err
		}
	}
	count := 0
	for _, slot := range slots {
		if needed[slot.ID] {
			count++
		}
	}
	if err := c.charge(uint64(count) * uint64(unsafe.Sizeof(int(0)))); err != nil {
		return nil, err
	}
	columns := make([]int, 0, count)
	for i, slot := range slots {
		if needed[slot.ID] {
			columns = append(columns, i)
		}
	}
	prepared.returningSources = columns
	return columns, nil
}
