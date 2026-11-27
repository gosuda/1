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
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func spillingWindowEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env, _ := environmentWithQuery(t, runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 32 << 20, MaxReservations: 10000, SpillBytes: 64 << 20, Directory: t.TempDir(), MaxFiles: 16, MaxReaders: 32})
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env.Sort = exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2}
	return configureSetWindowEnvironment(env)
}

func TestSQLComputedWindowKeysPreservePeersAndForwardedColumns(t *testing.T) {
	graph := sqlGraph(t, `SELECT id,p,x,rank() OVER (PARTITION BY p/2 ORDER BY x+1 DESC NULLS FIRST) AS r FROM (VALUES(1,2,10),(2,3,10),(3,2,20),(4,4,NULL),(5,4,7)) t(id,p,x) ORDER BY id`, nil)
	env := spillingWindowEnvironment(t)
	env.Limits.BatchRows = 1
	env.Window.PartitionMemoryBytes = 0
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(2), model.Int64Val(10), model.Int64Val(2)},
		{model.Int64Val(2), model.Int64Val(3), model.Int64Val(10), model.Int64Val(2)},
		{model.Int64Val(3), model.Int64Val(2), model.Int64Val(20), model.Int64Val(1)},
		{model.Int64Val(4), model.Int64Val(4), model.NullVal(model.TypeInt64), model.Int64Val(1)},
		{model.Int64Val(5), model.Int64Val(4), model.Int64Val(7), model.Int64Val(2)},
	}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWindowArgumentsEvaluateOnlyAccessedRows(t *testing.T) {
	for _, sample := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"lag value and default", `SELECT lag(10/x,1,10/x) OVER (ORDER BY id) FROM (VALUES(1,2),(2,0)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(5)}, {model.Int64Val(5)}}},
		{"lead value", `SELECT lead(10/x,1,10/(x+1)) OVER (ORDER BY id) FROM (VALUES(1,0),(2,2)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(5)}, {model.Int64Val(3)}}},
		{"null offset", `SELECT lead(10/x,NULL::integer,10/x) OVER (ORDER BY id) FROM (VALUES(1,0),(2,0)) t(id,x) ORDER BY id`, [][]model.Value{{model.NullVal(model.TypeInt64)}, {model.NullVal(model.TypeInt64)}}},
		{"first value", `SELECT first_value(10/x) OVER (ORDER BY id) FROM (VALUES(1,2),(2,0)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(5)}, {model.Int64Val(5)}}},
		{"ntile initial count", `SELECT ntile(10/x) OVER (ORDER BY id) FROM (VALUES(1,2),(2,0)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}}},
		{"nth outside frame", `SELECT nth_value(10/x,id+1) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM (VALUES(1,0),(2,2)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(5)}, {model.NullVal(model.TypeInt64)}}},
		{"masked ordering", `SELECT rank() OVER (ORDER BY CASE WHEN x<>0 THEN 10/x ELSE NULL END) FROM (VALUES(1,0),(2,2)) t(id,x) ORDER BY id`, [][]model.Value{{model.Int64Val(2)}, {model.Int64Val(1)}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, sample.sql, nil)
			env := spillingWindowEnvironment(t)
			env.Limits.BatchRows = 1
			env.Window.PartitionMemoryBytes = 0
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

func TestSQLWindowExpressionErrorReleasesSpilledPartition(t *testing.T) {
	for _, sql := range []string{
		`SELECT lead(x+1,10,10/(x-1)) OVER (ORDER BY x) FROM (VALUES(1)) t(x)`,
		`SELECT rank() OVER (PARTITION BY 10/x ORDER BY x) FROM (VALUES(0)) t(x)`,
	} {
		graph := sqlGraph(t, sql, nil)
		env := spillingWindowEnvironment(t)
		env.Window.PartitionMemoryBytes = 0
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		var batch columnar.Batch
		err = portal.Next(t.Context(), &batch)
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
			t.Fatalf("Next error=%v, want division by zero", err)
		}
		aggregateProjectionReleased(t, env.Query)
	}
}

func TestWindowAdmissionFailureReleasesArgumentEvaluator(t *testing.T) {
	graph := sqlGraph(t, `SELECT lead(x+1) OVER (ORDER BY x) FROM (VALUES(1),(2)) t(x)`, nil)
	env := setWindowEnvironment(t)
	env.Window.WorkBytes = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err == nil || portal != nil {
		if portal != nil {
			closePortal(t, portal)
		}
		t.Fatalf("Open=%v, error=%v; want failed admission", portal, err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestWindowVolatileArgumentRunsAtRequestedRows(t *testing.T) {
	base := sqlGraph(t, `SELECT x FROM (VALUES(1),(2),(3)) t(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	root, _ := graph.Rel(graph.Root())
	input, _ := graph.Output(graph.Root(), 0)
	info, _ := graph.Slot(input)
	b, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	add := func(spec ir.ScalarSpec) ir.ScalarID {
		spec.Scope = root.Scope
		id, err := b.AddScalar(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	signature := func(function model.FunctionID, count int, effects ir.Effects) ir.SignatureID {
		args := make([]ir.LayoutID, count)
		for i := range args {
			args[i] = info.Layout
		}
		id, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: info.Layout, Nullable: true, Effects: effects}, Parameters: args})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	column := add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Slot: input, Layout: info.Layout}})
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	value := add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Function: 900001, Signature: signature(900001, 1, effects), Layout: info.Layout, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{column}})
	value = add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Layout: info.Layout, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{value}})
	null := add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Layout: info.Layout, Nullable: true}, Value: model.NullVal(model.TypeInt64)})
	outputs := []model.SlotID{input}
	var calls []ir.WindowSpec
	for i, function := range []model.FunctionID{3112, 3111} {
		output, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: root.Scope, Layout: info.Layout, Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
		args := []ir.ScalarID{value}
		if i == 1 {
			args = []ir.ScalarID{value, null, value}
		}
		calls = append(calls, ir.WindowSpec{WindowCall: ir.WindowCall{Function: function, Signature: signature(function, len(args), effects), Result: output, Effects: effects}, Args: args})
	}
	window, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Window, Scope: root.Scope, Effects: effects, Frame: ir.Frame{Mode: ir.RangeFrame, Start: ir.FrameBound{Kind: ir.UnboundedPreceding}, End: ir.FrameBound{Kind: ir.CurrentRow}}}, Inputs: []ir.RelID{graph.Root()}, Outputs: outputs, Windows: calls})
	if err != nil {
		t.Fatal(err)
	}
	graph, err = b.Build(t.Context(), window)
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
	env := spillingWindowEnvironment(t)
	env.Window.PartitionMemoryBytes = 0
	count := int64(0)
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			if fn.ID != 900001 || args[0].IntVal != 1 {
				t.Fatalf("unexpected argument evaluation: function=%d args=%v", fn.ID, args)
			}
			count++
			return model.Int64Val(count), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), fragments, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(1), model.NullVal(model.TypeInt64)}, {model.Int64Val(2), model.Int64Val(2), model.NullVal(model.TypeInt64)}, {model.Int64Val(3), model.Int64Val(3), model.NullVal(model.TypeInt64)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) || count != 3 {
		t.Fatalf("rows=%v calls=%d, want %v and 3 calls", got, count, want)
	}
	aggregateProjectionReleased(t, env.Query)
}
