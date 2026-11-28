package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestScopePromotionPreservesGroupedDirectSlots(t *testing.T) {
	f := newFixture(t)
	prior := f.slot(f.integer, false, 10, 1)
	input := f.slot(f.integer, false, 10, 2)
	grouped := f.slot(f.integer, true, 0, 0)
	source := promotionChild(t, f, []model.SlotID{prior})
	earlier := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Slot: prior}})
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.AddScopeCapture(t.Context(), source.scope, grouped); err != nil {
		t.Fatal(err)
	}
	direct := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
	directOnce := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: source.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{direct}})
	row := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
	rowOnce := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: source.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{row}})
	bytes := f.b.bytes
	if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, map[model.SlotID]model.SlotID{grouped: input}, []ScalarID{directOnce}, []ScalarID{rowOnce}); err != nil {
		t.Fatal(err)
	}
	if f.b.bytes != bytes {
		t.Errorf("arena quota charge changed: got%d want%d", f.b.bytes, bytes)
	}
	if scalar := f.b.graph.scalars[earlier-1]; scalar.Scope != source.scope || scalar.Slot != prior || scalar.Nullable {
		t.Errorf("earlier prefix scalar changed: %+v", scalar)
	}
	if f.b.CaptureCount(source.scope) != 1 {
		t.Errorf("earlier capture prefix changed: count=%d", f.b.CaptureCount(source.scope))
	}
	if slot, ok := f.b.Capture(source.scope, 0); !ok || slot != prior {
		t.Errorf("earlier capture slot=%d ok=%v want%d", slot, ok, prior)
	}
	if scalar := f.b.graph.scalars[direct-1]; scalar.Scope != f.scope || scalar.Slot != grouped || !scalar.Nullable {
		t.Errorf("direct grouped slot lost: got scope%d slot%d nullable%v; want scope%d slot%d nullabletrue", scalar.Scope, scalar.Slot, scalar.Nullable, f.scope, grouped)
	}
	if scalar := f.b.graph.scalars[directOnce-1]; scalar.Scope != f.scope || !scalar.Nullable {
		t.Errorf("direct transparent grouping-set nullability lost: scope%d nullable%v", scalar.Scope, scalar.Nullable)
	}
	if scalar := f.b.graph.scalars[row-1]; scalar.Scope != f.scope || scalar.Slot != input || scalar.Nullable {
		t.Errorf("row input was not restored: scope%d slot%d nullable%v", scalar.Scope, scalar.Slot, scalar.Nullable)
	}
	if scalar := f.b.graph.scalars[rowOnce-1]; scalar.Scope != f.scope || scalar.Nullable {
		t.Errorf("row transparent nullability was not restored: scope%d nullable%v", scalar.Scope, scalar.Nullable)
	}
}

func TestScopePromotionPreservesGroupedDirectCaptures(t *testing.T) {
	f := newFixture(t)
	prior := f.slot(f.integer, false, 10, 1)
	input := f.slot(f.integer, false, 10, 2)
	grouped := f.slot(f.integer, true, 0, 0)
	source := promotionChild(t, f, []model.SlotID{prior})
	earlier := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Slot: prior}})
	mark, err := f.b.MarkScope(t.Context(), source.scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.AddScopeCapture(t.Context(), source.scope, grouped); err != nil {
		t.Fatal(err)
	}
	directChild := promotionChild(t, source, []model.SlotID{grouped})
	direct := directChild.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: directChild.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
	directOnce := directChild.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: directChild.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{direct}})
	directOutput := directChild.slot(f.integer, true, 0, 0)
	directBody := directChild.rel(RelSpec{Rel: Rel{Kind: Values, Scope: directChild.scope, Rows: 1}, Outputs: []model.SlotID{directOutput}, Expressions: []ScalarID{directOnce}})
	directArgument := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: directBody}})
	rowChild := promotionChild(t, source, []model.SlotID{grouped})
	rowGrandchild := promotionChild(t, rowChild, []model.SlotID{grouped})
	row := rowGrandchild.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: rowGrandchild.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
	rowOnce := rowGrandchild.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: rowGrandchild.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{row}})
	rowOutput := rowGrandchild.slot(f.integer, true, 0, 0)
	rowBody := rowGrandchild.rel(RelSpec{Rel: Rel{Kind: Values, Scope: rowGrandchild.scope, Rows: 1}, Outputs: []model.SlotID{rowOutput}, Expressions: []ScalarID{rowOnce}})
	rowNested := rowChild.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: rowChild.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: rowBody}})
	rowChildOutput := rowChild.slot(f.integer, true, 0, 0)
	rowChildBody := rowChild.rel(RelSpec{Rel: Rel{Kind: Values, Scope: rowChild.scope, Rows: 1, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{rowChildOutput}, Expressions: []ScalarID{rowNested}})
	rowArgument := source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: rowChildBody}})
	bytes := f.b.bytes
	if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, map[model.SlotID]model.SlotID{grouped: input}, []ScalarID{directArgument}, []ScalarID{rowArgument}); err != nil {
		t.Fatal(err)
	}
	if f.b.bytes != bytes {
		t.Errorf("arena quota charge changed: got%d want%d", f.b.bytes, bytes)
	}
	if scalar := f.b.graph.scalars[earlier-1]; scalar.Scope != source.scope || scalar.Slot != prior {
		t.Errorf("earlier prefix scalar changed: %+v", scalar)
	}
	if f.b.CaptureCount(source.scope) != 1 {
		t.Errorf("earlier capture prefix changed: count=%d", f.b.CaptureCount(source.scope))
	}
	if slot, ok := f.b.Capture(source.scope, 0); !ok || slot != prior {
		t.Errorf("earlier capture slot=%d ok=%v want%d", slot, ok, prior)
	}
	for _, scope := range []ScopeID{directChild.scope, rowChild.scope} {
		if parent, ok := f.b.graph.ScopeParent(scope); !ok || parent != f.scope {
			t.Errorf("new child%d parent=%d ok=%v want%d", scope, parent, ok, f.scope)
		}
	}
	if parent, ok := f.b.graph.ScopeParent(rowGrandchild.scope); !ok || parent != rowChild.scope {
		t.Errorf("row descendant parent=%d ok=%v want%d", parent, ok, rowChild.scope)
	}
	if slot, ok := f.b.Capture(directChild.scope, 0); !ok || slot != grouped || f.b.CaptureCount(directChild.scope) != 1 {
		t.Errorf("direct child grouped capture lost: slot%d ok%v count%d want%d", slot, ok, f.b.CaptureCount(directChild.scope), grouped)
	}
	for _, scope := range []ScopeID{rowChild.scope, rowGrandchild.scope} {
		if slot, ok := f.b.Capture(scope, 0); !ok || slot != input || f.b.CaptureCount(scope) != 1 {
			t.Errorf("row child%d input capture not restored: slot%d ok%v count%d want%d", scope, slot, ok, f.b.CaptureCount(scope), input)
		}
	}
	if scalar := f.b.graph.scalars[direct-1]; scalar.Scope != directChild.scope || scalar.Slot != grouped || !scalar.Nullable {
		t.Errorf("direct child grouped scalar lost: scope%d slot%d nullable%v", scalar.Scope, scalar.Slot, scalar.Nullable)
	}
	if !f.b.graph.scalars[directOnce-1].Nullable {
		t.Error("direct child transparent grouping-set nullability lost")
	}
	if scalar := f.b.graph.scalars[row-1]; scalar.Scope != rowGrandchild.scope || scalar.Slot != input || scalar.Nullable {
		t.Errorf("row child input scalar not restored: scope%d slot%d nullable%v", scalar.Scope, scalar.Slot, scalar.Nullable)
	}
	if f.b.graph.scalars[rowOnce-1].Nullable {
		t.Error("row child transparent nullability not restored")
	}
}

func TestScopePromotionRejectsAffectedPhaseSharingBeforeMutation(t *testing.T) {
	for _, kind := range []string{"scalar", "capture"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			input := f.slot(f.integer, false, 10, 1)
			grouped := f.slot(f.integer, true, 0, 0)
			source := promotionChild(t, f, nil)
			mark, err := f.b.MarkScope(t.Context(), source.scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.b.AddScopeCapture(t.Context(), source.scope, grouped); err != nil {
				t.Fatal(err)
			}
			var argument ScalarID
			var childScope ScopeID
			if kind == "scalar" {
				argument = source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Nullable: true, Slot: grouped}})
			} else {
				child := promotionChild(t, source, []model.SlotID{grouped})
				childScope = child.scope
				value := child.constant(1)
				output := child.slot(f.integer, false, 0, 0)
				body := child.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{value}})
				argument = source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: body}})
			}
			bytes := f.b.bytes
			_, err = f.b.PromoteScope(t.Context(), mark, f.scope, map[model.SlotID]model.SlotID{grouped: input}, []ScalarID{argument}, []ScalarID{argument})
			requireInvalid(t, err)
			if mark.used || f.b.bytes != bytes || f.b.graph.scalars[argument-1].Scope != source.scope {
				t.Fatal("rejected phase sharing mutated the construction")
			}
			if slot, ok := f.b.Capture(source.scope, 0); !ok || slot != grouped || f.b.CaptureCount(source.scope) != 1 {
				t.Fatal("rejected phase sharing changed source captures")
			}
			if childScope != 0 {
				if parent, ok := f.b.graph.ScopeParent(childScope); !ok || parent != source.scope {
					t.Fatal("rejected phase sharing reparented a child")
				}
				if slot, ok := f.b.Capture(childScope, 0); !ok || slot != grouped {
					t.Fatal("rejected phase sharing replaced a child capture")
				}
			}
		})
	}
}

func TestScopePromotionAllowsUnaffectedPhaseSharing(t *testing.T) {
	for _, kind := range []string{"constant", "slot", "subquery"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			prior := f.slot(f.integer, false, 10, 1)
			input := f.slot(f.integer, false, 10, 2)
			grouped := f.slot(f.integer, true, 0, 0)
			source := promotionChild(t, f, nil)
			mark, err := f.b.MarkScope(t.Context(), source.scope)
			if err != nil {
				t.Fatal(err)
			}
			var argument ScalarID
			var childScope ScopeID
			switch kind {
			case "constant":
				argument = source.constant(1)
			case "slot":
				if err := f.b.AddScopeCapture(t.Context(), source.scope, prior); err != nil {
					t.Fatal(err)
				}
				argument = source.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: source.scope, Layout: f.integer, Slot: prior}})
			case "subquery":
				if err := f.b.AddScopeCapture(t.Context(), source.scope, prior); err != nil {
					t.Fatal(err)
				}
				child := promotionChild(t, source, []model.SlotID{prior})
				childScope = child.scope
				value := child.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child.scope, Layout: f.integer, Slot: prior}})
				output := child.slot(f.integer, false, 0, 0)
				body := child.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{value}})
				argument = source.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: source.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: body}})
			}
			bytes := f.b.bytes
			if _, err := f.b.PromoteScope(t.Context(), mark, f.scope, map[model.SlotID]model.SlotID{grouped: input}, []ScalarID{argument}, []ScalarID{argument}); err != nil {
				t.Fatal(err)
			}
			if f.b.bytes != bytes || f.b.graph.scalars[argument-1].Scope != f.scope {
				t.Fatal("unaffected shared expression was not promoted without changing quota charges")
			}
			if childScope != 0 {
				if parent, ok := f.b.graph.ScopeParent(childScope); !ok || parent != f.scope {
					t.Fatal("unaffected shared subquery was not reparented")
				}
				if slot, ok := f.b.Capture(childScope, 0); !ok || slot != prior {
					t.Fatal("unaffected shared capture changed")
				}
			}
		})
	}
}

func TestScopePromotionRejectsPhaseRootsOutsideMark(t *testing.T) {
	for _, kind := range []string{"zero", "earlier", "unknown", "child"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			source := promotionChild(t, f, nil)
			earlier := source.constant(1)
			mark, err := f.b.MarkScope(t.Context(), source.scope)
			if err != nil {
				t.Fatal(err)
			}
			later := source.constant(2)
			var root ScalarID
			switch kind {
			case "earlier":
				root = earlier
			case "unknown":
				root = ScalarID(len(f.b.graph.scalars) + 1)
			case "child":
				root = promotionChild(t, source, nil).constant(3)
			}
			bytes := f.b.bytes
			_, err = f.b.PromoteScope(t.Context(), mark, f.scope, nil, []ScalarID{root}, nil)
			requireInvalid(t, err)
			if mark.used || f.b.bytes != bytes || f.b.graph.scalars[earlier-1].Scope != source.scope || f.b.graph.scalars[later-1].Scope != source.scope {
				t.Fatal("invalid phase root mutated the construction")
			}
		})
	}
}
