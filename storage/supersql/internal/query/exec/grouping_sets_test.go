package exec_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func groupingSetsEnvironment(t *testing.T, spill bool) (exec.Environment, string) {
	t.Helper()
	dir := t.TempDir()
	config := runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 32 << 20, MaxReservations: 10000, SpillMode: runtime.SpillEnabled, SpillBytes: 64 << 20, Directory: dir, MaxFiles: 24, MaxReaders: 32}
	env, _ := environmentWithQuery(t, config)
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env = configureSetWindowEnvironment(env)
	env.Aggregate = exaggregate.Options{MaxRecordBytes: 1024, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 128, MaxPartitions: 16}
	env.Sort.MaxRecordBytes, env.Sort.RunBytes, env.Sort.WorkBytes = 4096, 64<<10, 8<<20
	env.Sort.MaxRuns, env.Sort.MergeFanIn = 4, 2
	if spill {
		env.Spool.MemoryBytes = 0
	}
	return env, dir
}

func groupingSetsReleased(t *testing.T, env exec.Environment, dir string) {
	t.Helper()
	aggregateProjectionReleased(t, env.Query)
	if usage := env.Query.Usage().Spill; usage.Live != 0 || usage.Reservations != 0 {
		t.Errorf("grouping sets retained spill claims: %+v", usage)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("grouping sets retained spill files: %v", files)
	}
}

func assertGroupingSetsBag(t *testing.T, got, want [][]model.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("grouping-set rows=%v, want bag %v", got, want)
	}
	matched := make([]bool, len(want))
	for _, row := range got {
		found := false
		for i, expected := range want {
			if !matched[i] && reflect.DeepEqual(row, expected) {
				matched[i], found = true, true
				break
			}
		}
		if !found {
			t.Fatalf("unexpected grouping-set row %v; got bag %v, want %v", row, got, want)
		}
	}
}

func TestSQLGroupingSetsRows(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	const source = ` FROM (VALUES(1,10,2),(1,20,4),(2,10,8)) v(a,b,x) `
	rollup := [][]model.Value{{i(1), i(10), i(2)}, {i(1), i(20), i(4)}, {i(2), i(10), i(8)}, {i(1), null, i(6)}, {i(2), null, i(8)}, {null, null, i(14)}}
	cube := append(append([][]model.Value(nil), rollup...), []model.Value{null, i(10), i(10)}, []model.Value{null, i(20), i(4)})
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"explicit reordered keys", `SELECT a,b,sum(x)` + source + `GROUP BY GROUPING SETS ((b,a),(a),())`, rollup},
		{"rollup", `SELECT a,b,sum(x)` + source + `GROUP BY ROLLUP(a,b)`, rollup},
		{"cube", `SELECT a,b,sum(x)` + source + `GROUP BY CUBE(a,b)`, cube},
		{"cartesian rollups", `SELECT a,b,sum(x)` + source + `GROUP BY ROLLUP(a),ROLLUP(b)`, cube},
		{"ordinary key times grouping sets", `SELECT a,b,sum(x)` + source + `GROUP BY a,GROUPING SETS ((b),())`, rollup[:5]},
		{"composite rollup element", `SELECT a,b,sum(x)` + source + `GROUP BY ROLLUP((a,b))`, [][]model.Value{{i(1), i(10), i(2)}, {i(1), i(20), i(4)}, {i(2), i(10), i(8)}, {null, null, i(14)}}},
		{"nested rollup", `SELECT a,b,sum(x)` + source + `GROUP BY GROUPING SETS ((a),ROLLUP(b))`, [][]model.Value{{i(1), null, i(6)}, {i(2), null, i(8)}, {null, i(10), i(10)}, {null, i(20), i(4)}, {null, null, i(14)}}},
		{"duplicate sets and empty sets", `SELECT a,sum(x)` + source + `GROUP BY GROUPING SETS ((a),(a),(),())`, [][]model.Value{{i(1), i(6)}, {i(2), i(8)}, {i(1), i(6)}, {i(2), i(8)}, {null, i(14)}, {null, i(14)}}},
		{"distinct cartesian sets", `SELECT a,b,sum(x)` + source + `GROUP BY DISTINCT ROLLUP(a,b),ROLLUP(a)`, rollup},
		{"distinct sets preserve identical null rows", `SELECT a,sum(x) FROM (VALUES(NULL::integer,5)) v(a,x) GROUP BY DISTINCT GROUPING SETS ((a),(a),(),())`, [][]model.Value{{null, i(5)}, {null, i(5)}}},
		{"real null and subtotal null", `SELECT a,b,sum(x) FROM (VALUES(1,NULL::integer,2),(1,10,3),(NULL,10,7)) v(a,b,x) GROUP BY ROLLUP(a,b)`, [][]model.Value{{i(1), null, i(2)}, {i(1), i(10), i(3)}, {null, i(10), i(7)}, {i(1), null, i(5)}, {null, null, i(7)}, {null, null, i(12)}}},
		{"empty input repeated globals", `SELECT a,count(*),sum(x) FROM (VALUES(1,2)) v(a,x) WHERE false GROUP BY GROUPING SETS ((a),(),())`, [][]model.Value{{null, i(0), null}, {null, i(0), null}}},
		{"empty input nonempty sets", `SELECT a,b,count(*)` + source + `WHERE false GROUP BY GROUPING SETS ((a),(b))`, nil},
		{"explicit empty group without aggregate", `SELECT 1 FROM (VALUES(2)) v(x) WHERE false GROUP BY ()`, [][]model.Value{{i(1)}}},
		{"repeated empty groups without aggregate", `SELECT 1 FROM (VALUES(2)) v(x) WHERE false GROUP BY GROUPING SETS ((),())`, [][]model.Value{{i(1)}, {i(1)}}},
		{"rollup without aggregate calls", `SELECT x FROM (VALUES(1),(1),(2)) v(x) GROUP BY ROLLUP(x)`, [][]model.Value{{i(1)}, {i(2)}, {null}}},
		{"computed ordinal", `SELECT x+1,count(*) FROM (VALUES(1),(1),(2)) v(x) GROUP BY GROUPING SETS ((1),())`, [][]model.Value{{i(2), i(2)}, {i(3), i(1)}, {null, i(3)}}},
		{"computed output alias", `SELECT x+1 AS bucket,count(*) FROM (VALUES(1),(1),(2)) v(x) GROUP BY ROLLUP(bucket)`, [][]model.Value{{i(2), i(2)}, {i(3), i(1)}, {null, i(3)}}},
		{"computed grouping expression", `SELECT x%2,count(*) FROM (VALUES(1),(3),(2)) v(x) GROUP BY GROUPING SETS ((x%2),())`, [][]model.Value{{i(1), i(2)}, {i(0), i(1)}, {null, i(3)}}},
		{"having on aggregate", `SELECT a,b,sum(x)` + source + `GROUP BY ROLLUP(a,b) HAVING sum(x)>6`, [][]model.Value{{i(2), i(10), i(8)}, {i(2), null, i(8)}, {null, null, i(14)}}},
		{"having on rolled up key", `SELECT a,b,sum(x)` + source + `GROUP BY ROLLUP(a,b) HAVING b IS NULL`, [][]model.Value{{i(1), null, i(6)}, {i(2), null, i(8)}, {null, null, i(14)}}},
		{"distinct filter before deduplication", `SELECT g,count(*),count(DISTINCT x) FILTER(WHERE enabled),sum(DISTINCT x) FILTER(WHERE enabled),sum(x) FILTER(WHERE enabled) FROM (VALUES(1,5,false),(1,5,true),(1,5,true),(1,3,true),(1,NULL,true),(2,5,true),(2,7,NULL),(3,9,false)) v(g,x,enabled) GROUP BY GROUPING SETS ((g),())`, [][]model.Value{{i(1), i(5), i(2), i(8), i(13)}, {i(2), i(2), i(1), i(5), i(5)}, {i(3), i(1), i(0), null, null}, {null, i(8), i(2), i(8), i(18)}}},
		{"filter masks computed argument", `SELECT g,sum(DISTINCT 10/x) FILTER(WHERE x<>0),count(*) FILTER(WHERE NULL::boolean) FROM (VALUES(1,0),(1,2),(1,2),(2,5),(2,NULL)) v(g,x) GROUP BY GROUPING SETS ((g),())`, [][]model.Value{{i(1), i(5), i(0)}, {i(2), i(2), i(0)}, {null, i(7), i(0)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			for _, spill := range []bool{false, true} {
				t.Run(fmt.Sprintf("replay_spill=%t", spill), func(t *testing.T) {
					env, dir := groupingSetsEnvironment(t, spill)
					portal, err := exec.Open(t.Context(), graph, env)
					if err != nil {
						t.Fatal(err)
					}
					defer closePortal(t, portal)
					got := rows(t, portal)
					assertGroupingSetsBag(t, got, tc.want)
					root, err := graph.Plan().CloneNode(t.Context(), graph.Plan().Root(), 4<<20)
					if err != nil {
						t.Fatal(err)
					}
					count := float64(len(got))
					if count < root.Estimate.LowerRows || count > root.Estimate.UpperRows {
						t.Fatalf("actual rows=%d outside estimated bounds [%g,%g]", len(got), root.Estimate.LowerRows, root.Estimate.UpperRows)
					}
					groupingSetsReleased(t, env, dir)
				})
			}
		})
	}
}

func TestSQLGroupingSetsTypedNulls(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,s,b,count(*) FROM (VALUES(7::smallint,'x'::text,true)) v(n,s,b) GROUP BY GROUPING SETS ((n),(s),(b),())`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	n, s, b := model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.NullVal(model.TypeBoolean)
	want := [][]model.Value{{model.Int64Val(7), s, b, model.Int64Val(1)}, {n, model.StringVal("x"), b, model.Int64Val(1)}, {n, s, model.BoolVal(true), model.Int64Val(1)}, {n, s, b, model.Int64Val(1)}}
	assertGroupingSetsBag(t, rows(t, portal), want)
	if usage := env.Query.Usage(); usage.Spill.Peak == 0 {
		t.Fatalf("typed-null replay did not spill: %+v", usage)
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLGroupingSetsReplayAndAggregateSpill(t *testing.T) {
	var values []string
	var want [][]model.Value
	for group := range 9 {
		values = append(values, fmt.Sprintf("(%d,2),(%d,2),(%d,5)", group, group, group))
		row := []model.Value{model.Int64Val(int64(group)), model.Int64Val(3), model.Int64Val(9)}
		want = append(want, row, row)
	}
	total := []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(27), model.Int64Val(81)}
	want = append(want, total, total)
	graph := sqlGraph(t, `SELECT g,count(*),sum(x) FROM (VALUES`+strings.Join(values, ",")+`) v(g,x) GROUP BY GROUPING SETS ((g),(),(g),())`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Aggregate.GroupBytes = 32 << 10
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertGroupingSetsBag(t, rows(t, portal), want)
	if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles < 2 {
		t.Fatalf("replay and aggregate did not spill together: %+v", usage)
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLGroupingSetsActiveReplayCleanup(t *testing.T) {
	for _, ending := range []string{"early close", "cancel", "truncated replay"} {
		t.Run(ending, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT sum(0) FROM (VALUES(1),(2),(3)) v(g) GROUP BY GROUPING SETS ((),(g))`, nil)
			env, dir := groupingSetsEnvironment(t, true)
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
			if batch.Rows() != 1 {
				t.Fatalf("first batch has %d rows, want 1", batch.Rows())
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(0), 4096)
			if err != nil || !reflect.DeepEqual(value, model.Int64Val(0)) {
				t.Fatalf("first row=%v, error=%v, want 0", value, err)
			}
			if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
				t.Fatalf("replay was not active before %s: %+v", ending, usage)
			}
			switch ending {
			case "early close":
				closePortal(t, portal)
			case "cancel":
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled replay=%v, want context cancellation", err)
				}
			case "truncated replay":
				files, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range files {
					if err := os.Truncate(filepath.Join(dir, file.Name()), 0); err != nil {
						t.Fatal(err)
					}
				}
				for err == nil {
					err = portal.Next(ctx, &batch)
				}
				if !errors.Is(err, rowstore.ErrCorrupt) {
					t.Fatalf("truncated replay error=%v, want corrupt row spool", err)
				}
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestGroupingSetsFreezeVolatileProjection(t *testing.T) {
	graph := groupingSetsVolatileGraph(t)
	env, dir := groupingSetsEnvironment(t, true)
	var filters, arguments int64
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			switch fn.ID {
			case 900001:
				filters++
				return model.BoolVal(args[0].IntVal != 0), nil
			case 900002:
				if args[0].IntVal == 0 {
					t.Fatal("false FILTER evaluated its argument")
				}
				arguments++
				return model.Int64Val(arguments), nil
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
	null := model.NullVal(model.TypeInt64)
	want := [][]model.Value{{null, model.Int64Val(3)}, {model.Int64Val(0), null}, {model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(2), model.Int64Val(2)}, {null, model.Int64Val(3)}}
	assertGroupingSetsBag(t, rows(t, portal), want)
	if filters != 3 || arguments != 2 {
		t.Fatalf("evaluations: FILTER=%d argument=%d, want 3 and 2 across all sets", filters, arguments)
	}
	if usage := env.Query.Usage(); usage.Spill.Peak == 0 {
		t.Fatalf("volatile projected input did not replay from spill: %+v", usage)
	}
	groupingSetsReleased(t, env, dir)
}

func groupingSetsVolatileGraph(t *testing.T) *fragment.FragmentGraph {
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
	for _, n := range []int64{0, 1, 2} {
		values = append(values, add(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Layout: layouts[23]}, Value: model.Int64Val(n)}))
	}
	input, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: 3}, Outputs: []model.SlotID{x}, Expressions: values})
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
	filter, argument := call(900001, layouts[16]), call(900002, layouts[23])
	group, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layouts[23], Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layouts[20], Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 2108, Result: layouts[20], Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layouts[23]}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: scope, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{group, result}, Expressions: []ir.ScalarID{column}, Bindings: []ir.Binding{{Slot: group, Expr: column}}, GroupingSets: [][]uint32{{}, {0}, {}}, Aggregates: []ir.AggregateSpec{{AggregateCall: ir.AggregateCall{Function: 2108, Signature: sig, Result: result, Filter: filter, Effects: effects}, Args: []ir.ScalarID{argument}}}})
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

func TestGroupingSetsKeepCurrentOuterCapture(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x,b.k,b.total FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT v.y AS k,sum(v.z+a.x) AS total FROM (VALUES(10,2),(20,3)) v(y,z) GROUP BY GROUPING SETS ((v.y),())) b ORDER BY a.x,b.k NULLS LAST`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Apply.MaxRowBytes, env.Apply.WorkBytes = 4096, 64<<10
	env.Sort.MaxRecordBytes, env.Sort.RunBytes, env.Sort.WorkBytes = 4096, 64<<10, 8<<20
	env.Sort.MaxRuns, env.Sort.MergeFanIn = 4, 2
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	want := [][]model.Value{{i(1), i(10), i(3)}, {i(1), i(20), i(4)}, {i(1), null, i(7)}, {i(2), i(10), i(4)}, {i(2), i(20), i(5)}, {i(2), null, i(9)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured grouping sets=%v want=%v", got, want)
	}
	groupingSetsReleased(t, env, dir)
}
