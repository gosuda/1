package opt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestTinyMemoRetainsReferencedCTEProducer(t *testing.T) {
	f := newFixture(t)
	body, output := f.values(2)
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{output}, Materialization: ir.MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	consumerSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		t.Fatal(err)
	}
	consumer := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerSlot}})
	options := testOptions()
	options.SearchTime = time.Nanosecond
	result, err := Optimize(t.Context(), f.build(consumer), options)
	if err != nil {
		t.Fatal(err)
	}
	root, err := result.Plan.CloneNode(t.Context(), result.Plan.Root(), options.Plan.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if root.Algorithm != physical.CTEScan || root.Worktable.Producer == 0 {
		t.Fatalf("missing materialized dependency: %+v", root.Worktable)
	}
	producer, _ := result.Plan.Node(root.Worktable.Producer)
	if producer.Metadata().Algorithm != physical.CTEMaterialize {
		t.Fatal("consumer dependency is not a producer")
	}
	lower(t, result.Plan)
}

func TestUnusedErroringSelectCTERemainsUnevaluated(t *testing.T) {
	f := newFixture(t)
	input, slot := f.values(1)
	count := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	body := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: f.scope, Count: count, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{slot}})
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{slot}, Effects: ir.Effects{MayError: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	root, _ := f.values(1)
	result, err := Optimize(t.Context(), f.build(root), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.NodeCount() != 1 || rootMetadata(t, result.Plan).Algorithm != physical.Values {
		t.Fatal("unused SELECT CTE became an execution dependency")
	}
	lower(t, result.Plan)
}

func TestUnusedWritableCTERetainsMandatoryDependency(t *testing.T) {
	f := newFixture(t)
	input, slot := f.values(1)
	expr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: slot}})
	target, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Object: 10, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	body := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Modify, Scope: f.scope, Object: 10, DescriptorVersion: 1, Generation: 1, EncodingVersion: 1, ModifyKind: ir.Insert, ModifyTarget: ir.PhysicalModifyTarget, Effects: effects}, Inputs: []ir.RelID{input}, Assignments: []ir.Binding{{Slot: target, Expr: expr}}, Outputs: []model.SlotID{slot}, Bindings: []ir.Binding{{Slot: slot, Expr: expr}}})
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{slot}, Effects: effects})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	root, _ := f.values(1)
	result, err := Optimize(t.Context(), f.build(root), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	node, _ := result.Plan.Node(result.Plan.Root())
	if node.StatementDependencyCount() != 1 {
		t.Fatal("unused writer is not a mandatory dependency")
	}
	dependency, _ := node.StatementDependency(0)
	producer, _ := result.Plan.Node(dependency)
	modification, _ := producer.Child(0)
	write, _ := result.Plan.Node(modification)
	if write.Metadata().Logical.Rel != body || node.OutputCount() != 1 {
		t.Fatal("mandatory writer changed mutation identity or root output")
	}
	lower(t, result.Plan)
}
func TestSetOperationSeedRetainsMultiplicitySemantics(t *testing.T) {
	for _, kind := range []ir.SetKind{ir.Intersect, ir.Except, ir.Union} {
		t.Run(map[ir.SetKind]string{ir.Intersect: "intersect", ir.Except: "except", ir.Union: "union"}[kind], func(t *testing.T) {
			f := newFixture(t)
			left, l := f.values(2)
			right, _ := f.values(1)
			root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: f.scope, SetKind: kind}, Inputs: []ir.RelID{left, right}, Outputs: []model.SlotID{l}})
			options := testOptions()
			options.SearchTime = time.Nanosecond
			result, err := Optimize(t.Context(), f.build(root), options)
			if err != nil {
				t.Fatal(err)
			}
			metadata := rootMetadata(t, result.Plan)
			if metadata.Algorithm != physical.HashSetOp || metadata.Break != physical.MaterializeBoth {
				t.Fatalf("set seed=%+v", metadata)
			}
			graph, _ := result.Plan.Graph(metadata.Logical.Graph)
			relation, _ := graph.Rel(metadata.Logical.Rel)
			if relation.SetKind != kind || relation.All {
				t.Fatal("set multiplicity semantics changed")
			}
			lower(t, result.Plan)
		})
	}
}
func TestSoftMemoMemoryStopsBeforeJoinEnumeration(t *testing.T) {
	f := newFixture(t)
	left, l := f.values(3)
	right, r := f.values(2)
	graph := f.build(f.cross(left, right, l, r))
	options := testOptions()
	options.MemoBytes = 1
	result, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.BudgetExhausted || result.MemoBytes > 1 || rootMetadata(t, result.Plan).Logical.Rel != graph.Root() {
		t.Fatalf("memo limit did not retain original seed: %+v", result)
	}
	lower(t, result.Plan)
}
func TestStateMemorySpillsWithinWorkingLimit(t *testing.T) {
	f := newFixture(t)
	values, slot := f.values(100)
	expr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: slot}})
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Sort, Scope: f.scope}, Inputs: []ir.RelID{values}, Outputs: []model.SlotID{slot}, Order: []ir.OrderKey{{Expr: expr, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}})
	options := testOptions()
	options.WorkingMemory = 32
	result, err := Optimize(t.Context(), f.build(root), options)
	if err != nil {
		t.Fatal(err)
	}
	memory := rootMetadata(t, result.Plan).Memory
	if memory.PeakBytes > options.WorkingMemory || !memory.Spillable || memory.SpillBytes == 0 {
		t.Fatalf("state memory=%+v", memory)
	}
	if result.Plan.OwnedBytes() > options.Plan.MaxBytes {
		t.Fatal("physical plan exceeded output byte limit")
	}
	lower(t, result.Plan)
}
func TestPreCanceledRequestCannotReturnSeed(t *testing.T) {
	f := newFixture(t)
	root, _ := f.values(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := Optimize(ctx, f.build(root), testOptions())
	if !errors.Is(err, context.Canceled) || result.Plan != nil {
		t.Fatalf("cancellation unexpectedly planned: %+v %v", result, err)
	}
}
func TestParameterVarianceRetainsCustomMode(t *testing.T) {
	var policy ParameterPolicy
	if err := policy.ObserveGeneric(1); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []float64{0, 0, 0, 0, 10000} {
		if err := policy.ObserveCustom(cost, time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	if policy.Mode() != CustomPlan {
		t.Fatal("high custom variance was treated as generic-plan evidence")
	}
	for range 512 {
		if err := policy.ObserveCustom(100, time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	if policy.Mode() != GenericPlan {
		t.Fatal("bounded observation window failed to forget obsolete variance")
	}
}
