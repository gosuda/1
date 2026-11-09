package normalize_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func findRelation(t *testing.T, graph *ir.Graph, kind ir.RelKind) (ir.RelID, ir.Rel) {
	t.Helper()
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		node, _ := graph.Rel(id)
		if node.Kind == kind {
			return id, node
		}
	}
	t.Fatalf("missing relation kind %d", kind)
	return 0, ir.Rel{}
}

func TestParameterizedApplyIsNotDecorrelated(t *testing.T) {
	graph := normalized(t, bound(t, `SELECT b.y FROM (VALUES(1)) a(x) LEFT JOIN LATERAL (SELECT a.x AS y LIMIT 1) b ON b.y>1`))
	id, apply := findRelation(t, graph, ir.Apply)
	if apply.ApplyKind != ir.LateralApply || !apply.LateralOuter {
		t.Fatalf("Apply semantics=%+v", apply)
	}
	right, _ := graph.Input(id, 1)
	node, _ := graph.Rel(right)
	if graph.CaptureCount(node.Scope) != 1 {
		t.Fatalf("parameterized scope lost: %+v", node)
	}
	_, limit := findRelation(t, graph, ir.Limit)
	count, err := graph.CloneConstant(t.Context(), limit.Count, 1024)
	if err != nil || count.IntVal != 1 {
		t.Fatalf("correlated LIMIT=%+v, %v", count, err)
	}
}

func TestAggregateAndWindowMetadataSurvive(t *testing.T) {
	f := newFixture(t)
	one := f.constant(f.integer, model.Int64Val(1))
	inputSlot := f.slot(f.integer, false)
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{inputSlot}, Expressions: []ir.ScalarID{one}})
	key := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: inputSlot}})
	truth := f.constant(f.boolean, model.BoolVal(true))
	group, sum := f.slot(f.integer, true), f.slot(f.integer, false)
	aggSignature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990002, Result: f.integer}, Parameters: []ir.LayoutID{f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	order := ir.OrderKey{Expr: key, Direction: ir.Descending, Nulls: ir.NullsFirst, Operator: 1}
	aggregate := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: f.scope}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{group, sum}, Expressions: []ir.ScalarID{key}, Bindings: []ir.Binding{{Slot: group, Expr: key}}, GroupingSets: [][]uint32{{0}, {}}, Aggregates: []ir.AggregateSpec{{AggregateCall: ir.AggregateCall{Signature: aggSignature, Function: 990002, Result: sum, Distinct: true, Filter: truth}, Args: []ir.ScalarID{key}, Order: []ir.OrderKey{order}}}})
	groupKey := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: group, Nullable: true}})
	windowResult := f.slot(f.integer, false)
	windowSignature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990003, Result: f.integer}, Parameters: []ir.LayoutID{f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	frame := ir.Frame{Mode: ir.GroupsFrame, Start: ir.FrameBound{Kind: ir.OffsetPreceding, Offset: one}, End: ir.FrameBound{Kind: ir.CurrentRow}, Exclusion: ir.ExcludeTies}
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Window, Scope: f.scope, Frame: frame, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{aggregate}, Outputs: []model.SlotID{group, sum, windowResult}, Partition: []ir.ScalarID{groupKey}, Order: []ir.OrderKey{{Expr: groupKey, Direction: ir.Descending, Nulls: ir.NullsFirst, Operator: 1}}, Windows: []ir.WindowSpec{{WindowCall: ir.WindowCall{Signature: windowSignature, Function: 990003, Result: windowResult}, Args: []ir.ScalarID{groupKey}}}})
	graph := normalized(t, f.freeze(root))
	aggID, _ := findRelation(t, graph, ir.Aggregate)
	call, _ := graph.Aggregate(aggID, 0)
	if !call.Distinct || call.Filter == 0 || call.ArgumentCount() != 1 || call.OrderCount() != 1 || call.Result != sum {
		t.Fatalf("aggregate=%+v", call)
	}
	if width, ok := graph.GroupingSetWidth(aggID, 1); !ok || width != 0 {
		t.Fatal("empty-input grand-total grouping set lost")
	}
	_, window := findRelation(t, graph, ir.Window)
	if window.Frame.Mode != ir.GroupsFrame || window.Frame.Exclusion != ir.ExcludeTies || window.PartitionCount() != 1 || !window.Effects.MayError {
		t.Fatalf("window=%+v", window)
	}
	offset, err := graph.CloneConstant(t.Context(), window.Frame.Start.Offset, 1024)
	if err != nil || offset.IntVal != 1 {
		t.Fatalf("frame offset=%+v, %v", offset, err)
	}
}

func TestRecursiveSearchCycleFeedbackSurvives(t *testing.T) {
	f := newFixture(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer}}
	pathLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 2287, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{record}})
	if err != nil {
		t.Fatal(err)
	}
	key, search, mark, path := f.slot(f.integer, false), f.slot(pathLayout, false), f.slot(f.boolean, false), f.slot(pathLayout, false)
	outputs := []model.SlotID{key, search, mark, path}
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: outputs, Recursive: true, Materialization: ir.MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	one := f.constant(f.integer, model.Int64Val(1))
	anchor := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{key}, Expressions: []ir.ScalarID{one}})
	consumer := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: outputs})
	keyExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: key}})
	member := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope}, Inputs: []ir.RelID{consumer}, Outputs: []model.SlotID{key}, Bindings: []ir.Binding{{Slot: key, Expr: keyExpr}}})
	truth, falsity := f.constant(f.boolean, model.BoolVal(true)), f.constant(f.boolean, model.BoolVal(false))
	union := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.RecursiveUnion, Scope: f.scope, CTE: cte, All: true}, Inputs: []ir.RelID{anchor, member}, Outputs: outputs, RecursionSpec: ir.RecursionSpec{Recursion: ir.Recursion{Search: ir.DepthFirst, SearchSlot: search, CycleMark: mark, CyclePath: path, CycleValue: truth, CycleDefault: falsity, CycleContinue: truth}, SearchKeys: []model.SlotID{key}, CycleKeys: []model.SlotID{key}}})
	if err := f.b.BindCTE(t.Context(), cte, union); err != nil {
		t.Fatal(err)
	}
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: outputs})
	graph := normalized(t, f.freeze(root))
	declaration, _ := graph.CTE(cte)
	body, _ := graph.Rel(declaration.Body)
	if body.Kind != ir.RecursiveUnion || !body.All || body.CTE != cte {
		t.Fatalf("recursion=%+v", body)
	}
	if body.Recursion.Search != ir.DepthFirst || body.Recursion.CycleMark != mark || body.Recursion.CyclePath != path || body.Recursion.CycleContinue == 0 {
		t.Fatalf("recursion=%+v", body)
	}
	searchKey, _ := graph.SearchKey(declaration.Body, 0)
	cycleKey, _ := graph.CycleKey(declaration.Body, 0)
	if searchKey != key || cycleKey != key {
		t.Fatalf("recursive keys=%d/%d", searchKey, cycleKey)
	}
	value, err := graph.CloneConstant(t.Context(), body.Recursion.CycleValue, 1024)
	if err != nil || value.IntVal != 1 {
		t.Fatalf("cycle value=%+v, %v", value, err)
	}
}

func TestPureFilterMergeAndIdentityProjectionPruning(t *testing.T) {
	f := newFixture(t)
	output := f.slot(f.integer, false)
	value := f.constant(f.integer, model.Int64Val(42))
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ir.ScalarID{value}})
	predicate := f.constant(f.boolean, model.BoolVal(true))
	inner := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: predicate}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{output}})
	outer := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: predicate}, Inputs: []ir.RelID{inner}, Outputs: []model.SlotID{output}})
	reference := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: output}})
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope}, Inputs: []ir.RelID{outer}, Outputs: []model.SlotID{output}, Bindings: []ir.Binding{{Slot: output, Expr: reference}}})
	graph := normalized(t, f.freeze(root))
	filter, _ := graph.Rel(graph.Root())
	if filter.Kind != ir.Filter {
		t.Fatalf("identity projection was retained: %+v", filter)
	}
	child, _ := graph.Input(graph.Root(), 0)
	base, _ := graph.Rel(child)
	if base.Kind != ir.Values {
		t.Fatalf("stacked filter was retained: %+v", base)
	}
	values, err := evaluate(t, graph, []ir.ScalarID{filter.Predicate}, nil, 1)
	if err != nil || values[0][0].IsNull || values[0][0].IntVal != 1 {
		t.Fatalf("merged predicate=%+v, %v", values, err)
	}
}

func TestNullableFilterDoesNotExposeOuterVolatilePredicate(t *testing.T) {
	f := newFixture(t)
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}})
	null := f.constant(f.boolean, model.NullVal(model.TypeBoolean))
	inner := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: null}, Inputs: []ir.RelID{input}})
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990005, Result: f.boolean, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Function: 990005, Signature: signature, Effects: effects}})
	root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: predicate, Effects: effects}, Inputs: []ir.RelID{inner}})
	graph := normalized(t, f.freeze(root))
	child, _ := graph.Input(graph.Root(), 0)
	filter, _ := graph.Rel(child)
	if filter.Kind != ir.Filter {
		t.Fatal("NULL filter mask widened over volatile/error predicate")
	}
	value, err := graph.CloneConstant(t.Context(), filter.Predicate, 1024)
	if err != nil || !value.IsNull {
		t.Fatalf("inner filter=%+v, %v", value, err)
	}
}

func TestSetDistinctSortAndLimitSemanticsSurvive(t *testing.T) {
	graph := normalized(t, bound(t, `SELECT DISTINCT ON (x) x FROM (SELECT 1 AS x UNION ALL SELECT 2 AS x) q ORDER BY x DESC NULLS FIRST LIMIT 1 OFFSET 0`))
	_, set := findRelation(t, graph, ir.SetOp)
	if set.SetKind != ir.Union || !set.All {
		t.Fatalf("set semantics=%+v", set)
	}
	distinctID, distinct := findRelation(t, graph, ir.Distinct)
	if !distinct.DistinctOn || distinct.ExpressionCount() != 1 {
		t.Fatalf("DISTINCT ON=%+v", distinct)
	}
	if distinct.OrderCount() != 0 {
		key, _ := graph.Order(distinctID, 0)
		if key.Direction != ir.Descending || key.Nulls != ir.NullsFirst {
			t.Fatalf("DISTINCT order=%+v", key)
		}
	}
	_, limit := findRelation(t, graph, ir.Limit)
	count, err := graph.CloneConstant(t.Context(), limit.Count, 1024)
	if err != nil || count.IntVal != 1 || !limit.Effects.MayError {
		t.Fatalf("LIMIT=%+v count=%+v, %v", limit, count, err)
	}
}
