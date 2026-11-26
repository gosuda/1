package ir

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func groupingFixture(t *testing.T) (fixture, RelSpec) {
	t.Helper()
	f := newFixture(t)
	int4, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	left, right := f.slot(int4, false, 10, 1), f.slot(int4, false, 10, 2)
	input := f.rel(RelSpec{Rel: Rel{Kind: Scan, Scope: f.scope, Object: 10, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{left, right}})
	leftKey := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: int4, Slot: left}})
	rightKey := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: int4, Slot: right}})
	leftGroup, rightGroup := f.slot(int4, false, 0, 0), f.slot(int4, false, 0, 0)
	result := f.slot(int4, false, 0, 0)
	return f, RelSpec{
		Rel: Rel{Kind: Aggregate, Scope: f.scope}, Inputs: []RelID{input},
		Outputs:      []model.SlotID{leftGroup, rightGroup, result},
		Expressions:  []ScalarID{leftKey, rightKey},
		Bindings:     []Binding{{Slot: leftGroup, Expr: leftKey}, {Slot: rightGroup, Expr: rightKey}},
		GroupingSets: [][]uint32{{0, 1}},
		Groupings:    []GroupingSpec{{GroupingCall: GroupingCall{Result: result}, Args: []uint32{1, 0, 1}}},
	}
}

func TestGroupingOwnsArgumentsAcrossCloneAndExtend(t *testing.T) {
	f, spec := groupingFixture(t)
	result := spec.Groupings[0].Result
	root := f.rel(spec)
	spec.Groupings[0].Args[0] = 0
	spec.Groupings[0].Result = spec.Outputs[0]
	source := f.freeze(root)
	clone, err := source.CloneRelSpec(t.Context(), root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	clone.Groupings[0].Args[0] = 0
	builder, err := Extend(t.Context(), source, limitsForTest())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := builder.AddRel(t.Context(), clone)
	if err != nil {
		t.Fatal(err)
	}
	clone.Groupings[0].Args[1] = 1
	extended, err := builder.Build(t.Context(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		graph *Graph
		root  RelID
		args  []uint32
	}{
		{"source", source, root, []uint32{1, 0, 1}},
		{"extended original", extended, root, []uint32{1, 0, 1}},
		{"replacement", extended, replacement, []uint32{0, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, _ := tc.graph.Rel(tc.root)
			call, ok := tc.graph.Grouping(tc.root, 0)
			if !ok || node.GroupingCount() != 1 || call.Result != result || call.ArgumentCount() != uint32(len(tc.args)) {
				t.Fatalf("GROUPING result=%+v, present=%v, count=%d", call, ok, node.GroupingCount())
			}
			for i, want := range tc.args {
				if got, ok := tc.graph.GroupingArgument(tc.root, 0, uint32(i)); !ok || got != want {
					t.Fatalf("argument %d=%d, present=%v, want %d", i, got, ok, want)
				}
			}
		})
	}
	for _, indices := range [][3]uint32{{0, 0, 0}, {uint32(root), 1, 0}, {uint32(root), 0, 3}, {uint32(root), 0, math.MaxUint32}} {
		if _, ok := source.GroupingArgument(RelID(indices[0]), indices[1], indices[2]); ok {
			t.Fatalf("out-of-range GROUPING argument accepted: %v", indices)
		}
	}
}

func TestGroupingAcceptsThirtyOneRepeatedArguments(t *testing.T) {
	f, spec := groupingFixture(t)
	spec.Groupings[0].Args = make([]uint32, 31)
	root := f.rel(spec)
	graph := f.freeze(root)
	call, _ := graph.Grouping(root, 0)
	if call.ArgumentCount() != 31 {
		t.Fatalf("argument count=%d, want 31", call.ArgumentCount())
	}
	if index, ok := graph.GroupingArgument(root, 0, 30); !ok || index != 0 {
		t.Fatalf("last argument=%d, present=%v", index, ok)
	}
}

func TestGroupingRejectsInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(fixture, *RelSpec)
	}{
		{"no arguments", func(_ fixture, s *RelSpec) { s.Groupings[0].Args = nil }},
		{"too many arguments", func(_ fixture, s *RelSpec) { s.Groupings[0].Args = make([]uint32, 32) }},
		{"key outside grouping expressions", func(_ fixture, s *RelSpec) { s.Groupings[0].Args = []uint32{2} }},
		{"missing output", func(_ fixture, s *RelSpec) { s.Outputs = s.Outputs[:2] }},
		{"zero result", func(_ fixture, s *RelSpec) { s.Groupings[0].Result = 0 }},
		{"nullable result", func(f fixture, s *RelSpec) {
			layout := f.b.graph.slots[s.Groupings[0].Result-1].Layout
			s.Groupings[0].Result = f.slot(layout, true, 0, 0)
			s.Outputs[2] = s.Groupings[0].Result
		}},
		{"wrong integer type", func(f fixture, s *RelSpec) {
			s.Groupings[0].Result = f.slot(f.integer, false, 0, 0)
			s.Outputs[2] = s.Groupings[0].Result
		}},
		{"wrong scope", func(f fixture, s *RelSpec) {
			child, err := f.b.AddScope(f.t.Context(), f.scope, nil)
			if err != nil {
				f.t.Fatal(err)
			}
			layout := f.b.graph.slots[s.Groupings[0].Result-1].Layout
			result, err := f.b.AddSlot(f.t.Context(), SlotInfo{Scope: child, Layout: layout})
			if err != nil {
				f.t.Fatal(err)
			}
			s.Groupings[0].Result, s.Outputs[2] = result, result
		}},
		{"group binding collision", func(_ fixture, s *RelSpec) {
			s.Groupings[0].Result = s.Outputs[0]
			s.Outputs = s.Outputs[:2]
		}},
		{"aggregate result collision", func(f fixture, s *RelSpec) {
			result := s.Groupings[0].Result
			layout := f.b.graph.slots[result-1].Layout
			signature, err := f.b.AddSignature(f.t.Context(), SignatureSpec{Signature: Signature{Function: 100, Result: layout}})
			if err != nil {
				f.t.Fatal(err)
			}
			s.Aggregates = []AggregateSpec{{AggregateCall: AggregateCall{Signature: signature, Function: 100, Result: result}}}
		}},
		{"duplicate result", func(_ fixture, s *RelSpec) { s.Groupings = append(s.Groupings, s.Groupings[0]) }},
		{"foreign relation", func(_ fixture, s *RelSpec) {
			s.Kind, s.Rows = Values, 1
			s.Inputs, s.Expressions, s.Outputs, s.Bindings, s.GroupingSets = nil, nil, nil, nil, nil
		}},
		{"call arena payload", func(_ fixture, s *RelSpec) { s.Groupings[0].args = extent{count: 1} }},
		{"relation arena payload", func(_ fixture, s *RelSpec) { s.Rel.groupings = extent{count: 1} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, spec := groupingFixture(t)
			tc.change(f, &spec)
			_, err := f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestGroupingArgumentsShareIndexArenaLimit(t *testing.T) {
	f, spec := groupingFixture(t)
	spec.Groupings[0].Args = []uint32{1, 0, 1, 0, 1}
	f.b.limits.MaxOperands = 7
	root := f.rel(spec)
	graph := f.freeze(root)
	limited := limitsForTest()
	limited.MaxOperands = 6
	if _, err := Extend(t.Context(), graph, limited); !errors.Is(err, ErrLimit) {
		t.Fatalf("extension under argument limit=%v, want ErrLimit", err)
	}
	f, spec = groupingFixture(t)
	spec.Groupings[0].Args = []uint32{1, 0, 1, 0, 1}
	f.b.limits.MaxOperands = 6
	if _, err := f.b.AddRel(t.Context(), spec); !errors.Is(err, ErrLimit) {
		t.Fatalf("construction under argument limit=%v, want ErrLimit", err)
	}
}
