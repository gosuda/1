package physical

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Keep the query small enough for the existing fixture's IR limits. Replaying
// Direct, Args and Order roots mirrors the optimizer's physical root inventory.
func directLookupAggregateSpec(t *testing.T, count int, repeated bool) Spec {
	t.Helper()
	return directLookupAggregateStrideSpec(t, count, repeated, 1)
}

func directLookupAggregateStrideSpec(t *testing.T, count int, repeated bool, stride int) Spec {
	t.Helper()
	f := newFixture(t)
	input := f.scan(10)
	row := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: input.Output[0].ID}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
		Signature:  ir.Signature{Function: 3986, Result: f.layout},
		Parameters: []ir.LayoutID{f.layout, f.layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	var first ir.ScalarID
	var calls []ir.AggregateSpec
	var outputs []model.SlotID
	var physicalOutputs []Slot
	var expressions []ScalarRef
	for i := 0; i < count; i++ {
		direct := first
		if i == 0 || !repeated {
			direct = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(int64(i))})
			if i == 0 {
				first = direct
			}
			for padding := 1; padding < stride; padding++ {
				f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(-int64(padding))})
			}
		}
		result, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, ir.AggregateSpec{
			AggregateCall: ir.AggregateCall{Signature: signature, Function: 3986, Result: result},
			Direct:        []ir.ScalarID{direct},
			Args:          []ir.ScalarID{row},
		})
		outputs = append(outputs, result)
		physicalOutputs = append(physicalOutputs, Slot{Graph: 1, ID: result, Layout: f.layout})
		expressions = append(expressions, ScalarRef{Graph: 1, Scalar: direct}, ScalarRef{Graph: 1, Scalar: row}, ScalarRef{Graph: 1, Scalar: row})
	}
	logical := f.rel(ir.RelSpec{
		Rel:          ir.Rel{Kind: ir.Aggregate, Scope: f.scope},
		Inputs:       []ir.RelID{input.Logical.Rel},
		Outputs:      outputs,
		GroupingSets: [][]uint32{{}},
		Aggregates:   calls,
	})
	aggregate := NodeSpec{
		Logical: RelRef{Graph: 1, Rel: logical}, Algorithm: HashAggregate,
		Children: []NodeID{1}, Required: []Properties{single()},
		Output: physicalOutputs, Expressions: expressions,
		Delivered: single(), Break: ExpectedBreak(HashAggregate), Memory: Memory{PeakBytes: 1024},
		Aggregate: AggregateSpec{Phase: CompleteAggregate},
	}
	return f.spec(input, aggregate)
}

func TestDirectLookupWorkIsBounded(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		name := "distinct"
		if repeated {
			name = "duplicate"
		}
		t.Run(name, func(t *testing.T) {
			spec := directLookupAggregateSpec(t, 16, repeated)
			plan, err := NewPlan(t.Context(), spec, testLimits())
			if err != nil || plan == nil {
				t.Fatalf("small aggregate with 16 Direct entries: plan=%v error=%v", plan, err)
			}
		})
	}
}

func TestDirectLookupPreservesConstructionGuards(t *testing.T) {
	spec := directLookupAggregateSpec(t, 1, false)
	if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		alter func(*Limits)
	}{
		{"operands", func(limits *Limits) { limits.MaxOperands = 1 }},
		{"bytes", func(limits *Limits) { limits.MaxBytes = 1 }},
		{"depth", func(limits *Limits) { limits.MaxDepth = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := testLimits()
			test.alter(&limits)
			if plan, err := NewPlan(t.Context(), spec, limits); plan != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("plan=%v error=%v, want resource limit", plan, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if plan, err := NewPlan(ctx, spec, testLimits()); plan != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled plan=%v error=%v", plan, err)
	}
	// A second registered graph does not change the aggregate's graph identity.
	// Its scalar number can match a Direct ID, but it cannot provide that
	// aggregate's logical result slots.
	other := directLookupAggregateSpec(t, 1, false)
	spec.Graphs = append(spec.Graphs, other.Graphs[0])
	root := &spec.Nodes[len(spec.Nodes)-1]
	root.Output[0].Graph = 2
	if plan, err := NewPlan(t.Context(), spec, testLimits()); plan != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign graph output=%v error=%v", plan, err)
	}
}

func TestDirectLookupKeepsGraphLocalScalarIDs(t *testing.T) {
	spec := directLookupAggregateSpec(t, 1, false)
	other := directLookupAggregateSpec(t, 1, false)
	spec.Graphs = append(spec.Graphs, other.Graphs[0])
	root := &spec.Nodes[len(spec.Nodes)-1]
	direct := root.Expressions[0].Scalar
	root.Expressions = make([]ScalarRef, 128)
	for i := range root.Expressions {
		// These constants share a number with the logical Direct root but
		// belong to another registered graph. No Direct lookup is needed.
		root.Expressions[i] = ScalarRef{Graph: 2, Scalar: direct}
	}
	limits := testLimits()
	limits.MaxOperands = 192
	if plan, err := NewPlan(t.Context(), spec, limits); err != nil || plan == nil {
		t.Fatalf("foreign constants: plan=%v error=%v", plan, err)
	}
}

func TestDirectLookupReleasesScratchBudget(t *testing.T) {
	spec := directLookupAggregateSpec(t, 1, false)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	root := spec.Nodes[len(spec.Nodes)-1]
	const retained = uint64(7)
	for _, test := range []struct {
		name     string
		bytes    uint64
		operands uint32
		want     error
	}{
		{"available", 64, 512, nil},
		{"exact capacity", retained + 8, 512, nil},
		{"one byte short", retained + 7, 512, ErrLimit},
		{"no scratch bytes", retained, 512, ErrLimit},
		{"work exhausted after allocation", 64, 4, ErrLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := testLimits()
			limits.MaxBytes = test.bytes
			limits.MaxOperands = test.operands
			budget := copier{ctx: t.Context(), limits: limits, used: retained}
			err := plan.validateScalarSubplans(t.Context(), root, &budget)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if budget.used != retained {
				t.Fatalf("retained bytes=%d, want %d", budget.used, retained)
			}
		})
	}
}

func TestDirectLookupBoundsDistinctCollisions(t *testing.T) {
	spec := directLookupAggregateStrideSpec(t, 4, false, 8)
	root := spec.Nodes[len(spec.Nodes)-1]
	for i := 0; i < 4; i++ {
		if id := root.Expressions[3*i].Scalar; id != ir.ScalarID(2+8*i) {
			t.Fatalf("Direct root %d: scalar=%d, want %d", i, id, 2+8*i)
		}
	}
	// Four distinct IDs with identical low bits collide in the eight-bucket
	// table. The replay also looks up the row root, which is absent.
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	const retained = uint64(7)
	for _, test := range []struct {
		name     string
		operands uint32
		want     error
	}{
		{"hit and miss", 512, nil},
		{"collision work exhausted", 22, ErrLimit},
		{"collision probes charged", 56, ErrLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := testLimits()
			limits.MaxBytes = 64
			limits.MaxOperands = test.operands
			budget := copier{ctx: t.Context(), limits: limits, used: retained}
			err := plan.validateScalarSubplans(t.Context(), root, &budget)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if budget.used != retained {
				t.Fatalf("retained bytes=%d, want %d", budget.used, retained)
			}
		})
	}
}
