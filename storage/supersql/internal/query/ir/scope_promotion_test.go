package ir

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func promotionChild(t *testing.T, parent fixture, captures []model.SlotID) fixture {
	t.Helper()
	id, err := parent.b.AddScope(t.Context(), parent.scope, captures)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{t: t, b: parent.b, scope: id, integer: parent.integer, boolean: parent.boolean}
}

func TestScopePromotionPreservesEarlierCapturesAndChildQueries(t *testing.T) {
	f := newFixture(t)
	prior := f.slot(f.integer, false, 10, 1)
	input := f.slot(f.integer, false, 10, 2)
	scan := f.rel(RelSpec{Rel: Rel{Kind: Scan, Scope: f.scope, Object: 10, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{prior, input}})
	source := promotionChild(t, f, []model.SlotID{prior})
	earlier := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Slot: prior}})
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.AddScopeCapture(t.Context(), source.scope, input); err != nil {
		t.Fatal(err)
	}
	child := promotionChild(t, source, []model.SlotID{input})
	value := child.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child.scope, Layout: f.integer, Slot: input}})
	childOutput := child.slot(f.integer, false, 0, 0)
	body := child.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child.scope, Rows: 1}, Outputs: []model.SlotID{childOutput}, Expressions: []ScalarID{value}})
	argument := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: body}})
	bytes := f.b.bytes
	if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, nil); err != nil {
		t.Fatal(err)
	}
	if f.b.bytes != bytes {
		t.Fatalf("promotion changed arena quota charge: got %d, want %d", f.b.bytes, bytes)
	}
	if f.b.graph.scalars[earlier-1].Scope != source.scope || f.b.CaptureCount(source.scope) != 1 {
		t.Fatal("earlier expression or capture prefix changed")
	}
	if captured, ok := f.b.Capture(source.scope, 0); !ok || captured != prior {
		t.Fatal("earlier capture was not preserved")
	}
	if parent, ok := f.b.graph.ScopeParent(child.scope); !ok || parent != f.scope {
		t.Fatal("argument subquery was not reparented")
	}
	output := f.slot(f.integer, true, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope, Effects: Effects{MayError: true}}, Inputs: []RelID{scan}, Outputs: []model.SlotID{output}, Bindings: []Binding{{Slot: output, Expr: argument}}})
	f.freeze(root)
}

func TestScopePromotionRestoresGroupedInputsAndNullability(t *testing.T) {
	f := newFixture(t)
	input := f.slot(f.integer, false, 10, 1)
	grouped := f.slot(f.integer, true, 0, 0)
	scan := f.scan(10, input)
	source := promotionChild(t, f, nil)
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.AddScopeCapture(t.Context(), source.scope, grouped); err != nil {
		t.Fatal(err)
	}
	child := promotionChild(t, source, []model.SlotID{grouped})
	value := child.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
	once := child.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: child.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{value}})
	childOutput := child.slot(f.integer, true, 0, 0)
	body := child.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child.scope, Rows: 1}, Outputs: []model.SlotID{childOutput}, Expressions: []ScalarID{once}})
	argument := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: body}})
	if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, map[model.SlotID]model.SlotID{grouped: input}); err != nil {
		t.Fatal(err)
	}
	if captured, ok := f.b.Capture(child.scope, 0); !ok || captured != input || f.b.CaptureCount(source.scope) != 0 {
		t.Fatal("grouped capture was not replaced by its aggregate input")
	}
	if f.b.graph.scalars[value-1].Nullable || f.b.graph.scalars[once-1].Nullable {
		t.Fatal("transparent scalar nullability no longer matches the input")
	}
	output := f.slot(f.integer, true, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope, Effects: Effects{MayError: true}}, Inputs: []RelID{scan}, Outputs: []model.SlotID{output}, Bindings: []Binding{{Slot: output, Expr: argument}}})
	f.freeze(root)
}

func TestScopePromotionRejectsEarlierOperands(t *testing.T) {
	f := newFixture(t)
	source := promotionChild(t, f, nil)
	prior := source.constant(1)
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	later := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: source.scope, Layout: f.integer}, Operands: []ScalarID{prior}})
	_, err = f.b.PromoteScope(t.Context(), mark, f.scope, nil)
	requireInvalid(t, err)
	if f.b.graph.scalars[prior-1].Scope != source.scope || f.b.graph.scalars[later-1].Scope != source.scope {
		t.Fatal("rejected promotion changed existing scalar scopes")
	}
}

func TestScopePromotionRejectsReusedMarks(t *testing.T) {
	f := newFixture(t)
	source := promotionChild(t, f, nil)
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	source.constant(1)
	if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, nil); err != nil {
		t.Fatal(err)
	}
	_, err = f.b.PromoteScope(t.Context(), mark, f.scope, nil)
	requireInvalid(t, err)
}

func TestScopePromotionPreservesNestedCTEBarrier(t *testing.T) {
	f := newFixture(t)
	source := promotionChild(t, f, nil)
	output := source.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: source.scope, Outputs: []model.SlotID{output}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	body := promotionChild(t, source, nil)
	constant := body.constant(1)
	bodyOutput := body.slot(f.integer, false, 0, 0)
	bodyRel := body.rel(RelSpec{Rel: Rel{Kind: Values, Scope: body.scope, Rows: 1}, Outputs: []model.SlotID{bodyOutput}, Expressions: []ScalarID{constant}})
	if err := f.b.BindCTE(t.Context(), cte, bodyRel); err != nil {
		t.Fatal(err)
	}
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	consumer := promotionChild(t, source, nil)
	consumerOutput := consumer.slot(f.integer, false, 0, 0)
	consumerRel := consumer.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: consumer.scope, CTE: cte}, Outputs: []model.SlotID{consumerOutput}})
	argument := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: consumerRel}})
	_, err = f.b.PromoteScope(t.Context(), mark, f.scope, nil)
	if !errors.Is(err, ErrScopeDependency) {
		t.Fatalf("nested CTE promotion = %v, want dependency error", err)
	}
	if f.b.graph.scalars[argument-1].Scope != source.scope {
		t.Fatal("rejected CTE promotion changed the argument")
	}
	if parent, ok := f.b.graph.ScopeParent(consumer.scope); !ok || parent != source.scope {
		t.Fatal("rejected CTE promotion changed the consumer parent")
	}
}
