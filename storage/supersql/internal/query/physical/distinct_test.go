package physical

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestDistinctOnOrdersOnlyDistinctKeysButPreservesRowChoice(t *testing.T) {
	f := newFixture(t)
	first, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		t.Fatal(err)
	}
	one := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	two := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(2)})
	key := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: first}})
	values := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{first, second}, Expressions: []ir.ScalarID{one, two}})
	distinct := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Distinct, Scope: f.scope, DistinctOn: true}, Inputs: []ir.RelID{values}, Outputs: []model.SlotID{first, second}, Expressions: []ir.ScalarID{key}, Order: []ir.OrderKey{{Expr: key, Direction: ir.Descending, Nulls: ir.NullsFirst, Operator: 413}}})
	output := []Slot{{Graph: 1, ID: first, Layout: f.layout}, {Graph: 1, ID: second, Layout: f.layout}}
	ordered := single()
	ordered.Ordering = []OrderKey{{Key: Key{Slot: output[0], Type: f.typ}, Direction: ir.Descending, Nulls: ir.NullsFirst, Operator: 413}}
	source := NodeSpec{Logical: RelRef{1, values}, Algorithm: Values, Output: output, Delivered: single(), Break: Streaming}
	sort := NodeSpec{Logical: source.Logical, Algorithm: Sort, Children: []NodeID{1}, Output: output, Required: []Properties{single()}, Delivered: ordered, Break: MaterializeInput, Memory: Memory{PeakBytes: 1024}}
	dedup := NodeSpec{Logical: RelRef{1, distinct}, Algorithm: StreamDistinct, Children: []NodeID{2}, Output: output, Required: []Properties{ordered}, Delivered: ordered, Break: Streaming, Memory: Memory{PeakBytes: 1024}}
	spec := f.spec(source, sort, dedup)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	root, _ := plan.Node(plan.Root())
	order, _ := root.Delivered().Ordering(0)
	if order.Key.Slot.ID != first || order.Direction != ir.Descending {
		t.Fatalf("representative ordering=%+v", order)
	}
	spec.Nodes[1].Delivered.Ordering[0].Direction = ir.Ascending
	requireInvalid(t, spec)
}
