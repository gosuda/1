package physical

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// The graph is valid before physical planning. Its direct subquery can read a
// grouped output that the aggregate's per-row input does not deliver.
func aggregateDirectSubquerySpec(t *testing.T, captured bool) (Spec, ir.ScalarID) {
	t.Helper()
	f := newFixture(t)
	input := f.scan(10)
	addSlot := func(scope ir.ScopeID) model.SlotID {
		slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		return slot
	}
	grouped, result := addSlot(f.scope), addSlot(f.scope)
	group := Slot{Graph: 1, ID: grouped, Layout: f.layout}
	var captures []model.SlotID
	var parameters []Slot
	if captured {
		captures = []model.SlotID{grouped}
		parameters = []Slot{group}
	}
	child, err := f.b.AddScope(t.Context(), f.scope, captures)
	if err != nil {
		t.Fatal(err)
	}
	var value ir.ScalarID
	if captured {
		value = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: child, Layout: f.layout, Slot: grouped}})
	} else {
		value = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: child, Layout: f.layout}, Value: model.Int64Val(1)})
	}
	childOutput := addSlot(child)
	body := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: child, Rows: 1}, Outputs: []model.SlotID{childOutput}, Expressions: []ir.ScalarID{value}})
	effects := ir.Effects{MayError: true}
	direct := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Subquery, Scope: f.scope, Layout: f.layout, Nullable: true, Effects: effects, SubqueryKind: ir.ScalarSubquery, Relation: body}})
	row := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: input.Output[0].ID}})
	wrapper := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: f.scope, Layout: f.layout, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{direct}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
		Signature:  ir.Signature{Function: 3986, Result: f.layout, Effects: effects},
		Parameters: []ir.LayoutID{f.layout, f.layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	logical := f.rel(ir.RelSpec{
		Rel:          ir.Rel{Kind: ir.Aggregate, Scope: f.scope, Effects: effects},
		Inputs:       []ir.RelID{input.Logical.Rel},
		Outputs:      []model.SlotID{grouped, result},
		Expressions:  []ir.ScalarID{row},
		Bindings:     []ir.Binding{{Slot: grouped, Expr: row}},
		GroupingSets: [][]uint32{{0}},
		Aggregates: []ir.AggregateSpec{{
			AggregateCall: ir.AggregateCall{Signature: signature, Function: 3986, Result: result, Effects: effects},
			Direct:        []ir.ScalarID{direct},
			Args:          []ir.ScalarID{row},
		}},
	})
	bodyProperties := single()
	bodyProperties.Parameters = parameters
	subplan := NodeSpec{
		Logical: RelRef{Graph: 1, Rel: body}, Algorithm: Values,
		Output:      []Slot{{Graph: 1, ID: childOutput, Layout: f.layout}},
		Expressions: []ScalarRef{{Graph: 1, Scalar: value}},
		Delivered:   bodyProperties, Break: Streaming,
	}
	aggregate := NodeSpec{
		Logical: RelRef{Graph: 1, Rel: logical}, Algorithm: HashAggregate,
		Children: []NodeID{1}, Required: []Properties{single()},
		Output:         []Slot{group, {Graph: 1, ID: result, Layout: f.layout}},
		Expressions:    []ScalarRef{{Graph: 1, Scalar: row}, {Graph: 1, Scalar: direct}},
		ScalarSubplans: []ScalarSubplan{{Scalar: ScalarRef{Graph: 1, Scalar: direct}, Root: 2}},
		Delivered:      single(), Break: ExpectedBreak(HashAggregate), Memory: Memory{PeakBytes: 1024},
		Aggregate: AggregateSpec{Phase: CompleteAggregate, GroupKeys: []Key{{Slot: input.Output[0], Type: f.typ}}},
	}
	return f.spec(input, subplan, aggregate), wrapper
}

func TestAggregateDirectSubqueryValidationUsesEachPhase(t *testing.T) {
	for _, captured := range []bool{false, true} {
		name := "uncorrelated"
		if captured {
			name = "grouped-capture"
		}
		t.Run(name, func(t *testing.T) {
			spec, wrapper := aggregateDirectSubquerySpec(t, captured)
			if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
				t.Fatalf("direct argument plan: %v", err)
			}
			// The same subquery identity is now also reached through an
			// ordinary physical expression. Its prior Direct validation
			// cannot grant grouped-output access to this second phase.
			root := &spec.Nodes[len(spec.Nodes)-1]
			root.Expressions = append(root.Expressions, ScalarRef{Graph: 1, Scalar: wrapper})
			plan, err := NewPlan(t.Context(), spec, testLimits())
			if captured {
				if plan != nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unavailable scalar capture") {
					t.Fatalf("row-phase grouped capture: plan=%v error=%v", plan, err)
				}
			} else if err != nil {
				t.Fatalf("unaffected shared subquery: %v", err)
			}
		})
	}
}

func TestAggregateDirectCaptureInputs(t *testing.T) {
	spec, _ := aggregateDirectSubquerySpec(t, true)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	root := spec.Nodes[len(spec.Nodes)-1]
	input, group, result := spec.Nodes[0].Output[0], root.Output[0], root.Output[1]
	wrongMetadata := group
	wrongMetadata.Nullable = true
	for _, test := range []struct {
		name   string
		slot   Slot
		direct bool
		want   bool
	}{
		{"direct-group", group, true, true},
		{"row-group", group, false, false},
		{"direct-input", input, true, false},
		{"row-input", input, false, true},
		{"direct-result", result, true, false},
		{"row-result", result, false, false},
		{"direct-nullability-mismatch", wrongMetadata, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			remaining := uint64(testLimits().MaxOperands)
			got, err := plan.scalarCaptureAvailable(root, test.slot, test.direct, &remaining)
			if err != nil || got != test.want {
				t.Fatalf("capture available=%v error=%v, want %v", got, err, test.want)
			}
		})
	}
	remaining := uint64(0)
	if available, err := plan.scalarCaptureAvailable(root, group, true, &remaining); available || !errors.Is(err, ErrLimit) {
		t.Fatalf("exhausted capture budget: available=%v error=%v", available, err)
	}
}
