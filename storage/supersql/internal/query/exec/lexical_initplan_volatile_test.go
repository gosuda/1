package exec_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestLexicalInitplanVolatilityResetsOnlyWithCTEOwner(t *testing.T) {
	graph := lexicalVolatileGraph(t)
	env := lexicalInitplanEnvironment(t)
	calls := int64(0)
	env.Functions = aggregateProjectionResolver(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			return model.Int64Val(calls), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(1), model.Int64Val(1)},
		{model.Int64Val(1), model.Int64Val(2), model.Int64Val(1)},
		{model.Int64Val(2), model.Int64Val(1), model.Int64Val(2)},
		{model.Int64Val(2), model.Int64Val(2), model.Int64Val(2)},
	}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("lexical volatile rows=%v want=%v", got, want)
	}
	if calls != 2 {
		t.Fatalf("volatile evaluations=%d want one per each of two CTE owners", calls)
	}
	aggregateProjectionReleased(t, env.Query)
}

func lexicalVolatileGraph(t *testing.T) *fragment.FragmentGraph {
	t.Helper()
	base := sqlGraph(t, `SELECT x FROM (VALUES(1),(2)) a(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	outer, _ := graph.Rel(graph.Root())
	outerSlot, _ := graph.Output(graph.Root(), 0)
	info, _ := graph.Slot(outerSlot)
	b, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope := func(parent ir.ScopeID) ir.ScopeID {
		id, err := b.AddScope(t.Context(), parent, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	slot := func(scope ir.ScopeID, nullable bool) model.SlotID {
		id, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: info.Layout, Nullable: nullable})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	scalar := func(spec ir.ScalarSpec) ir.ScalarID {
		id, err := b.AddScalar(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	relation := func(spec ir.RelSpec) ir.RelID {
		id, err := b.AddRel(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	values := func(scope ir.ScopeID, output model.SlotID, data ...int64) ir.RelID {
		expressions := make([]ir.ScalarID, len(data))
		for i, value := range data {
			expressions[i] = scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: info.Layout}, Value: model.Int64Val(value)})
		}
		return relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: uint64(len(data))}, Outputs: []model.SlotID{output}, Expressions: expressions})
	}
	parentScope := scope(outer.Scope)
	cteSlot := slot(parentScope, false)
	body := values(parentScope, cteSlot, 0)
	cte, err := b.DeclareCTE(t.Context(), ir.CTESpec{Scope: parentScope, Outputs: []model.SlotID{cteSlot}, Materialization: ir.MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	k := slot(parentScope, false)
	parentInput := values(parentScope, k, 1, 2)
	innerScope := scope(parentScope)
	dummy := slot(innerScope, false)
	innerInput := values(innerScope, dummy, 0)
	scalarScope := scope(innerScope)
	inputSlot := slot(scalarScope, false)
	source := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: scalarScope, CTE: cte}, Outputs: []model.SlotID{inputSlot}})
	argument := scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scalarScope, Layout: info.Layout, Slot: inputSlot}})
	effects := ir.Effects{Volatility: ir.Volatile}
	signature, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900033, Result: info.Layout, Effects: effects}, Parameters: []ir.LayoutID{info.Layout}})
	if err != nil {
		t.Fatal(err)
	}
	call := scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: scalarScope, Layout: info.Layout, Function: 900033, Signature: signature, Effects: effects}, Operands: []ir.ScalarID{argument}})
	scalarSlot := slot(scalarScope, false)
	scalarBody := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scalarScope, Effects: effects}, Inputs: []ir.RelID{source}, Outputs: []model.SlotID{scalarSlot}, Bindings: []ir.Binding{{Slot: scalarSlot, Expr: call}}})
	result := slot(innerScope, true)
	effects.MayError = true
	innerApply := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: innerScope, ApplyKind: ir.ScalarApply, Effects: effects}, Inputs: []ir.RelID{innerInput, scalarBody}, Outputs: []model.SlotID{dummy, result}})
	resultExpression := scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: innerScope, Layout: info.Layout, Slot: result, Nullable: true}})
	inner := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: innerScope, Effects: effects}, Inputs: []ir.RelID{innerApply}, Outputs: []model.SlotID{result}, Bindings: []ir.Binding{{Slot: result, Expr: resultExpression}}})
	parentResult := slot(parentScope, true)
	parent := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: parentScope, ApplyKind: ir.LateralApply, Effects: effects}, Inputs: []ir.RelID{parentInput, inner}, Outputs: []model.SlotID{k, parentResult}})
	outerK, outerValue := slot(outer.Scope, false), slot(outer.Scope, true)
	root := relation(ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: outer.Scope, ApplyKind: ir.LateralApply, Effects: effects}, Inputs: []ir.RelID{graph.Root(), parent}, Outputs: []model.SlotID{outerSlot, outerK, outerValue}})
	graph, err = b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := opt.Optimize(t.Context(), graph, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), planned.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}
