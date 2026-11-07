package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func pushdownOrderedGraph(t *testing.T, reader storage.SnapshotReader, request storage.PushdownRequest, limit bool) *fragment.FragmentGraph {
	t.Helper()
	base := scanGraph(t, reader, request, true, false).Plan()
	logical, _ := base.Graph(1)
	builder, err := ir.Extend(t.Context(), logical, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]physical.NodeSpec, base.NodeCount())
	for i := range nodes {
		nodes[i], err = base.CloneNode(t.Context(), physical.NodeID(i+1), 4<<20)
		if err != nil {
			t.Fatal(err)
		}
	}
	input := nodes[base.Root()-1]
	slot := input.Output[0]
	info, _ := logical.Slot(slot.ID)
	typ, _ := logical.Type(slot.Layout)
	expression, err := builder.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: info.Scope, Layout: slot.Layout, Slot: slot.ID}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Sort, Scope: info.Scope}, Inputs: []ir.RelID{logical.Root()}, Outputs: []model.SlotID{slot.ID}, Order: []ir.OrderKey{{Expr: expression, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}})
	if err != nil {
		t.Fatal(err)
	}
	properties := input.Delivered
	properties.Ordering = []physical.OrderKey{{Key: physical.Key{Slot: slot, Type: typ}, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}
	nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.Sort, Children: []physical.NodeID{base.Root()}, Output: []physical.Slot{slot}, Required: []physical.Properties{input.Delivered}, Delivered: properties, Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 4 << 20, Spillable: true}})
	if limit {
		count, err := builder.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: info.Scope, Layout: slot.Layout}, Value: model.Int64Val(1)})
		if err != nil {
			t.Fatal(err)
		}
		root, err = builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: info.Scope, Count: count, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{root}, Outputs: []model.SlotID{slot.ID}})
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.Limit, Children: []physical.NodeID{physical.NodeID(len(nodes))}, Output: []physical.Slot{slot}, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: count}}, Required: []physical.Properties{properties}, Delivered: properties, Break: physical.Streaming, Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Count: 1}})
	}
	logical, err = builder.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{logical}, Nodes: nodes, Root: physical.NodeID(len(nodes))}, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
