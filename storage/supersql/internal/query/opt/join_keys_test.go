package opt

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func joinHasAlgorithm(t *testing.T, plan *physical.Plan, maxBytes uint64, alg physical.Algorithm) bool {
	t.Helper()
	for _, a := range planAlgorithms(t, plan, maxBytes) {
		if a == alg {
			return true
		}
	}
	return false
}

// joinKeys admits only strict immutable non-erroring comparisons over a
// hash- or merge-joinable operator. A mixed equality/inequality predicate
// must still key the equality instead of disabling keyed joins entirely.
func TestMixedEqualityInequalitySelectsHashJoin(t *testing.T) {
	f := newFixture(t)
	left, l := f.source(10)
	right, r := f.source(20)
	rSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: r}})
	eq := f.comparison(l, rSlot, 410)
	rSlot2 := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: r}})
	lt := f.comparison(l, rSlot2, 412)
	and := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, BoolOp: ir.And}, Operands: []ir.ScalarID{eq, lt}})
	outR, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	join := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.LeftJoin, Predicate: and},
		Inputs:  []ir.RelID{left, right},
		Outputs: []model.SlotID{l, outR},
	})
	options := testOptions()
	options.Access = accessStub{}
	result, err := Optimize(t.Context(), f.build(join), options)
	if err != nil {
		t.Fatalf("optimize with mixed eq/lt predicate: %v", err)
	}
	if !joinHasAlgorithm(t, result.Plan, options.Plan.MaxBytes, physical.HashJoin) {
		t.Fatalf("expected HashJoin for mixed eq/lt predicate, got %v", planAlgorithms(t, result.Plan, options.Plan.MaxBytes))
	}
	lower(t, result.Plan)
}

// An inequality-only predicate must never produce a lookup plan: the key
// operator has no proven equality semantics, so plan validation rejects it.
func TestInequalityOnlyWithIndexPlansNoLookupJoin(t *testing.T) {
	f := newFixture(t)
	left, l := f.source(10)
	right, r := f.source(20)
	rSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: r}})
	lt := f.comparison(l, rSlot, 412)
	outR, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	join := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.LeftJoin, Predicate: lt},
		Inputs:  []ir.RelID{left, right},
		Outputs: []model.SlotID{l, outR},
	})
	options := testOptions()
	options.Access = accessStub{paths: []physical.Algorithm{physical.SecondaryScan}}
	result, err := Optimize(t.Context(), f.build(join), options)
	if err != nil {
		t.Fatalf("optimize with lt-only predicate and index: %v", err)
	}
	if joinHasAlgorithm(t, result.Plan, options.Plan.MaxBytes, physical.LookupJoin) {
		t.Fatalf("lt-only predicate must not plan a lookup join, got %v", planAlgorithms(t, result.Plan, options.Plan.MaxBytes))
	}
	lower(t, result.Plan)
}

// Keyed plans evaluate the residual only for key-matching pairs, so a
// predicate with an erroring conjunct must stay on the nested loop: the
// nested loop raises for pairs the keys would discard before the residual.
func TestErroringConjunctKeepsNestedLoopJoin(t *testing.T) {
	f := newFixture(t)
	left, l := f.source(10)
	right, r := f.source(20)
	rSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: r}})
	eq := f.comparison(l, rSlot, 410)
	zero := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(0)})
	one := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	divSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 687, Result: f.layout, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	lSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: l}})
	div := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.layout, Signature: divSig, Operator: 687, Strict: true, Effects: ir.Effects{MayError: true}}, Operands: []ir.ScalarID{lSlot, zero}})
	gtSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 413, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	gt := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: gtSig, Operator: 413, Strict: true, Effects: ir.Effects{MayError: true}}, Operands: []ir.ScalarID{div, one}})
	and := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, BoolOp: ir.And, Effects: ir.Effects{MayError: true}}, Operands: []ir.ScalarID{eq, gt}})
	outR, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	join := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.LeftJoin, Predicate: and, Effects: ir.Effects{MayError: true}},
		Inputs:  []ir.RelID{left, right},
		Outputs: []model.SlotID{l, outR},
	})
	options := testOptions()
	options.Access = accessStub{}
	result, err := Optimize(t.Context(), f.build(join), options)
	if err != nil {
		t.Fatalf("optimize with erroring conjunct: %v", err)
	}
	for _, alg := range planAlgorithms(t, result.Plan, options.Plan.MaxBytes) {
		if alg == physical.HashJoin || alg == physical.MergeJoin || alg == physical.LookupJoin {
			t.Fatalf("erroring predicate must not use a keyed join, got %v", planAlgorithms(t, result.Plan, options.Plan.MaxBytes))
		}
	}
	lower(t, result.Plan)
}
