package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCTEDirectChildBodyBuildsThroughSiblingConsumer(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, true, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	bodyScope, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
	bodyOut := bodyFixture.slot(f.integer, false, 10, 1)
	body := bodyFixture.scan(10, bodyOut)
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatalf("bind independent child body: %v", err)
	}
	consumerScope, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumerFixture := fixture{t: t, b: f.b, scope: consumerScope, integer: f.integer, boolean: f.boolean}
	consumerOut := consumerFixture.slot(f.integer, true, 0, 0)
	consumer := consumerFixture.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: consumerScope, CTE: cte}, Outputs: []model.SlotID{consumerOut}})
	exists := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.boolean, SubqueryKind: ExistsSubquery, Relation: consumer}})
	result := f.slot(f.boolean, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{result}, Expressions: []ScalarID{exists}})
	f.freeze(root)
}

func TestCTEDirectChildBodyMayCaptureHigherAncestor(t *testing.T) {
	f := newFixture(t)
	outer := f.slot(f.integer, false, 10, 1)
	input := f.scan(10, outer)
	declarationScope, err := f.b.AddScope(t.Context(), f.scope, []model.SlotID{outer})
	if err != nil {
		t.Fatal(err)
	}
	declaration := fixture{t: t, b: f.b, scope: declarationScope, integer: f.integer, boolean: f.boolean}
	out := declaration.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: declarationScope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	bodyScope, err := f.b.AddScope(t.Context(), declarationScope, []model.SlotID{outer})
	if err != nil {
		t.Fatal(err)
	}
	bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
	ref := bodyFixture.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: bodyScope, Layout: f.integer, Slot: outer}})
	bodyOut := bodyFixture.slot(f.integer, false, 0, 0)
	body := bodyFixture.rel(RelSpec{Rel: Rel{Kind: Values, Scope: bodyScope, Rows: 1}, Outputs: []model.SlotID{bodyOut}, Expressions: []ScalarID{ref}})
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatalf("bind body capturing higher ancestor: %v", err)
	}
	consumerOut := declaration.slot(f.integer, false, 0, 0)
	consumer := declaration.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: declarationScope, CTE: cte}, Outputs: []model.SlotID{consumerOut}})
	exists := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.boolean, SubqueryKind: ExistsSubquery, Relation: consumer}})
	result := f.slot(f.boolean, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{input}, Outputs: []model.SlotID{result}, Bindings: []Binding{{Slot: result, Expr: exists}}})
	f.freeze(root)
}

func TestCTEDirectChildBodyRejectsDeclarationScopeCapture(t *testing.T) {
	f := newFixture(t)
	outer := f.slot(f.integer, false, 10, 1)
	input := f.scan(10, outer)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	bodyScope, err := f.b.AddScope(t.Context(), f.scope, []model.SlotID{outer})
	if err != nil {
		t.Fatal(err)
	}
	bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
	ref := bodyFixture.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: bodyScope, Layout: f.integer, Slot: outer}})
	bodyOut := bodyFixture.slot(f.integer, false, 0, 0)
	body := bodyFixture.rel(RelSpec{Rel: Rel{Kind: Values, Scope: bodyScope, Rows: 1}, Outputs: []model.SlotID{bodyOut}, Expressions: []ScalarID{ref}})
	requireInvalid(t, f.b.BindCTE(t.Context(), cte, body))
	graph, err := f.b.Build(t.Context(), input)
	if graph != nil {
		t.Fatal("body capturing declaration-scope slot produced a graph")
	}
	requireInvalid(t, err)
}

func TestCTEDirectChildBodyRejectsCaptureAddedAfterBinding(t *testing.T) {
	f := newFixture(t)
	outer := f.slot(f.integer, false, 10, 1)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	bodyScope, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
	bodyOut := bodyFixture.slot(f.integer, false, 0, 0)
	body := bodyFixture.rel(RelSpec{Rel: Rel{Kind: Values, Scope: bodyScope, Rows: 1}, Outputs: []model.SlotID{bodyOut}, Expressions: []ScalarID{bodyFixture.constant(1)}})
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	consumerOut := f.slot(f.integer, false, 0, 0)
	consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerOut}})
	if err := f.b.AddScopeCapture(t.Context(), bodyScope, outer); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), consumer)
	if graph != nil {
		t.Fatal("capture added after binding escaped CTE lifetime validation")
	}
	requireInvalid(t, err)
}

func TestCTEBodyRejectsNonChildScope(t *testing.T) {
	for _, scenario := range []string{"unrelated root", "grandchild"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			out := f.slot(f.integer, false, 0, 0)
			cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
			if err != nil {
				t.Fatal(err)
			}
			var parent ScopeID
			if scenario == "grandchild" {
				parent, err = f.b.AddScope(t.Context(), f.scope, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			bodyScope, err := f.b.AddScope(t.Context(), parent, nil)
			if err != nil {
				t.Fatal(err)
			}
			bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
			bodyOut := bodyFixture.slot(f.integer, false, 10, 1)
			body := bodyFixture.scan(10, bodyOut)
			consumerOut := f.slot(f.integer, false, 0, 0)
			consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerOut}})
			requireInvalid(t, f.b.BindCTE(t.Context(), cte, body))
			graph, err := f.b.Build(t.Context(), consumer)
			if graph != nil {
				t.Fatal("non-child CTE body produced a graph")
			}
			requireInvalid(t, err)
		})
	}
}

func TestCTEDirectChildBodyRejectsIncompatibleSchema(t *testing.T) {
	for _, scenario := range []string{"type", "nullability", "column count"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			out := f.slot(f.integer, false, 0, 0)
			cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Materialization: MaterializeAlways})
			if err != nil {
				t.Fatal(err)
			}
			bodyScope, err := f.b.AddScope(t.Context(), f.scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			bodyFixture := fixture{t: t, b: f.b, scope: bodyScope, integer: f.integer, boolean: f.boolean}
			layout := f.integer
			if scenario == "type" {
				layout = f.boolean
			}
			bodyOutputs := []model.SlotID{bodyFixture.slot(layout, scenario == "nullability", 10, 1)}
			if scenario == "column count" {
				bodyOutputs = append(bodyOutputs, bodyFixture.slot(f.integer, false, 10, 2))
			}
			body := bodyFixture.rel(RelSpec{Rel: Rel{Kind: Scan, Scope: bodyScope, Object: 10, DescriptorVersion: 1, Generation: 1}, Outputs: bodyOutputs})
			consumerOut := f.slot(f.integer, false, 0, 0)
			consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerOut}})
			requireInvalid(t, f.b.BindCTE(t.Context(), cte, body))
			graph, err := f.b.Build(t.Context(), consumer)
			if graph != nil {
				t.Fatal("incompatible CTE body schema produced a graph")
			}
			requireInvalid(t, err)
		})
	}
}
