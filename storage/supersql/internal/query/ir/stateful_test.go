package ir

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRecursiveCTEHasExplicitBackedge(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	zero := f.constant(0)
	anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{zero}})
	member := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: []model.SlotID{out}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: cte, All: true}, Inputs: []RelID{anchor, member}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), cte, union); err != nil {
		t.Fatal(err)
	}
	root := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{out}})
	g := f.freeze(root)
	decl, _ := g.CTE(cte)
	relation, _ := g.Rel(decl.Body)
	if !decl.Recursive || relation.Kind != RecursiveUnion || !relation.All {
		t.Fatalf("recursive semantics lost: %+v, %+v", decl, relation)
	}
	ref, _ := g.Rel(member)
	if !ref.RecursiveReference {
		t.Fatal("recursive working-table reference lost")
	}
}

func TestRejectsRecursiveReferenceInAnchor(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: []model.SlotID{out}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: cte}, Inputs: []RelID{ref, ref}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), cte, union); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), union)
	if graph != nil {
		t.Fatal("illegal recursive anchor frozen")
	}
	requireInvalid(t, err)
}
func TestRejectsMutuallyRecursiveCTEs(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	first, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
	if err != nil {
		t.Fatal(err)
	}
	firstRef := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: first}, Outputs: []model.SlotID{out}})
	secondRef := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: second}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), first, secondRef); err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), second, firstRef); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), firstRef)
	if graph != nil {
		t.Fatal("mutual recursion frozen")
	}
	requireInvalid(t, err)
}
func TestRejectsRecursiveConsumerAsStatementRoot(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	zero := f.constant(0)
	anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{zero}})
	ref := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: []model.SlotID{out}})
	union := f.rel(RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: cte}, Inputs: []RelID{anchor, ref}, Outputs: []model.SlotID{out}})
	if err := f.b.BindCTE(t.Context(), cte, union); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), ref)
	if graph != nil {
		t.Fatal("working table escaped as root")
	}
	requireInvalid(t, err)
}
func TestScalarApplyCapturesOuterInput(t *testing.T) {
	f := newFixture(t)
	leftSlot := f.slot(f.integer, false, 10, 1)
	left := f.scan(10, leftSlot)
	child, err := f.b.AddScope(t.Context(), f.scope, []model.SlotID{leftSlot})
	if err != nil {
		t.Fatal(err)
	}
	childSlot, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: child, Layout: f.integer})
	if err != nil {
		t.Fatal(err)
	}
	outer := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child, Layout: f.integer, Slot: leftSlot}})
	right := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child, Rows: 1}, Outputs: []model.SlotID{childSlot}, Expressions: []ScalarID{outer}})
	result := f.slot(f.integer, true, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Apply, Scope: f.scope, ApplyKind: ScalarApply, Effects: Effects{MayError: true}}, Inputs: []RelID{left, right}, Outputs: []model.SlotID{leftSlot, result}})
	g := f.freeze(root)
	n, _ := g.Rel(root)
	if !n.Effects.MayError {
		t.Fatal("scalar Apply lost cardinality error")
	}
	capture, ok := g.Capture(child, 0)
	if !ok || capture != leftSlot {
		t.Fatalf("capture = %v, %v", capture, ok)
	}
}
func TestOuterJoinRequiresNullableExtendedOutputs(t *testing.T) {
	f := newFixture(t)
	leftSlot, rightSlot := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 20, 1)
	left, right := f.scan(10, leftSlot), f.scan(20, rightSlot)
	predicate := f.truth()
	_, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Join, Scope: f.scope, JoinKind: LeftJoin, Predicate: predicate}, Inputs: []RelID{left, right}, Outputs: []model.SlotID{leftSlot, rightSlot}})
	requireInvalid(t, err)
}
func TestGroupingSetsPreserveOmittedKeyNullability(t *testing.T) {
	f := newFixture(t)
	inputSlot := f.slot(f.integer, false, 10, 1)
	scan := f.scan(10, inputSlot)
	key := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	group, count := f.slot(f.integer, true, 0, 0), f.slot(f.integer, false, 0, 0)
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 100, Result: f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	root := f.rel(RelSpec{Rel: Rel{Kind: Aggregate, Scope: f.scope}, Inputs: []RelID{scan}, Outputs: []model.SlotID{group, count}, Expressions: []ScalarID{key}, Bindings: []Binding{{Slot: group, Expr: key}}, GroupingSets: [][]uint32{{0}, {}}, Aggregates: []AggregateSpec{{AggregateCall: AggregateCall{Signature: signature, Function: 100, Result: count}}}})
	g := f.freeze(root)
	if width, ok := g.GroupingSetWidth(root, 1); !ok || width != 0 {
		t.Fatalf("grand-total grouping set = %v, %v", width, ok)
	}
	if slot, _ := g.Slot(group); !slot.Nullable {
		t.Fatal("omitted grouping key is not nullable")
	}
}
func TestWindowFrameRetainsTypedBounds(t *testing.T) {
	f := newFixture(t)
	inputSlot := f.slot(f.integer, false, 10, 1)
	scan := f.scan(10, inputSlot)
	key := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	offset := f.constant(2)
	result := f.slot(f.integer, false, 0, 0)
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 101, Result: f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Mode: GroupsFrame, Start: FrameBound{Kind: OffsetPreceding, Offset: offset}, End: FrameBound{Kind: CurrentRow}, Exclusion: ExcludeTies}
	root := f.rel(RelSpec{Rel: Rel{Kind: Window, Scope: f.scope, Frame: frame, Effects: Effects{MayError: true}}, Inputs: []RelID{scan}, Outputs: []model.SlotID{inputSlot, result}, Order: []OrderKey{{Expr: key, Direction: Ascending, Nulls: NullsLast, Operator: 1}}, Windows: []WindowSpec{{WindowCall: WindowCall{Signature: signature, Function: 101, Result: result}}}})
	g := f.freeze(root)
	n, _ := g.Rel(root)
	if n.Frame != frame {
		t.Fatalf("frame = %+v", n.Frame)
	}
}
func TestRangeFrameRequiresConcreteInRangeSignature(t *testing.T) {
	f := newFixture(t)
	inputSlot := f.slot(f.integer, false, 10, 1)
	scan := f.scan(10, inputSlot)
	key := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	offset := f.constant(2)
	result := f.slot(f.integer, false, 0, 0)
	windowSignature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 101, Result: f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	inRange, err := f.b.AddSignature(t.Context(), SignatureSpec{
		Signature:  Signature{Function: 4126, Result: f.boolean, Strict: true, Nullable: true, Effects: Effects{MayError: true}},
		Parameters: []LayoutID{f.integer, f.integer, f.integer, f.boolean, f.boolean},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := RelSpec{
		Rel:     Rel{Kind: Window, Scope: f.scope, Effects: Effects{MayError: true}, Frame: Frame{Mode: RangeFrame, Start: FrameBound{Kind: OffsetPreceding, Offset: offset, InRange: inRange}, End: FrameBound{Kind: CurrentRow}}},
		Inputs:  []RelID{scan},
		Outputs: []model.SlotID{inputSlot, result},
		Order:   []OrderKey{{Expr: key, Direction: Ascending, Nulls: NullsLast, Operator: 1}},
		Windows: []WindowSpec{{WindowCall: WindowCall{Signature: windowSignature, Function: 101, Result: result}}},
	}
	root := f.rel(spec)
	graph := f.freeze(root)
	relation, _ := graph.Rel(root)
	if relation.Frame.Start.InRange != inRange {
		t.Fatalf("in_range signature = %d, want %d", relation.Frame.Start.InRange, inRange)
	}
	bad := newFixture(t)
	badInput := bad.slot(bad.integer, false, 10, 1)
	badScan := bad.scan(10, badInput)
	badKey := bad.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: bad.scope, Layout: bad.integer, Slot: badInput}})
	badOffset := bad.constant(2)
	badResult := bad.slot(bad.integer, false, 0, 0)
	badWindowSignature, err := bad.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 101, Result: bad.integer}})
	if err != nil {
		t.Fatal(err)
	}
	badRoot, err := bad.b.AddRel(t.Context(), RelSpec{
		Rel:     Rel{Kind: Window, Scope: bad.scope, Effects: Effects{MayError: true}, Frame: Frame{Mode: RangeFrame, Start: FrameBound{Kind: OffsetPreceding, Offset: badOffset}, End: FrameBound{Kind: CurrentRow}}},
		Inputs:  []RelID{badScan},
		Outputs: []model.SlotID{badInput, badResult},
		Order:   []OrderKey{{Expr: badKey, Direction: Ascending, Nulls: NullsLast, Operator: 1}},
		Windows: []WindowSpec{{WindowCall: WindowCall{Signature: badWindowSignature, Function: 101, Result: badResult}}},
	})
	if err == nil {
		var rejected *Graph
		rejected, err = bad.b.Build(t.Context(), badRoot)
		if rejected != nil {
			t.Fatal("RANGE frame without in_range signature was frozen")
		}
	}
	requireInvalid(t, err)
}
func TestModificationSeparatesAssignmentsFromReturning(t *testing.T) {
	f := newFixture(t)
	value := f.constant(42)
	inputSlot := f.slot(f.integer, false, 0, 0)
	input := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{inputSlot}, Expressions: []ScalarID{value}})
	inputExpr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	target, result := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Modify, Scope: f.scope, Object: 10, DescriptorVersion: 2, Generation: 1, EncodingVersion: 1, ModifyKind: Insert, ModifyTarget: PhysicalModifyTarget, Effects: Effects{Volatility: Volatile, MayError: true}}, Inputs: []RelID{input}, Assignments: []Binding{{Slot: target, Expr: inputExpr}}, Outputs: []model.SlotID{result}, Bindings: []Binding{{Slot: result, Expr: inputExpr}}})
	g := f.freeze(root)
	assignment, _ := g.Assignment(root, 0)
	returning, _ := g.Binding(root, 0)
	if assignment.Slot != target || returning.Slot != result {
		t.Fatalf("assignment/RETURNING confused: %+v, %+v", assignment, returning)
	}
}
func TestLogicalViewModificationUsesCatalogIdentityWithoutPhysicalIdentity(t *testing.T) {
	f := newFixture(t)
	value := f.constant(42)
	inputSlot := f.slot(f.integer, false, 0, 0)
	input := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{inputSlot}, Expressions: []ScalarID{value}})
	inputExpr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	target := f.slot(f.integer, false, 10, 1)
	root := f.rel(RelSpec{Rel: Rel{Kind: Modify, Scope: f.scope, Object: 10, DescriptorVersion: 2, ModifyKind: Insert, ModifyTarget: LogicalViewModifyTarget, Effects: Effects{Volatility: Volatile, MayError: true}}, Inputs: []RelID{input}, Assignments: []Binding{{Slot: target, Expr: inputExpr}}})
	f.freeze(root)
}

func TestRejectsUnderstatedCaseEffects(t *testing.T) {
	f := newFixture(t)
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 5, Result: f.integer, Effects: Effects{MayError: true}}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Signature: signature, Function: 5, Effects: Effects{MayError: true}}})
	condition, zero := f.truth(), f.constant(0)
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Case, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{condition, zero, call}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("understated CASE effects = %v", err)
	}
}
