package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestSQLComputedAggregateProjection(t *testing.T) {
	for _, sample := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"argument", "SELECT sum(x+1) FROM (VALUES(1),(2),(NULL)) v(x)", [][]model.Value{{model.Int64Val(5)}}},
		{"filter suppresses division", "SELECT sum(10/x) FILTER(WHERE x<>0) FROM (VALUES(0),(2),(NULL),(5)) v(x)", [][]model.Value{{model.Int64Val(7)}}},
		{"null filter", "SELECT sum(10/x) FILTER(WHERE NULL::boolean),count(*) FILTER(WHERE NULL::boolean) FROM (VALUES(0),(0)) v(x)", [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(0)}}},
		{"numeric argument", "SELECT sum(x+0.25::numeric)=3.75::numeric FROM (VALUES(1::numeric),(2::numeric),(0::numeric)) v(x)", [][]model.Value{{model.BoolVal(true)}}},
		{"wide integer argument", "SELECT sum(x+1::bigint)=18446744073709551614::numeric FROM (VALUES(9223372036854775806::bigint),(9223372036854775806::bigint)) v(x)", [][]model.Value{{model.BoolVal(true)}}},
		{"grouping survives false filter", "SELECT x+1,sum(10/x) FILTER(WHERE x<>0) FROM (VALUES(0),(2),(2)) v(x) GROUP BY x+1 ORDER BY x+1", [][]model.Value{{model.Int64Val(1), model.NullVal(model.TypeInt64)}, {model.Int64Val(3), model.Int64Val(10)}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, sample.sql, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, sample.want) {
				t.Fatalf("rows=%v, want %v", got, sample.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func aggregateProjectionReleased(t *testing.T, query *runtime.Query) {
	t.Helper()
	usage := query.Usage()
	workRetained := usage.Work.Live != 0 || usage.Work.Reservations != 0
	outputRetained := usage.Output.Live != 0 || usage.Output.Reservations != 0
	if workRetained || outputRetained || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("aggregate projection retains resources: %+v", usage)
	}
}

type aggregateProjectionResolver func(expr.Function) (expr.ScalarCall, error)

func (f aggregateProjectionResolver) Resolve(fn expr.Function) (expr.ScalarCall, error) {
	return f(fn)
}

func volatileAggregateGraph(t *testing.T) *fragment.FragmentGraph {
	t.Helper()
	b, err := ir.NewBuilder(irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layouts := make(map[model.TypeID]ir.LayoutID)
	for _, typ := range []model.Type{{ID: 23, Storage: model.TypeInt64}, {ID: 20, Storage: model.TypeInt64}, {ID: 16, Storage: model.TypeBoolean}} {
		layouts[typ.ID], err = b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
	}
	add := func(spec ir.ScalarSpec) ir.ScalarID {
		spec.Scope = scope
		id, err := b.AddScalar(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	x, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layouts[23]})
	if err != nil {
		t.Fatal(err)
	}
	var values []ir.ScalarID
	for _, n := range []int64{0, 1, 2, 3} {
		values = append(values, add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Layout: layouts[23]}, Value: model.Int64Val(n)}))
	}
	input, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: 4}, Outputs: []model.SlotID{x}, Expressions: values})
	if err != nil {
		t.Fatal(err)
	}
	column := add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Slot: x, Layout: layouts[23]}})
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	call := func(function model.FunctionID, result ir.LayoutID) ir.ScalarID {
		sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: result, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layouts[23]}})
		if err != nil {
			t.Fatal(err)
		}
		return add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Function: function, Signature: sig, Layout: result, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{column}})
	}
	filter := call(900001, layouts[16])
	argument := call(900002, layouts[23])
	var outputs []model.SlotID
	var calls []ir.AggregateSpec
	for range 2 {
		result, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layouts[20], Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 2108, Result: layouts[20], Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layouts[23]}})
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, result)
		calls = append(calls, ir.AggregateSpec{AggregateCall: ir.AggregateCall{Function: 2108, Signature: sig, Result: result, Filter: filter, Effects: effects}, Args: []ir.ScalarID{argument}})
	}
	root, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: scope, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: outputs, GroupingSets: [][]uint32{{}}, Aggregates: calls})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	optimized, err := opt.Optimize(t.Context(), graph, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), optimized.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}

func TestAggregateVolatileFilterRunsOncePerCallAndMasksArguments(t *testing.T) {
	graph := volatileAggregateGraph(t)
	env := statefulEnvironment(t)
	filters, arguments := 0, 0
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			x := args[0].IntVal
			if fn.ID == 900001 {
				filters++
				if x == 1 {
					return model.NullVal(model.TypeBoolean), nil
				}
				return model.BoolVal(x >= 2), nil
			}
			if fn.ID != 900002 || x < 2 {
				t.Fatalf("inactive or unexpected function %d on x=%d", fn.ID, x)
			}
			arguments++
			return model.Int64Val(x), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(5), model.Int64Val(5)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) || filters != 8 || arguments != 4 {
		t.Fatalf("rows=%v filters=%d arguments=%d; want %v, 8, 4", got, filters, arguments, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestAggregateProjectionCancellationReleasesState(t *testing.T) {
	graph := volatileAggregateGraph(t)
	env := statefulEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			cancel()
			return model.BoolVal(true), nil
		}, nil
	})
	portal, err := exec.Open(ctx, graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled aggregate = %v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestAggregateProjectionConsumerAdmissionFailureReleasesChild(t *testing.T) {
	graph := sqlGraph(t, "SELECT sum(10/x) FILTER(WHERE x<>0) FROM (VALUES(0),(2)) v(x)", nil)
	env := statefulEnvironment(t)
	env.Aggregate.WorkBytes = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if portal != nil {
		closePortal(t, portal)
	}
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("aggregate admission failure = %v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestAggregateProjectionOutputQuotaReleasesEvaluation(t *testing.T) {
	graph := volatileAggregateGraph(t)
	env := statefulEnvironment(t)
	var held *runtime.Reservation
	defer func() {
		if held != nil {
			held.Release()
		}
	}()
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			if held == nil {
				var err error
				usage := env.Query.Usage().Output
				held, err = env.Query.TryReserve(runtime.OutputMemory, usage.Limit-usage.Live)
				if err != nil {
					return model.Value{}, err
				}
			}
			if fn.ID == 900001 {
				return model.BoolVal(true), nil
			}
			return model.Int64Val(1), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	if err := portal.Next(t.Context(), &batch); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("projection output quota failure = %v", err)
	}
	if held == nil {
		t.Fatal("quota exhausted before aggregate projection evaluation")
	}
	held.Release()
	held = nil
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLGroupingResolvesOrdinalsAndOutputAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"computed ordinal", `SELECT x+1,count(*) FROM (VALUES(1),(1),(2)) t(x) GROUP BY 1 ORDER BY 1`, [][]model.Value{{model.Int64Val(2), model.Int64Val(2)}, {model.Int64Val(3), model.Int64Val(1)}}},
		{"output alias", `SELECT x+1 AS bucket,count(*) FROM (VALUES(1),(1),(2)) t(x) GROUP BY bucket ORDER BY bucket`, [][]model.Value{{model.Int64Val(2), model.Int64Val(2)}, {model.Int64Val(3), model.Int64Val(1)}}},
		{"input precedes alias", `SELECT x%2 AS x,count(*) FROM (VALUES(1),(3),(2)) t(x) GROUP BY x ORDER BY x`, [][]model.Value{{model.Int64Val(0), model.Int64Val(1)}, {model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(1), model.Int64Val(1)}}},
		{"expanded star ordinals", `SELECT t.*,count(*) FROM (VALUES(1,9),(1,9),(2,8)) t(x,y) GROUP BY 1,2 ORDER BY 1`, [][]model.Value{{model.Int64Val(1), model.Int64Val(9), model.Int64Val(2)}, {model.Int64Val(2), model.Int64Val(8), model.Int64Val(1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
