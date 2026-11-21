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

func TestSQLUncorrelatedScalarProjection(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"one row", `SELECT (SELECT 7) FROM (VALUES(1),(2),(3)) a(x)`, [][]model.Value{{model.Int64Val(7)}, {model.Int64Val(7)}, {model.Int64Val(7)}}},
		{"empty right", `SELECT (SELECT y FROM (VALUES(7)) r(y) WHERE FALSE) FROM (VALUES(1),(2)) a(x)`, [][]model.Value{{model.NullVal(model.TypeInt64)}, {model.NullVal(model.TypeInt64)}}},
		{"empty left suppresses error", `SELECT (SELECT 1/n FROM generate_series(0,0)t(n)) FROM (VALUES(1)) a(x) WHERE FALSE`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scalar rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLUncorrelatedScalarStillRejectsMultipleRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT (SELECT y FROM (VALUES(7),(8)) r(y)) FROM (VALUES(1),(2)) a(x)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "21000" {
		t.Fatalf("uncorrelated cardinality=%v", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("cardinality failure published output")
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestUncorrelatedScalarEvaluatesVolatileBodyOnce(t *testing.T) {
	base := sqlGraph(t, `SELECT x FROM (VALUES(1),(2),(3)) a(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	left, _ := graph.Rel(graph.Root())
	leftSlot, _ := graph.Output(graph.Root(), 0)
	info, _ := graph.Slot(leftSlot)
	builder, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := builder.AddScope(t.Context(), left.Scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	effects := ir.Effects{Volatility: ir.Volatile}
	signature, err := builder.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900033, Result: info.Layout, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := builder.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: scope, Layout: info.Layout, Function: 900033, Signature: signature, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	rightSlot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: info.Layout})
	if err != nil {
		t.Fatal(err)
	}
	right, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: 1, Effects: effects}, Outputs: []model.SlotID{rightSlot}, Expressions: []ir.ScalarID{value}})
	if err != nil {
		t.Fatal(err)
	}
	resultSlot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: left.Scope, Layout: info.Layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	effects.MayError = true
	root, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Apply, Scope: left.Scope, ApplyKind: ir.ScalarApply, Effects: effects}, Inputs: []ir.RelID{graph.Root(), right}, Outputs: []model.SlotID{leftSlot, resultSlot}})
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
	env := applyEnvironment(t)
	calls := int64(0)
	env.Functions = aggregateProjectionResolver(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			return model.Int64Val(calls), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), fragments, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(2), model.Int64Val(1)}, {model.Int64Val(3), model.Int64Val(1)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("volatile scalar rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}
