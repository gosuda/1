package ir

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type fixture struct {
	t                *testing.T
	b                *Builder
	scope            ScopeID
	integer, boolean LayoutID
}

func limitsForTest() Limits {
	return Limits{MaxNodes: 1024, MaxOperands: 4096, MaxSlots: 256, MaxDepth: 64, MaxBytes: 1 << 20, SourceBytes: 4096}
}
func newFixture(t *testing.T) fixture {
	t.Helper()
	b, err := NewBuilder(limitsForTest())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t: t, b: b}
	f.scope, err = b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.integer, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	f.boolean, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) slot(layout LayoutID, nullable bool, object model.ObjectID, column model.ColumnID) model.SlotID {
	f.t.Helper()
	id, err := f.b.AddSlot(f.t.Context(), SlotInfo{Scope: f.scope, Layout: layout, Nullable: nullable, Object: object, Column: column})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) scalar(spec ScalarSpec) ScalarID {
	f.t.Helper()
	id, err := f.b.AddScalar(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) rel(spec RelSpec) RelID {
	f.t.Helper()
	id, err := f.b.AddRel(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) constant(value int64) ScalarID {
	return f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer}, Value: model.Int64Val(value)})
}
func (f fixture) truth() ScalarID {
	return f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.boolean}, Value: model.BoolVal(true)})
}
func (f fixture) scan(object model.ObjectID, slot model.SlotID) RelID {
	return f.rel(RelSpec{Rel: Rel{Kind: Scan, Scope: f.scope, Object: object, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{slot}})
}
func (f fixture) freeze(root RelID) *Graph {
	f.t.Helper()
	graph, err := f.b.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	return graph
}
func requireInvalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want invalid IR", err)
	}
}

func TestScanFilterProjectJoinCTE(t *testing.T) {
	f := newFixture(t)
	leftSlot, rightSlot := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 20, 1)
	left, right := f.scan(10, leftSlot), f.scan(20, rightSlot)
	predicate := f.truth()
	join := f.rel(RelSpec{Rel: Rel{Kind: Join, Scope: f.scope, JoinKind: InnerJoin, Predicate: predicate}, Inputs: []RelID{left, right}, Outputs: []model.SlotID{leftSlot, rightSlot}})
	filtered := f.rel(RelSpec{Rel: Rel{Kind: Filter, Scope: f.scope, Predicate: predicate}, Inputs: []RelID{join}, Outputs: []model.SlotID{leftSlot, rightSlot}})
	expr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: rightSlot}})
	projectSlot := f.slot(f.integer, false, 0, 0)
	bindings := []Binding{{Slot: projectSlot, Expr: expr}}
	project := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{filtered}, Outputs: []model.SlotID{projectSlot}, Bindings: bindings})
	bindings[0].Expr = predicate
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{projectSlot}, Materialization: MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, project); err != nil {
		t.Fatal(err)
	}
	consumerSlot := f.slot(f.integer, false, 0, 0)
	consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerSlot}})
	g := f.freeze(consumer)
	binding, ok := g.Binding(project, 0)
	if !ok || binding.Expr != expr {
		t.Fatalf("owned projection binding = %+v, %v", binding, ok)
	}
	decl, ok := g.CTE(cte)
	if !ok || decl.Body != project || decl.Materialization != MaterializeAlways {
		t.Fatalf("CTE = %+v, %v", decl, ok)
	}
	if got, ok := g.Input(join, 1); !ok || got != right {
		t.Fatalf("join right input = %v, %v", got, ok)
	}
	if _, ok := g.Input(join, math.MaxUint32); ok {
		t.Fatal("out-of-bounds traversal accepted")
	}
	if _, err := f.b.AddScope(t.Context(), 0, nil); !errors.Is(err, ErrFrozen) {
		t.Fatalf("mutation after freeze = %v", err)
	}
	if graph, err := f.b.Build(t.Context(), consumer); graph != nil || !errors.Is(err, ErrFrozen) {
		t.Fatalf("second Build = %v, %v", graph, err)
	}
}

func TestCasePreservesTypedBranchesAndEffects(t *testing.T) {
	f := newFixture(t)
	effects := Effects{Volatility: Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 99, Result: f.integer, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 99, Signature: signature, Effects: effects}})
	condition, zero := f.truth(), f.constant(0)
	branch := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Case, Scope: f.scope, Layout: f.integer, Effects: effects, Span: Span{Start: 7, End: 39}}, Operands: []ScalarID{condition, zero, call}})
	slot := f.slot(f.integer, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1, Effects: effects}, Outputs: []model.SlotID{slot}, Expressions: []ScalarID{branch}})
	g := f.freeze(root)
	n, _ := g.Scalar(branch)
	if n.Layout != f.integer || n.Effects != effects || n.Nullable || n.Span != (Span{Start: 7, End: 39}) {
		t.Fatalf("CASE metadata = %+v", n)
	}
	if last, ok := g.Operand(branch, 2); !ok || last != call {
		t.Fatalf("CASE ELSE = %v, %v", last, ok)
	}
}

func TestConstantOwnsNestedPayloadAndLayouts(t *testing.T) {
	f := newFixture(t)
	leaf := model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64, Modifier: -1}}
	layout := model.TypeLayout{Type: model.Type{ID: 9001, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 1000, Storage: model.TypeArray}, Children: []model.TypeLayout{leaf}}, {Type: model.Type{ID: 17, Storage: model.TypeBytes}}}}
	layoutID, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	bits := uint64(0x7ff8000000000035)
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{{Type: model.TypeArray, ArrayVal: []model.Value{model.Float64Val(math.Float64frombits(bits)), model.NullVal(model.TypeFloat64)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -3}}}, model.BytesVal([]byte{1, 2, 3})}}
	id := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: layoutID}, Value: value})
	value.ArrayVal[0].ArrayVal[0].FloatVal = 0
	value.ArrayVal[1].BytesVal[0] = 9
	layout.Children[0].Children[0].Type.ID = 999
	slot := f.slot(layoutID, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{slot}, Expressions: []ScalarID{id}})
	g := f.freeze(root)
	owned, err := g.CloneConstant(t.Context(), id, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if math.Float64bits(owned.ArrayVal[0].ArrayVal[0].FloatVal) != bits || !owned.ArrayVal[0].ArrayVal[1].IsNull || owned.ArrayVal[0].Dimensions[0].LowerBound != -3 || owned.ArrayVal[1].BytesVal[0] != 1 {
		t.Fatalf("constant lost bits, NULL, shape, or ownership: %+v", owned)
	}
	owned.ArrayVal[1].BytesVal[0] = 7
	again, err := g.CloneConstant(t.Context(), id, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if again.ArrayVal[1].BytesVal[0] != 1 {
		t.Fatal("constant accessor leaked mutable bytes")
	}
	arrayID, _ := g.LayoutChild(layoutID, 0)
	leafID, _ := g.LayoutChild(arrayID, 0)
	typ, _ := g.Type(leafID)
	if typ.ID != 701 || typ.Modifier != -1 {
		t.Fatalf("nested type metadata = %+v", typ)
	}
}

func TestRejectsInvisibleSlot(t *testing.T) {
	f := newFixture(t)
	outer := f.slot(f.integer, false, 0, 0)
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child, Layout: f.integer, Slot: outer}})
	requireInvalid(t, err)
}
func TestRejectsUnavailableRelationSlot(t *testing.T) {
	f := newFixture(t)
	left, right := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 20, 1)
	scan := f.scan(10, left)
	expr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: right}})
	out := f.slot(f.integer, false, 0, 0)
	_, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{scan}, Outputs: []model.SlotID{out}, Bindings: []Binding{{Slot: out, Expr: expr}}})
	requireInvalid(t, err)
	if graph, err := f.b.Build(t.Context(), scan); graph != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("poisoned builder returned graph: %v, %v", graph, err)
	}
}
func TestRejectsForwardHandles(t *testing.T) {
	t.Run("scalar", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Bool, Scope: f.scope, Layout: f.boolean, BoolOp: Not}, Operands: []ScalarID{1}})
		requireInvalid(t, err)
	})
	t.Run("relation", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{1}})
		requireInvalid(t, err)
	})
}
func TestRejectsCaseResultMismatch(t *testing.T) {
	f := newFixture(t)
	condition, value := f.truth(), f.constant(1)
	_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Case, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{condition, value, condition}})
	requireInvalid(t, err)
}
func TestRejectsCallSignatureMismatch(t *testing.T) {
	f := newFixture(t)
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 7, Result: f.integer}, Parameters: []LayoutID{f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	arg := f.truth()
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 7, Signature: signature}, Operands: []ScalarID{arg}})
	requireInvalid(t, err)
}
func TestRejectsCopiedBuilder(t *testing.T) {
	f := newFixture(t)
	copied := *f.b
	_, err := copied.AddScope(t.Context(), 0, nil)
	requireInvalid(t, err)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
	g := f.freeze(root)
	if g.Root() != root {
		t.Fatal("copy attempt damaged original builder")
	}
}
func TestRejectsMalformedPayloadRange(t *testing.T) {
	f := newFixture(t)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
	f.b.graph.rels[root-1].outputs = extent{start: math.MaxUint32, count: 2}
	graph, err := f.b.Build(t.Context(), root)
	if graph != nil {
		t.Fatal("malformed graph escaped verifier")
	}
	requireInvalid(t, err)
}
func TestResourceAndCancellationFailures(t *testing.T) {
	t.Run("capacity bytes", func(t *testing.T) {
		limits := limitsForTest()
		limits.MaxBytes = 1
		b, err := NewBuilder(limits)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.AddScope(t.Context(), 0, nil)
		var sqlError *model.Error
		if !errors.As(err, &sqlError) || sqlError.Code != "54000" {
			t.Fatalf("byte limit error = %v", err)
		}
	})
	t.Run("node count", func(t *testing.T) {
		limits := limitsForTest()
		limits.MaxNodes = 1
		b, err := NewBuilder(limits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddScope(t.Context(), 0, nil); err != nil {
			t.Fatal(err)
		}
		_, err = b.AddScope(t.Context(), 0, nil)
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("node limit = %v", err)
		}
	})
	t.Run("cancelled build", func(t *testing.T) {
		f := newFixture(t)
		root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		graph, err := f.b.Build(ctx, root)
		if graph != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Build = %v, %v", graph, err)
		}
	})
}

func TestIncrementalCaptureOfGroupedOutput(t *testing.T) {
	f := newFixture(t)
	key, ungrouped := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 10, 2)
	input := f.rel(RelSpec{Rel: Rel{Kind: Scan, Scope: f.scope, Object: 10, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{key, ungrouped}})
	keyExpr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: key}})
	group := f.slot(f.integer, false, 0, 0)
	grouped := f.rel(RelSpec{Rel: Rel{Kind: Aggregate, Scope: f.scope}, Inputs: []RelID{input}, Outputs: []model.SlotID{group}, Expressions: []ScalarID{keyExpr}, Bindings: []Binding{{Slot: group, Expr: keyExpr}}, GroupingSets: [][]uint32{{0}}})
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.AddScopeCapture(t.Context(), child, group); err != nil {
		t.Fatal(err)
	}
	inner := fixture{t: t, b: f.b, scope: child, integer: f.integer, boolean: f.boolean}
	ref := inner.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child, Layout: f.integer, Slot: group}})
	innerOut := inner.slot(f.integer, false, 0, 0)
	subquery := inner.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child, Rows: 1}, Outputs: []model.SlotID{innerOut}, Expressions: []ScalarID{ref}})
	exists := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.boolean, SubqueryKind: ExistsSubquery, Relation: subquery}})
	out := f.slot(f.boolean, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{grouped}, Outputs: []model.SlotID{out}, Bindings: []Binding{{Slot: out, Expr: exists}}})
	g := f.freeze(root)
	if got, ok := g.Capture(child, 0); !ok || got != group || g.CaptureCount(child) != 1 {
		t.Fatalf("grouped dependency = %v, %v, count %d", got, ok, g.CaptureCount(child))
	}
	if err := f.b.AddScopeCapture(t.Context(), child, ungrouped); !errors.Is(err, ErrFrozen) {
		t.Fatalf("capture after Build = %v", err)
	}
	if _, ok := g.Capture(child, 1); ok {
		t.Fatal("frozen graph acquired an ungrouped dependency")
	}
}

func TestIncrementalCapturesHaveLinearStorage(t *testing.T) {
	limits := limitsForTest()
	limits.MaxOperands = 256
	b, err := NewBuilder(limits)
	if err != nil {
		t.Fatal(err)
	}
	rootScope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.AddScope(t.Context(), rootScope, nil)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := b.AddScope(t.Context(), child, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots := make([]model.SlotID, 64)
	for i := range slots {
		slots[i], err = b.AddSlot(t.Context(), SlotInfo{Scope: rootScope, Layout: layout})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.AddScopeCapture(t.Context(), child, slots[i]); err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		if err := b.AddScopeCapture(t.Context(), grandchild, slots[i]); err != nil {
			t.Fatalf("grandparent capture %d: %v", i, err)
		}
	}
	for range 100 {
		if err := b.AddScopeCapture(t.Context(), child, slots[0]); err != nil {
			t.Fatalf("idempotent capture: %v", err)
		}
	}
	root, err := b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Values, Scope: rootScope, Rows: 1}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []ScopeID{child, grandchild} {
		if g.CaptureCount(scope) != 64 {
			t.Fatalf("scope %d captures = %d", scope, g.CaptureCount(scope))
		}
		for i, want := range slots {
			if got, ok := g.Capture(scope, uint32(i)); !ok || got != want {
				t.Fatalf("scope %d capture %d = %v, %v; want %d", scope, i, got, ok, want)
			}
		}
	}
}

func TestRejectsInvalidIncrementalCapture(t *testing.T) {
	for _, scenario := range []string{"zero scope", "forward scope", "zero slot", "forward slot", "own slot", "sibling slot", "descendant slot", "unrelated root"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			outer := f.slot(f.integer, false, 0, 0)
			child, err := f.b.AddScope(t.Context(), f.scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			owner := child
			switch scenario {
			case "sibling slot":
				owner, err = f.b.AddScope(t.Context(), f.scope, nil)
			case "descendant slot":
				owner, err = f.b.AddScope(t.Context(), child, nil)
			case "unrelated root":
				owner, err = f.b.AddScope(t.Context(), 0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			slot, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: owner, Layout: f.integer})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "zero scope":
				child, slot = 0, outer
			case "forward scope":
				child, slot = ScopeID(math.MaxUint32), outer
			case "zero slot":
				slot = 0
			case "forward slot":
				slot = model.SlotID(math.MaxUint32)
			}
			requireInvalid(t, f.b.AddScopeCapture(t.Context(), child, slot))
			if graph, err := f.b.Build(t.Context(), 0); graph != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid capture did not poison builder: %v, %v", graph, err)
			}
		})
	}
}

func TestCaptureCannotAuthorizeRejectedScalar(t *testing.T) {
	f := newFixture(t)
	outer := f.slot(f.integer, false, 0, 0)
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: child, Layout: f.integer, Slot: outer}})
	requireInvalid(t, err)
	if captureErr := f.b.AddScopeCapture(t.Context(), child, outer); captureErr != err {
		t.Fatalf("capture replaced prior error: %v, want %v", captureErr, err)
	}
	if graph, buildErr := f.b.Build(t.Context(), 0); graph != nil || buildErr != err {
		t.Fatalf("failed scalar recovered: %v, %v", graph, buildErr)
	}
}

func TestIncrementalCaptureResourceFailures(t *testing.T) {
	for _, scenario := range []string{"operand quota", "byte quota", "cancelled", "copied builder"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			first, second := f.slot(f.integer, false, 0, 0), f.slot(f.integer, false, 0, 0)
			child, err := f.b.AddScope(t.Context(), f.scope, []model.SlotID{first})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			want := ErrLimit
			switch scenario {
			case "operand quota":
				f.b.limits.MaxOperands = 1
				if err := f.b.AddScopeCapture(ctx, child, first); err != nil {
					t.Fatalf("duplicate at quota: %v", err)
				}
			case "byte quota":
				f.b.limits.MaxBytes = f.b.bytes
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "copied builder":
				copied := *f.b
				f.b = &copied
				want = ErrInvalid
			}
			if err := f.b.AddScopeCapture(ctx, child, second); !errors.Is(err, want) {
				t.Fatalf("capture failure = %v, want %v", err, want)
			}
			if graph, err := f.b.Build(t.Context(), 0); graph != nil || !errors.Is(err, want) {
				t.Fatalf("failed capture builder = %v, %v", graph, err)
			}
		})
	}
}
