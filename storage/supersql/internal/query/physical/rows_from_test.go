package physical

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestRowsFromRejectsUnprovenDelivery(t *testing.T) {
	for _, defect := range []string{"one input", "swapped input", "expression", "rewind", "ordering", "third input distribution"} {
		t.Run(defect, func(t *testing.T) {
			spec, typ := rowsFromPlanSpec(t)
			root := &spec.Nodes[3]
			switch defect {
			case "one input":
				root.Children, root.Required = root.Children[:1], root.Required[:1]
			case "swapped input":
				root.Children[0], root.Children[2] = root.Children[2], root.Children[0]
			case "expression":
				root.Expressions = spec.Nodes[0].Expressions
			case "rewind":
				root.Delivered.Rewindability = Rewindable
			case "ordering":
				root.Delivered.Ordering = []OrderKey{{Key: Key{Slot: root.Output[0], Type: typ}, Operator: 412, Direction: ir.Ascending, Nulls: ir.NullsLast}}
			case "third input distribution":
				child := spec.Nodes[2]
				partitioned := single()
				partitioned.Distribution = Distribution{Kind: HashPartitioned, Partitions: 2, Keys: []Key{{Slot: child.Output[0], Type: typ}}, Function: 1, Epoch: 1}
				exchange := NodeSpec{Logical: child.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{3}, Output: child.Output, Delivered: partitioned, Required: []Properties{single()}, Break: ExchangeBoundary, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 64}, Memory: Memory{WorkingBytes: 64, PeakBytes: 64}}
				root.Children[2], root.Required[2] = 4, partitioned
				spec.Nodes = append(spec.Nodes[:3], exchange, *root)
				spec.Root = 5
			}
			requireInvalid(t, spec)
		})
	}
}

func rowsFromPlanSpec(t *testing.T) (Spec, model.Type) {
	t.Helper()
	f := newFixture(t)
	var nodes []NodeSpec
	logical := ir.RelSpec{Rel: ir.Rel{Kind: ir.RowsFrom, Scope: f.scope, Effects: ir.Effects{MayError: true}}}
	root := NodeSpec{Algorithm: RowsFrom, Delivered: single(), Break: Streaming}
	for i := range 3 {
		input, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		output, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		value := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(int64(i))})
		inputRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{input}, Expressions: []ir.ScalarID{value}})
		nodes = append(nodes, NodeSpec{Logical: RelRef{1, inputRel}, Algorithm: Values, Output: []Slot{{Graph: 1, ID: input, Layout: f.layout}}, Expressions: []ScalarRef{{1, value}}, Delivered: single(), Break: Streaming})
		logical.Inputs = append(logical.Inputs, inputRel)
		logical.Outputs = append(logical.Outputs, output)
		root.Children = append(root.Children, NodeID(i+1))
		root.Required = append(root.Required, single())
		root.Output = append(root.Output, Slot{Graph: 1, ID: output, Layout: f.layout, Nullable: true})
	}
	root.Logical = RelRef{1, f.rel(logical)}
	return f.spec(append(nodes, root)...), f.typ
}
