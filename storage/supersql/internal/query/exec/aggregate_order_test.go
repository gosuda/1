package exec_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func TestSQLAggregateOrderTransitions(t *testing.T) {
	i, f := model.Int64Val, model.Float64Val
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{
			name: "opposite orders in one aggregate",
			sql: `SELECT sum(x ORDER BY k),sum(x ORDER BY k DESC)
				FROM (VALUES(1e16::float8,1),(-1e16::float8,2),(1::float8,3)) v(x,k)`,
			want: [][]model.Value{{f(1), f(0)}},
		},
		{
			name: "independent multicolumn and null orders",
			sql: `SELECT sum(x ORDER BY a ASC NULLS LAST,b DESC),
				sum(x ORDER BY a ASC NULLS FIRST,b DESC),
				sum(x ORDER BY k DESC),sum(x ORDER BY a,b DESC),
				sum(x ORDER BY a DESC,b DESC)
				FROM (VALUES(1e16::float8,0,2,1),(-1e16::float8,0,1,2),(1::float8,NULL,0,3)) v(x,a,b,k)`,
			want: [][]model.Value{{f(1), f(0), f(0), f(1), f(0)}},
		},
		{
			name: "distinct computed ordered arguments",
			sql: `SELECT sum(DISTINCT x+1 ORDER BY x+1 DESC),count(DISTINCT x ORDER BY x)
				FROM (VALUES(2),(1),(2),(3),(NULL::integer),(NULL)) v(x)`,
			want: [][]model.Value{{i(9), i(3)}},
		},
		{
			name: "filter guards argument and order division",
			sql: `SELECT sum(10/x ORDER BY 100/x DESC) FILTER(WHERE x<>0),
				count(1 ORDER BY 100/x) FILTER(WHERE x<>0),
				sum(10/x ORDER BY 100/x) FILTER(WHERE NULL::boolean),
				count(1 ORDER BY 100/x) FILTER(WHERE false)
				FROM (VALUES(0),(2),(NULL::integer),(5)) v(x)`,
			want: [][]model.Value{{i(7), i(2), model.NullVal(model.TypeInt64), i(0)}},
		},
		{
			name: "empty ordered aggregate retains identity",
			sql: `SELECT sum(x ORDER BY k),count(1 ORDER BY k)
				FROM (VALUES(1::float8,1)) v(x,k) WHERE false`,
			want: [][]model.Value{{model.NullVal(model.TypeFloat64), i(0)}},
		},
		{
			name: "ordered executable families preserve null semantics",
			sql: `SELECT count(1 ORDER BY k DESC),count(x ORDER BY k),min(x ORDER BY k),
				max(x ORDER BY k DESC),avg(x ORDER BY k)=3::numeric,
				bit_and(x ORDER BY k),bit_or(x ORDER BY k DESC),bit_xor(x ORDER BY k),
				bool_and(x>0 ORDER BY k),bool_or(x>3 ORDER BY k DESC),every(x>0 ORDER BY k)
				FROM (VALUES(2,1),(2,2),(5,3),(NULL::integer,4)) v(x,k)`,
			want: [][]model.Value{{i(4), i(3), i(2), i(5), model.BoolVal(true), i(0), i(7), i(5), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows=%v, want %v", got, tc.want)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAggregateOrderGroupPartitions(t *testing.T) {
	var values []string
	for group := range 9 {
		values = append(values, fmt.Sprintf("(%d,1e16::float8,1),(%d,-1e16::float8,2),(%d,1::float8,3)", group, group, group))
	}
	graph := sqlGraph(t, `SELECT g,sum(x ORDER BY k),sum(x ORDER BY k DESC)
		FROM (VALUES`+strings.Join(values, ",")+`) v(g,x,k) GROUP BY g ORDER BY g`, nil)
	i, f := model.Int64Val, model.Float64Val
	want := [][]model.Value{
		{i(0), f(1), f(0)}, {i(1), f(1), f(0)}, {i(2), f(1), f(0)},
		{i(3), f(1), f(0)}, {i(4), f(1), f(0)}, {i(5), f(1), f(0)},
		{i(6), f(1), f(0)}, {i(7), f(1), f(0)}, {i(8), f(1), f(0)},
	}
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 64 << 10
			if spill {
				env.Aggregate.GroupBytes = 32 << 10
				env.Aggregate.PartitionMemoryBytes = 1024
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows=%v, want %v", got, want)
			}
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAggregateOrderGroupingSetsReplay(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,sum(x ORDER BY g,k),sum(x ORDER BY g DESC,k DESC),GROUPING(g)
		FROM (VALUES(1,1e16::float8,1),(1,-1e16::float8,2),(1,1::float8,3),
			(2,1e16::float8,1),(2,-1e16::float8,2),(2,1::float8,3)) v(g,x,k)
		GROUP BY GROUPING SETS ((g),(),(g),())`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Aggregate.PartitionMemoryBytes = 1024
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, f := model.Int64Val, model.Float64Val
	null := model.NullVal(model.TypeInt64)
	want := [][]model.Value{
		{i(1), f(1), f(0), i(0)}, {i(2), f(1), f(0), i(0)}, {null, f(1), f(0), i(1)},
		{i(1), f(1), f(0), i(0)}, {i(2), f(1), f(0), i(0)}, {null, f(1), f(0), i(1)},
	}
	assertGroupingSetsBag(t, groupingFunctionRows(t, portal, []int{3}), want)
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("ordered grouping-set input did not spill")
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLAggregateOrderActiveSpillCleanup(t *testing.T) {
	for _, ending := range []string{"early close", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT sum(x ORDER BY k),sum(x ORDER BY k DESC)
				FROM (VALUES(1e16::float8,1),(-1e16::float8,2),(1::float8,3)) v(x,k)
				GROUP BY GROUPING SETS ((),(),())`, nil)
			env, dir := groupingSetsEnvironment(t, true)
			env.Aggregate.PartitionMemoryBytes = 1024
			env.Limits.BatchRows = 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			portal, err := exec.Open(ctx, graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			if err := portal.Next(ctx, &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 2 {
				t.Fatalf("first batch has %d rows and %d columns, want 1 and 2", batch.Rows(), len(batch.Columns))
			}
			for index, want := range []model.Value{model.Float64Val(1), model.Float64Val(0)} {
				got, _, err := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(0), 4096)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("first result column %d=%v, error=%v, want %v", index, got, err, want)
				}
			}
			if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
				t.Fatalf("spill was not active before %s: %+v", ending, usage)
			}
			if ending == "cancel" {
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("Next error=%v, want context cancellation", err)
				}
			} else {
				closePortal(t, portal)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestAggregateOrderFreezesVolatileProjectionAcrossGroupingSets(t *testing.T) {
	graph := aggregateOrderVolatileGraph(t)
	env, dir := groupingSetsEnvironment(t, true)
	env.Aggregate.PartitionMemoryBytes = 1024
	var filters, arguments, orders int64
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			x := args[0].IntVal
			switch fn.ID {
			case 900001:
				filters++
				return model.BoolVal(x != 0), nil
			case 900002:
				if x == 0 {
					t.Fatal("false FILTER evaluated its argument")
				}
				arguments++
				return model.Int64Val(arguments), nil
			case 900003:
				if x == 0 {
					t.Fatal("false FILTER evaluated its order expression")
				}
				orders++
				return model.Int64Val(orders), nil
			default:
				t.Fatalf("unexpected function %d", fn.ID)
				return model.Value{}, nil
			}
		}, nil
	})
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	want := [][]model.Value{{null, i(3)}, {i(0), null}, {i(1), i(1)}, {i(2), i(2)}, {null, i(3)}}
	assertGroupingSetsBag(t, rows(t, portal), want)
	if filters != 3 || arguments != 2 || orders != 2 {
		t.Fatalf("evaluations: FILTER=%d argument=%d order=%d, want 3, 2, 2 across all sets", filters, arguments, orders)
	}
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("volatile ordered projection did not replay from spill")
	}
	groupingSetsReleased(t, env, dir)
}

func aggregateOrderVolatileGraph(t *testing.T) *fragment.FragmentGraph {
	t.Helper()
	base := sqlGraph(t, `SELECT x FROM (VALUES(0),(1),(2)) v(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	input, _ := graph.Rel(graph.Root())
	x, _ := graph.Output(graph.Root(), 0)
	info, _ := graph.Slot(x)
	b, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	integer, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	column, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: input.Scope, Slot: x, Layout: info.Layout}})
	if err != nil {
		t.Fatal(err)
	}
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	call := func(function model.FunctionID, result ir.LayoutID) ir.ScalarID {
		sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: result, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{info.Layout}})
		if err != nil {
			t.Fatal(err)
		}
		id, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: input.Scope, Function: function, Signature: sig, Layout: result, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{column}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	filter, argument, order := call(900001, boolean), call(900002, info.Layout), call(900003, info.Layout)
	group, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: input.Scope, Layout: info.Layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: input.Scope, Layout: integer, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 2108, Result: integer, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{info.Layout}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel: ir.Rel{Kind: ir.Aggregate, Scope: input.Scope, Effects: effects}, Inputs: []ir.RelID{graph.Root()},
		Outputs: []model.SlotID{group, result}, Expressions: []ir.ScalarID{column},
		Bindings: []ir.Binding{{Slot: group, Expr: column}}, GroupingSets: [][]uint32{{}, {0}, {}},
		Aggregates: []ir.AggregateSpec{{
			AggregateCall: ir.AggregateCall{Function: 2108, Signature: sig, Result: result, Filter: filter, Effects: effects},
			Args:          []ir.ScalarID{argument}, Order: []ir.OrderKey{
				{Expr: order, Direction: ir.Descending, Nulls: ir.NullsLast, Operator: 521},
				{Expr: argument, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 97},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
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
