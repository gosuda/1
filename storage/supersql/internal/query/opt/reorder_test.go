package opt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func largeJoinGraph(t *testing.T, count int, withPredicates bool) (*ir.Graph, []model.SlotID, []ir.ScalarID) {
	t.Helper()
	f := newFixture(t)
	leaves := make([]ir.RelID, count)
	slots := make([]model.SlotID, count)
	for i := range leaves {
		rows := uint64(1)
		if i < 2 {
			rows = 20
		}
		leaves[i], slots[i] = f.values(rows)
	}
	var predicates []ir.ScalarID
	if withPredicates {
		last := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: slots[count-1]}})
		predicates = append(predicates, f.comparison(slots[0], last, 412))
		predicates = append(predicates, f.comparison(slots[count-2], last, 412))
		one := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
		predicates = append(predicates, f.comparison(slots[1], one, 413))
	}
	var build func(int, int) ir.RelID
	build = func(lo, hi int) ir.RelID {
		if hi-lo == 1 {
			return leaves[lo]
		}
		mid := lo + (hi-lo)/2
		left, right := build(lo, mid), build(mid, hi)
		kind, predicate := ir.CrossJoin, ir.ScalarID(0)
		if lo == 0 && hi == count && len(predicates) != 0 {
			kind = ir.InnerJoin
			predicate = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, BoolOp: ir.And}, Operands: predicates})
		}
		return f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: kind, Predicate: predicate}, Inputs: []ir.RelID{left, right}, Outputs: slots[lo:hi]})
	}
	return f.build(build(0, count)), slots, predicates
}

func largeJoinOptions() Options {
	options := testOptions()
	options.IR.MaxDepth = 256
	options.Plan.MaxDepth = 256
	options.Plan.MaxOperands = 65536
	options.Plan.MaxProperties = 65536
	return options
}

func joinLeafOrder(graph *ir.Graph) []ir.RelID {
	var order []ir.RelID
	var visit func(ir.RelID)
	visit = func(id ir.RelID) {
		r, _ := graph.Rel(id)
		if r.InputCount() == 0 {
			order = append(order, id)
		}
		for i := range r.InputCount() {
			child, _ := graph.Input(id, i)
			visit(child)
		}
	}
	visit(graph.Root())
	return order
}

func requireJoinOutputs(t *testing.T, plan *physical.Plan, slots []model.SlotID) {
	t.Helper()
	root, ok := plan.Node(plan.Root())
	if !ok || root.OutputCount() != uint32(len(slots)) {
		t.Fatalf("incomplete root output: want %d slots", len(slots))
	}
	for i, want := range slots {
		got, ok := root.Output(uint32(i))
		if !ok || got.ID != want {
			t.Fatalf("output %d = %+v, want slot %d", i, got, want)
		}
	}
}

func TestGreedyJoinBeyondSubsetAndWordBoundaries(t *testing.T) {
	for _, count := range []int{9, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			graph, slots, _ := largeJoinGraph(t, count, false)
			options := largeJoinOptions()
			original, err := optimizeOriginal(t.Context(), graph, options)
			if err != nil {
				t.Fatal(err)
			}
			first, err := Optimize(t.Context(), graph, options)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Optimize(t.Context(), graph, options)
			if err != nil {
				t.Fatal(err)
			}
			if rootMetadata(t, first.Plan).Cost.Total >= rootMetadata(t, original.Plan).Cost.Total {
				t.Fatal("greedy alternative did not improve the estimated cost")
			}
			a, _ := first.Plan.Graph(1)
			b, _ := second.Plan.Graph(1)
			order := joinLeafOrder(a)
			if slices.Equal(order, joinLeafOrder(graph)) || !slices.Equal(order, joinLeafOrder(b)) {
				t.Fatal("expected a deterministic alternative to original leaf order")
			}
			wantLeaves := joinLeafOrder(graph)
			slices.Sort(order)
			slices.Sort(wantLeaves)
			if !slices.Equal(order, wantLeaves) {
				t.Fatalf("leaf membership changed: got %v want %v", order, wantLeaves)
			}
			requireJoinOutputs(t, first.Plan, slots)
			if rootMetadata(t, first.Plan).Cost != rootMetadata(t, second.Plan).Cost || first.MemoExpressions != second.MemoExpressions {
				t.Fatal("repeated optimization changed the deterministic winner")
			}
		})
	}
}

func TestGreedyJoinPreservesPredicatesAcrossWordBoundary(t *testing.T) {
	graph, slots, predicates := largeJoinGraph(t, 65, true)
	result, err := Optimize(t.Context(), graph, largeJoinOptions())
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, _ := result.Plan.Graph(1)
	if rebuilt == graph {
		t.Fatal("expected the greedy predicate-bearing alternative")
	}
	counts := make(map[ir.ScalarID]int)
	var scalar func(ir.ScalarID)
	scalar = func(id ir.ScalarID) {
		if slices.Contains(predicates, id) {
			counts[id]++
			return
		}
		s, _ := rebuilt.Scalar(id)
		for i := range s.OperandCount() {
			child, _ := rebuilt.Operand(id, i)
			scalar(child)
		}
	}
	var visit func(ir.RelID)
	visit = func(id ir.RelID) {
		r, _ := rebuilt.Rel(id)
		if r.Predicate != 0 {
			scalar(r.Predicate)
		}
		for i := range r.InputCount() {
			child, _ := rebuilt.Input(id, i)
			visit(child)
		}
	}
	visit(rebuilt.Root())
	for _, predicate := range predicates {
		if counts[predicate] != 1 {
			t.Errorf("predicate %d occurs %d times, want once", predicate, counts[predicate])
		}
	}
	requireJoinOutputs(t, result.Plan, slots)
}

func TestLargeJoinSoftBudgetsKeepCompleteOriginal(t *testing.T) {
	graph, slots, _ := largeJoinGraph(t, 65, true)
	for _, budget := range []string{"expressions", "memory", "time"} {
		t.Run(budget, func(t *testing.T) {
			options := largeJoinOptions()
			switch budget {
			case "expressions":
				options.MemoExpressions = 1
			case "memory":
				options.MemoBytes = 1
			case "time":
				options.SearchTime = time.Nanosecond
			}
			result, err := Optimize(t.Context(), graph, options)
			if err != nil {
				t.Fatal(err)
			}
			registered, _ := result.Plan.Graph(1)
			if !result.BudgetExhausted || !slices.Equal(joinLeafOrder(registered), joinLeafOrder(graph)) {
				t.Fatal("soft exhaustion lost the complete original-order fallback")
			}
			requireJoinOutputs(t, result.Plan, slots)
		})
	}
}

func TestLargeJoinCancellationReturnsNoPlan(t *testing.T) {
	graph, _, _ := largeJoinGraph(t, 65, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := Optimize(ctx, graph, largeJoinOptions())
	if !errors.Is(err, context.Canceled) || result.Plan != nil {
		t.Fatalf("canceled optimization returned plan %v, error %v", result.Plan, err)
	}
}

func TestGreedyJoinSoftExhaustionDoesNotPublishPrefix(t *testing.T) {
	graph, slots, _ := largeJoinGraph(t, 9, true)
	options := largeJoinOptions()
	options.MemoExpressions = 1
	if err := validateOptions(&options); err != nil {
		t.Fatal(err)
	}
	o := optimizer{ctx: t.Context(), graph: graph, options: options}
	o.groups = allocate[group](&o, uint64(graph.RelCount())+1)
	o.access = allocate[scanAccess](&o, uint64(graph.RelCount())+1)
	o.predicates = allocate[ir.ScalarID](&o, uint64(graph.RelCount())+1)
	o.producers = allocate[*expression](&o, uint64(graph.CTECount())+1)
	o.inventory()
	seed := o.seed(graph.Root(), 0)
	if o.err != nil {
		t.Fatal(o.err)
	}
	complete, err := o.plan(seed)
	if err != nil {
		t.Fatal(err)
	}
	o.searchStart = time.Now()
	result, err := o.reorder(complete)
	if err != nil {
		t.Fatal(err)
	}
	if !o.exhausted || result != complete {
		t.Fatal("an exhausted greedy prefix replaced the validated complete plan")
	}
	requireJoinOutputs(t, result, slots)
}

func TestLargeJoinKeepsOriginalWhenGreedyDepthCannotFit(t *testing.T) {
	graph, slots, _ := largeJoinGraph(t, 65, false)
	options := largeJoinOptions()
	options.IR.MaxDepth = 64
	result, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := result.Plan.Graph(1)
	if !result.BudgetExhausted || !slices.Equal(joinLeafOrder(registered), joinLeafOrder(graph)) {
		t.Fatal("a valid bushy original was lost to an infeasible left-deep alternative")
	}
	requireJoinOutputs(t, result.Plan, slots)
}
