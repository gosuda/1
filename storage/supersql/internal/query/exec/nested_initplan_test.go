package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestSQLNestedIndependentInitplans(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"rescanned parent", `SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (SELECT (SELECT 7) AS y WHERE a.x>0) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(7)}, {model.Int64Val(2), model.Int64Val(7)}, {model.Int64Val(3), model.Int64Val(7)}}},
		{"first parent empty", `SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (SELECT (SELECT 7) AS y WHERE a.x>1) b`, [][]model.Value{{model.Int64Val(2), model.Int64Val(7)}, {model.Int64Val(3), model.Int64Val(7)}}},
		{"all parents empty", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (SELECT 1/n FROM generate_series(0,0)t(n)) AS y WHERE a.x>10) b`, nil},
		{"empty scalar result", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (SELECT y FROM (VALUES(7)) r(y) WHERE FALSE) AS y WHERE a.x>0) b`, [][]model.Value{{model.Int64Val(1), model.NullVal(model.TypeInt64)}, {model.Int64Val(2), model.NullVal(model.TypeInt64)}}},
		{"two enclosing rescans", `SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (SELECT c.y FROM (VALUES(a.x)) v(z) CROSS JOIN LATERAL (SELECT (SELECT 7) AS y WHERE v.z>1) c) b`, [][]model.Value{{model.Int64Val(2), model.Int64Val(7)}, {model.Int64Val(3), model.Int64Val(7)}}},
		{"nested cached bodies", `SELECT (SELECT (SELECT 7)) FROM (VALUES(1),(2)) a(x)`, [][]model.Value{{model.Int64Val(7)}, {model.Int64Val(7)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v", got, scenario.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestNestedInitplanVolatilityIsQueryScoped(t *testing.T) {
	graph := nestedVolatileGraph(t)
	for _, base := range []int64{0, 10} {
		env := applyEnvironment(t)
		calls := 0
		env.Functions = aggregateProjectionResolver(func(expr.Function) (expr.ScalarCall, error) {
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				calls++
				return model.Int64Val(base + int64(calls)), nil
			}, nil
		})
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		if calls != 0 {
			t.Fatalf("opening parent evaluated scalar body %d times", calls)
		}
		want := [][]model.Value{{model.Int64Val(1), model.Int64Val(0), model.Int64Val(base + 1)}, {model.Int64Val(2), model.Int64Val(0), model.Int64Val(base + 1)}, {model.Int64Val(3), model.Int64Val(0), model.Int64Val(base + 1)}}
		if got := rows(t, portal); !reflect.DeepEqual(got, want) {
			t.Fatalf("query base=%v rows=%v want=%v", base, got, want)
		}
		if calls != 1 {
			t.Fatalf("query base=%v scalar evaluations=%d want=1", base, calls)
		}
		closePortal(t, portal)
		aggregateProjectionReleased(t, env.Query)
	}
}

func TestNestedInitplanErrorsReleaseGlobalCache(t *testing.T) {
	for _, scenario := range []struct{ name, body, code string }{
		{"cardinality", `SELECT y FROM (VALUES(7),(8)) r(y)`, "21000"},
		{"nested cache cardinality", `SELECT (SELECT y FROM (VALUES(7),(8)) r(y))`, "21000"},
		{"late first demand", `SELECT 1/n FROM generate_series(0,0)t(n)`, "22012"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT (`+scenario.body+`) AS y WHERE a.x>1) b`, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != scenario.code || batch.Rows() != 0 {
				t.Fatalf("nested error=%v rows=%d want=%s", err, batch.Rows(), scenario.code)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestNestedInitplanEarlyCloseAndCancellation(t *testing.T) {
	for _, cancelNext := range []bool{false, true} {
		name := "early close"
		if cancelNext {
			name = "cancel next demand"
		}
		t.Run(name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT a.x,b.y FROM (VALUES(1),(2),(3)) a(x) CROSS JOIN LATERAL (SELECT (SELECT 7) AS y WHERE a.x>0) b`, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 2 || batch.Columns[0].Int64[batch.Lane(0)] != 1 || batch.Columns[1].Int64[batch.Lane(0)] != 7 {
				t.Fatalf("first nested row=%+v", batch)
			}
			if cancelNext {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled demand=%v", err)
				}
			}
			closePortal(t, portal)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func nestedVolatileGraph(t *testing.T) *fragment.FragmentGraph {
	t.Helper()
	base := sqlGraph(t, `SELECT x FROM (VALUES(1),(2),(3)) a(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	outer, _ := graph.Rel(graph.Root())
	outerSlot, _ := graph.Output(graph.Root(), 0)
	info, _ := graph.Slot(outerSlot)
	builder, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	parentScope, err := builder.AddScope(t.Context(), outer.Scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentSlot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: parentScope, Layout: info.Layout})
	if err != nil {
		t.Fatal(err)
	}
	zero, err := builder.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: parentScope, Layout: info.Layout}, Value: model.Int64Val(0)})
	if err != nil {
		t.Fatal(err)
	}
	parentInput, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: parentScope, Rows: 1}, Outputs: []model.SlotID{parentSlot}, Expressions: []ir.ScalarID{zero}})
	if err != nil {
		t.Fatal(err)
	}
	scalarScope, err := builder.AddScope(t.Context(), parentScope, nil)
	if err != nil {
		t.Fatal(err)
	}
	effects := ir.Effects{Volatility: ir.Volatile}
	signature, err := builder.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900033, Result: info.Layout, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := builder.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: scalarScope, Layout: info.Layout, Function: 900033, Signature: signature, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	scalarSlot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: scalarScope, Layout: info.Layout})
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scalarScope, Rows: 1, Effects: effects}, Outputs: []model.SlotID{scalarSlot}, Expressions: []ir.ScalarID{value}})
	if err != nil {
		t.Fatal(err)
	}
	resultSlot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: parentScope, Layout: info.Layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	effects.MayError = true
	parent, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: parentScope, ApplyKind: ir.ScalarApply, Effects: effects}, Inputs: []ir.RelID{parentInput, scalar}, Outputs: []model.SlotID{parentSlot, resultSlot}})
	if err != nil {
		t.Fatal(err)
	}
	dummyOutput, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: outer.Scope, Layout: info.Layout})
	if err != nil {
		t.Fatal(err)
	}
	valueOutput, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: outer.Scope, Layout: info.Layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: outer.Scope, ApplyKind: ir.LateralApply, Effects: effects}, Inputs: []ir.RelID{graph.Root(), parent}, Outputs: []model.SlotID{outerSlot, dummyOutput, valueOutput}})
	if err != nil {
		t.Fatal(err)
	}
	graph, err = builder.Build(t.Context(), root)
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
