package opt

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestRowsFromLongestInputDefinesCardinality(t *testing.T) {
	f := newFixture(t)
	spec := ir.RelSpec{Rel: ir.Rel{Kind: ir.RowsFrom, Scope: f.scope, RowsFromOrdinality: true, Effects: ir.Effects{MayError: true}}}
	for i, rows := range []uint64{2, 0, 7} {
		input, slot := f.values(rows)
		if i == 0 {
			key := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: slot}})
			input = f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Sort, Scope: f.scope}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{slot}, Order: []ir.OrderKey{{Expr: key, Operator: 412, Direction: ir.Ascending, Nulls: ir.NullsLast}}})
		}
		output, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		spec.Inputs = append(spec.Inputs, input)
		spec.Outputs = append(spec.Outputs, output)
	}
	ordinalLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	ordinal, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: ordinalLayout})
	if err != nil {
		t.Fatal(err)
	}
	spec.Outputs = append(spec.Outputs, ordinal)
	graph := f.build(f.rel(spec))
	for _, rewind := range []bool{false, true} {
		options := testOptions()
		if rewind {
			options.Required.Rewindability = physical.Rewindable
		}
		result, err := Optimize(t.Context(), graph, options)
		if err != nil {
			t.Fatal(err)
		}
		id := result.Plan.Root()
		root, _ := result.Plan.Node(id)
		if rewind {
			if root.Metadata().Algorithm != physical.Materialize {
				t.Fatalf("rewindable zipper requires materialization, got %v", root.Metadata().Algorithm)
			}
			id, _ = root.Child(0)
			root, _ = result.Plan.Node(id)
		}
		if root.Metadata().Algorithm != physical.RowsFrom || root.ChildCount() != 3 {
			t.Fatalf("zipper shape: %+v, children=%d", root.Metadata(), root.ChildCount())
		}
		estimate := root.Metadata().Estimate
		if estimate.Rows != 7 || estimate.LowerRows != 7 || estimate.UpperRows != 7 {
			t.Fatalf("longest input cardinality: %+v", estimate)
		}
		if root.Delivered().OrderingCount() != 0 || root.Delivered().Rewindability() != physical.OnePass || root.Metadata().Break != physical.Streaming {
			t.Fatal("zipper inherited ordering or rewindability")
		}
		fragments := lower(t, result.Plan)
		owner, _ := fragments.Owner(id)
		for i := range root.ChildCount() {
			child, _ := root.Child(i)
			childOwner, _ := fragments.Owner(child)
			if childOwner != owner {
				t.Fatalf("input %d is not owned by the zipper streaming pipeline", i)
			}
		}
		pipeline, _ := fragments.Pipeline(owner)
		if pipeline.ParallelEligible() {
			t.Fatal("zipper effects cannot enter parallel scan pushdown")
		}
	}
}

func TestRowsFromZeroWidthKeepsLongestCardinality(t *testing.T) {
	f := newFixture(t)
	var inputs []ir.RelID
	for _, rows := range []uint64{0, 3, 9} {
		inputs = append(inputs, f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: rows}}))
	}
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.RowsFrom, Scope: f.scope, Effects: ir.Effects{MayError: true}}, Inputs: inputs})
	result, err := Optimize(t.Context(), f.build(root), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	node, _ := result.Plan.Node(result.Plan.Root())
	if node.OutputCount() != 0 || node.Metadata().Estimate.Rows != 9 || node.Metadata().Estimate.LowerRows != 9 || node.Metadata().Estimate.UpperRows != 9 {
		t.Fatalf("zero-width zipper lost row multiplicity: %+v", node.Metadata())
	}
}
