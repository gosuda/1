package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRecursiveWorkingTableCannotEscapeThroughAlias(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	recursive, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{f.constant(1)}})
	reference := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: recursive, RecursiveReference: true}, Outputs: []model.SlotID{out}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: recursive, All: true}, Inputs: []RelID{anchor, reference}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), recursive, union); err != nil {
		t.Fatal(err)
	}
	alias, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), alias, reference); err != nil {
		t.Fatal(err)
	}
	root := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: alias}, Outputs: []model.SlotID{out}})
	graph, err := f.b.Build(t.Context(), root)
	if graph != nil {
		t.Fatal("working-table alias escaped its recursive invocation")
	}
	requireInvalid(t, err)
}

func TestRecursiveMemberRejectsDistinctWorkingTableAliases(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	recursive, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{f.constant(1)}})
	var inputs []RelID
	for range 2 {
		reference := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: recursive, RecursiveReference: true}, Outputs: []model.SlotID{out}})
		alias, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.b.BindCTE(t.Context(), alias, reference); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: alias}, Outputs: []model.SlotID{out}}))
	}
	member := f.rel(RelSpec{Rel: Rel{Kind: SetOp, Scope: f.scope, SetKind: Union, All: true}, Inputs: inputs, Outputs: []model.SlotID{out}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: recursive, All: true}, Inputs: []RelID{anchor, member}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), recursive, union); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), union)
	if graph != nil {
		t.Fatal("distinct recursive sources were mistaken for repeated readers of one CTE")
	}
	requireInvalid(t, err)
}

func TestRecursiveReferenceInsideScalarLocalCTEIsRejected(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, true, 0, 0)
	recursive, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true, Effects: Effects{MayError: true}})
	if err != nil {
		t.Fatal(err)
	}
	anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{f.constant(1)}})
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: child, Layout: f.integer, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	reference := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: child, CTE: recursive, RecursiveReference: true, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{inner}})
	alias, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: child, Outputs: []model.SlotID{inner}, Effects: Effects{MayError: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), alias, reference); err != nil {
		t.Fatal(err)
	}
	consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: child, CTE: alias, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{inner}})
	value := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.integer, Nullable: true, Relation: consumer, SubqueryKind: ScalarSubquery, Effects: Effects{MayError: true}}})
	member := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{value}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: recursive, All: true, Effects: Effects{MayError: true}}, Inputs: []RelID{anchor, member}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), recursive, union); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), union)
	if graph != nil {
		t.Fatal("scalar-local CTE hid a forbidden recursive reference")
	}
	requireInvalid(t, err)
}
