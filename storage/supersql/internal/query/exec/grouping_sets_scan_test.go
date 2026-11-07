package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestGroupingSetsReplaysSelectedNativeMorsels(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		sets     [][]uint32
		want     [][]model.Value
		distinct bool
	}{
		{"plain", [][]uint32{{0}}, [][]model.Value{{model.Int64Val(3), model.Int64Val(1)}, {model.Int64Val(4), model.Int64Val(1)}}, false},
		{"grouping sets", [][]uint32{{0}, {}}, [][]model.Value{{model.Int64Val(3), model.Int64Val(1)}, {model.Int64Val(4), model.Int64Val(1)}, {model.NullVal(model.TypeInt64), model.Int64Val(2)}}, false},
		{"distinct", [][]uint32{{0}}, [][]model.Value{{model.Int64Val(3), model.Int64Val(1)}, {model.Int64Val(4), model.Int64Val(1)}}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reader, request := realScan(t)
			base := scanGraph(t, reader, request, false, false).Plan()
			logical, _ := base.Graph(1)
			scan, err := base.CloneNode(t.Context(), base.Root(), 4<<20)
			if err != nil {
				t.Fatal(err)
			}
			source := scan.Output[0]
			rel, _ := logical.Rel(logical.Root())
			b, err := ir.Extend(t.Context(), logical, irLimits())
			if err != nil {
				t.Fatal(err)
			}
			key, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: rel.Scope, Layout: source.Layout, Slot: source.ID}})
			if err != nil {
				t.Fatal(err)
			}
			group, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: rel.Scope, Layout: source.Layout, Nullable: true})
			if err != nil {
				t.Fatal(err)
			}
			count, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: rel.Scope, Layout: source.Layout})
			if err != nil {
				t.Fatal(err)
			}
			effects := ir.Effects{MayError: true}
			function := model.FunctionID(2803)
			var arguments []ir.ScalarID
			var parameters []ir.LayoutID
			if scenario.distinct {
				function = 2147
				arguments = []ir.ScalarID{key}
				parameters = []ir.LayoutID{source.Layout}
			}
			signature, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: source.Layout, Effects: effects}, Parameters: parameters})
			if err != nil {
				t.Fatal(err)
			}
			root, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: rel.Scope, Effects: effects}, Inputs: []ir.RelID{logical.Root()}, Outputs: []model.SlotID{group, count}, Expressions: []ir.ScalarID{key}, Bindings: []ir.Binding{{Slot: group, Expr: key}}, GroupingSets: scenario.sets, Aggregates: []ir.AggregateSpec{{AggregateCall: ir.AggregateCall{Function: function, Signature: signature, Result: count, Effects: effects, Distinct: scenario.distinct}, Args: arguments}}})
			if err != nil {
				t.Fatal(err)
			}
			logical, err = b.Build(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			properties := scan.Delivered
			aggregate := physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.HashAggregate, Children: []physical.NodeID{1}, Output: []physical.Slot{{Graph: 1, ID: group, Layout: source.Layout, Nullable: true}, {Graph: 1, ID: count, Layout: source.Layout}}, Required: []physical.Properties{properties}, Delivered: properties, Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 1 << 20, Spillable: true}, Aggregate: physical.AggregateSpec{Phase: physical.CompleteAggregate, GroupKeys: []physical.Key{{Slot: source, Type: request.Layout[0].Type}}}}
			plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{logical}, Nodes: []physical.NodeSpec{scan, aggregate}, Root: 2}, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			env, dir := groupingSetsEnvironment(t, true)
			env.Reader = reader
			env.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertGroupingSetsBag(t, rows(t, portal), scenario.want)
			if !reader.cursorClosed || !reader.readerClosed {
				t.Fatal("grouping-set EOF retained native cursor or snapshot")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}
