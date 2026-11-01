package ir

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRecursiveSearchCycleGeneratedColumns(t *testing.T) {
	for _, scenario := range []struct {
		name                                string
		invalidPath, nullMark, nullableMark bool
	}{{name: "typed key records"}, {name: "boolean path elements", invalidPath: true}, {name: "null in nonnullable cycle mark", nullMark: true}, {name: "nullable cycle mark", nullMark: true, nullableMark: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			integer := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
			element := model.TypeLayout{Type: model.Type{ID: 9001, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer}}
			if scenario.invalidPath {
				element = model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
			}
			pathType, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 9000, Storage: model.TypeArray}, Children: []model.TypeLayout{element}})
			if err != nil {
				t.Fatal(err)
			}
			key, search, mark, path := f.slot(f.integer, false, 0, 0), f.slot(pathType, false, 0, 0), f.slot(f.boolean, scenario.nullableMark, 0, 0), f.slot(pathType, false, 0, 0)
			outputs := []model.SlotID{key, search, mark, path}
			cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: outputs, Recursive: true})
			if err != nil {
				t.Fatal(err)
			}
			zero, yes := f.constant(0), f.truth()
			if scenario.nullMark {
				yes = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.boolean, Nullable: true}, Value: model.NullVal(model.TypeBoolean)})
			}
			no := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.boolean}, Value: model.BoolVal(false)})
			anchor := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{key}, Expressions: []ScalarID{zero}})
			ref := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: outputs})
			keyExpr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: key}})
			member := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{ref}, Outputs: []model.SlotID{key}, Bindings: []Binding{{Slot: key, Expr: keyExpr}}})
			union, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: RecursiveUnion, Scope: f.scope, CTE: cte, All: true}, Inputs: []RelID{anchor, member}, Outputs: outputs, RecursionSpec: RecursionSpec{Recursion: Recursion{Search: DepthFirst, SearchSlot: search, CycleMark: mark, CyclePath: path, CycleValue: yes, CycleDefault: no, CycleContinue: yes}, SearchKeys: []model.SlotID{key}, CycleKeys: []model.SlotID{key}}})
			if scenario.invalidPath || scenario.nullMark && !scenario.nullableMark {
				requireInvalid(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.b.BindCTE(t.Context(), cte, union); err != nil {
				t.Fatal(err)
			}
			g := f.freeze(union)
			n, _ := g.Rel(union)
			if n.Recursion.CyclePath != path || n.Recursion.Search != DepthFirst {
				t.Fatalf("recursive metadata = %+v", n.Recursion)
			}
			if got, ok := g.CycleKey(union, 0); !ok || got != key {
				t.Fatalf("cycle key = %v, %v", got, ok)
			}
		})
	}
}
func TestScalarSubqueryCarriesCardinalityError(t *testing.T) {
	f := newFixture(t)
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	childSlot, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: child, Layout: f.integer})
	if err != nil {
		t.Fatal(err)
	}
	one := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: child, Layout: f.integer}, Value: model.Int64Val(1)})
	relation := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: child, Rows: 1}, Outputs: []model.SlotID{childSlot}, Expressions: []ScalarID{one}})
	expr := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: relation}})
	out := f.slot(f.integer, true, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{expr}})
	g := f.freeze(root)
	n, _ := g.Scalar(expr)
	if !n.Nullable || !n.Effects.MayError || n.Relation != relation {
		t.Fatalf("scalar subquery contract = %+v", n)
	}
}
func TestRejectsSourceSpanOutsideInput(t *testing.T) {
	f := newFixture(t)
	_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Span: Span{Start: 4000, End: 5000}}, Value: model.Int64Val(1)})
	requireInvalid(t, err)
}
func TestRejectsMalformedNestedConstant(t *testing.T) {
	f := newFixture(t)
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1000, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: layout}, Value: model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}}})
	requireInvalid(t, err)
}
func TestConstantCloneHasIndependentBudget(t *testing.T) {
	f := newFixture(t)
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}})
	if err != nil {
		t.Fatal(err)
	}
	constant := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: layout}, Value: model.BytesVal([]byte{1, 2, 3})})
	out := f.slot(layout, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{constant}})
	g := f.freeze(root)
	value, err := g.CloneConstant(t.Context(), constant, 2)
	if !errors.Is(err, ErrLimit) || value.BytesVal != nil {
		t.Fatalf("undersized clone = %+v, %v", value, err)
	}
}
func TestRejectsZeroLimits(t *testing.T) {
	if b, err := NewBuilder(Limits{}); b != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero limits = %v, %v", b, err)
	}
}

func TestRejectsCTECycleThroughAggregateArgument(t *testing.T) {
	f := newFixture(t)
	out := f.slot(f.integer, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	childOut, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: child, Layout: f.integer})
	if err != nil {
		t.Fatal(err)
	}
	consumer := f.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: child, CTE: cte}, Outputs: []model.SlotID{childOut}})
	exists := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.boolean, SubqueryKind: ExistsSubquery, Relation: consumer}})
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 12, Result: f.integer}, Parameters: []LayoutID{f.boolean}})
	if err != nil {
		t.Fatal(err)
	}
	input := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
	body := f.rel(RelSpec{Rel: Rel{Kind: Aggregate, Scope: f.scope}, Inputs: []RelID{input}, Outputs: []model.SlotID{out}, GroupingSets: [][]uint32{{}}, Aggregates: []AggregateSpec{{AggregateCall: AggregateCall{Signature: signature, Function: 12, Result: out}, Args: []ScalarID{exists}}}})
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), body)
	if graph != nil {
		t.Fatal("CTE cycle hidden in aggregate argument was frozen")
	}
	requireInvalid(t, err)
}

func TestSharedScalarDAGHasBoundedDependencyVerification(t *testing.T) {
	f := newFixture(t)
	expr := f.truth()
	for range 40 {
		expr = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Bool, Scope: f.scope, Layout: f.boolean, BoolOp: And}, Operands: []ScalarID{expr, expr}})
	}
	out := f.slot(f.boolean, false, 0, 0)
	_, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{expr}})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("exponential dependency traversal = %v, want bounded resource error", err)
	}
}

func fieldLayout(t *testing.T, f fixture) LayoutID {
	t.Helper()
	id, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 9000, Storage: model.TypeComposite}, Children: []model.TypeLayout{
		{Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		{Type: model.Type{ID: 16, Storage: model.TypeBoolean}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFieldPreservesParentEffectsAndRebuilds(t *testing.T) {
	f := newFixture(t)
	layout := fieldLayout(t, f)
	effects := Effects{Volatility: Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 99, Result: f.integer, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Signature: signature, Function: 99, Effects: effects}})
	yes := f.truth()
	row := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout, Effects: effects}, Operands: []ScalarID{call, yes}})
	operands := []ScalarID{row}
	field := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.boolean, FieldIndex: 1, Effects: effects}, Operands: operands})
	operands[0] = yes
	out := f.slot(f.boolean, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1, Effects: effects}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{field}})
	g := f.freeze(root)
	n, _ := g.Scalar(field)
	if n.FieldIndex != 1 || n.Nullable || n.Effects != effects {
		t.Fatalf("field metadata = %+v", n)
	}
	if got, ok := g.Operand(field, 0); !ok || got != row {
		t.Fatalf("field lost original row operand: %d, %v", got, ok)
	}
	rebuilt := newFixture(t)
	rebuiltLayout := fieldLayout(t, rebuilt)
	one, no := rebuilt.constant(1), rebuilt.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: rebuilt.scope, Layout: rebuilt.boolean}, Value: model.BoolVal(false)})
	rebuiltRow := rebuilt.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: rebuilt.scope, Layout: rebuiltLayout}, Operands: []ScalarID{one, no}})
	metadata := n.Metadata()
	metadata.Scope, metadata.Layout, metadata.Effects = rebuilt.scope, rebuilt.boolean, Effects{}
	rebuiltField := rebuilt.scalar(ScalarSpec{Scalar: metadata, Operands: []ScalarID{rebuiltRow}})
	rebuiltOut := rebuilt.slot(rebuilt.boolean, false, 0, 0)
	rebuiltRoot := rebuilt.rel(RelSpec{Rel: Rel{Kind: Values, Scope: rebuilt.scope, Rows: 1}, Outputs: []model.SlotID{rebuiltOut}, Expressions: []ScalarID{rebuiltField}})
	rebuiltGraph := rebuilt.freeze(rebuiltRoot)
	rebuiltNode, _ := rebuiltGraph.Scalar(rebuiltField)
	if rebuiltNode.Kind != Field || rebuiltNode.FieldIndex != 1 {
		t.Fatalf("rebuilt field = %+v", rebuiltNode)
	}
	if got, ok := rebuiltGraph.Operand(rebuiltField, 0); !ok || got != rebuiltRow {
		t.Fatalf("rebuilt field operand = %d, %v", got, ok)
	}
	n.FieldIndex = 0
	original, _ := g.Scalar(field)
	if original.FieldIndex != 1 {
		t.Fatal("scalar accessor exposed mutable metadata")
	}
}

func TestFieldNullability(t *testing.T) {
	for _, scenario := range []string{"first field", "nullable row child", "nonnull row slot", "nullable row slot"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			layout := fieldLayout(t, f)
			var parent ScalarID
			var input RelID
			nullable := scenario != "first field"
			if scenario == "nonnull row slot" || scenario == "nullable row slot" {
				parentNullable := scenario == "nullable row slot"
				slot := f.slot(layout, parentNullable, 10, 1)
				input = f.scan(10, slot)
				parent = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: layout, Slot: slot, Nullable: parentNullable}})
			} else {
				child := f.constant(5)
				if nullable {
					child = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Nullable: true}, Value: model.NullVal(model.TypeInt64)})
				}
				yes := f.truth()
				parent = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout}, Operands: []ScalarID{child, yes}})
				input = f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
			}
			field := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.integer, Nullable: nullable}, Operands: []ScalarID{parent}})
			out := f.slot(f.integer, nullable, 0, 0)
			root := f.rel(RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{input}, Outputs: []model.SlotID{out}, Bindings: []Binding{{Slot: out, Expr: field}}})
			g := f.freeze(root)
			n, _ := g.Scalar(field)
			if n.FieldIndex != 0 || n.Nullable != nullable {
				t.Fatalf("first field = %+v", n)
			}
		})
	}
}

func TestRejectsInvalidField(t *testing.T) {
	for _, scenario := range []string{"index outside layout", "wrong result layout", "noncomposite parent", "missing parent", "two parents", "stray index"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			layout := fieldLayout(t, f)
			child, yes := f.constant(5), f.truth()
			parent := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout}, Operands: []ScalarID{child, yes}})
			spec := ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{parent}}
			switch scenario {
			case "index outside layout":
				spec.FieldIndex = 2
			case "wrong result layout":
				spec.Layout = f.boolean
			case "noncomposite parent":
				spec.Operands[0] = child
			case "missing parent":
				spec.Operands = nil
			case "two parents":
				spec.Operands = []ScalarID{parent, parent}
			case "stray index":
				spec = ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, FieldIndex: 1}, Value: model.Int64Val(5)}
			}
			_, err := f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestRejectsFieldLosingNullability(t *testing.T) {
	for _, scenario := range []string{"nullable child", "nullable parent", "unknown slot child"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			layout := fieldLayout(t, f)
			var parent ScalarID
			if scenario == "nullable child" {
				child := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Nullable: true}, Value: model.NullVal(model.TypeInt64)})
				parent = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout}, Operands: []ScalarID{child, f.truth()}})
			} else {
				nullable := scenario == "nullable parent"
				slot := f.slot(layout, nullable, 0, 0)
				parent = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: layout, Slot: slot, Nullable: nullable}})
			}
			_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{parent}})
			requireInvalid(t, err)
		})
	}
}

func TestRejectsFieldLosingParentEffects(t *testing.T) {
	f := newFixture(t)
	layout := fieldLayout(t, f)
	effects := Effects{Volatility: Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 99, Result: f.boolean, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Function: 99, Effects: effects}})
	parent := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout, Effects: effects}, Operands: []ScalarID{f.constant(5), call}})
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{parent}})
	requireInvalid(t, err)
}

func TestRejectsCTECycleThroughFieldParent(t *testing.T) {
	f := newFixture(t)
	layout := fieldLayout(t, f)
	out := f.slot(f.boolean, false, 0, 0)
	cte, err := f.b.DeclareCTE(t.Context(), CTESpec{Scope: f.scope, Outputs: []model.SlotID{out}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := fixture{t: t, b: f.b, scope: child, integer: f.integer, boolean: f.boolean}
	childOut := inner.slot(f.boolean, false, 0, 0)
	consumer := inner.rel(RelSpec{Rel: Rel{Kind: CTEConsumer, Scope: child, CTE: cte}, Outputs: []model.SlotID{childOut}})
	exists := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.boolean, SubqueryKind: ExistsSubquery, Relation: consumer}})
	one := f.constant(1)
	row := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Row, Scope: f.scope, Layout: layout}, Operands: []ScalarID{one, exists}})
	field := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Field, Scope: f.scope, Layout: f.boolean, FieldIndex: 1}, Operands: []ScalarID{row}})
	body := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{field}})
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	graph, err := f.b.Build(t.Context(), body)
	if graph != nil {
		t.Fatal("CTE cycle beneath Field escaped verification")
	}
	requireInvalid(t, err)
}

func TestRejectsInvalidCaptureScopesAtBuild(t *testing.T) {
	for _, scenario := range []string{"scope cycle", "invalid captured owner"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			outer := f.slot(f.integer, false, 0, 0)
			child, err := f.b.AddScope(t.Context(), f.scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.b.AddScopeCapture(t.Context(), child, outer); err != nil {
				t.Fatal(err)
			}
			root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
			if scenario == "scope cycle" {
				f.b.graph.scopes[child-1].parent = child
			} else {
				f.b.graph.slots[outer-1].Scope = child
			}
			graph, err := f.b.Build(t.Context(), root)
			if graph != nil {
				t.Fatal("invalid capture scope escaped verification")
			}
			requireInvalid(t, err)
		})
	}
}

func TestPersistedKindNumbersRemainAppendOnly(t *testing.T) {
	if SQLJSON != ScalarKind(25) || ArrayQuantified != ScalarKind(26) || FieldStore != ScalarKind(27) || ArrayStore != ScalarKind(28) || JSONBStore != ScalarKind(29) {
		t.Fatalf("scalar kind numbering changed: SQLJSON=%d ArrayQuantified=%d FieldStore=%d ArrayStore=%d JSONBStore=%d", SQLJSON, ArrayQuantified, FieldStore, ArrayStore, JSONBStore)
	}
	if ArraySubquery != SubqueryKind(6) || TupleSubquery != SubqueryKind(7) {
		t.Fatalf("subquery kind numbering changed: ArraySubquery=%d TupleSubquery=%d", ArraySubquery, TupleSubquery)
	}
	if CatalogScan != RelKind(23) || Sample != RelKind(24) {
		t.Fatalf("relation kind numbering changed: CatalogScan=%d Sample=%d", CatalogScan, Sample)
	}
}
