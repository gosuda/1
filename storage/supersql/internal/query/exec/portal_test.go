package exec_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func irLimits() ir.Limits {
	return ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: 10000}
}
func sqlGraph(t *testing.T, sql string, parameters []model.TypeLayout) *fragment.FragmentGraph {
	t.Helper()
	return sqlGraphWithCatalog(t, sql, nil, parameters)
}

func sqlGraphWithCatalog(t *testing.T, sql string, snapshot bind.CatalogSnapshot, parameters []model.TypeLayout) *fragment.FragmentGraph {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()}, parameters)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := normalize.Normalize(t.Context(), bound.Graph(), normalize.Options{Limits: irLimits(), MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}})
	if err != nil {
		t.Fatal(err)
	}
	optimized, err := opt.Optimize(t.Context(), logical, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), optimized.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
func environment(t *testing.T) (exec.Environment, *runtime.Budget) {
	t.Helper()
	return environmentWithQuery(t, runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 32 << 20, MaxReservations: 10000, SpillMode: runtime.SpillDisabled})
}

func environmentWithQuery(t *testing.T, config runtime.QueryConfig) (exec.Environment, *runtime.Budget) {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2, MaxGroups: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	shared, err := runtime.NewBudget(128<<20, 10000)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, shared, runtime.OSFileSystem{}, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := expr.DefaultLimits()
	limits.MaxRows = 2
	return exec.Environment{Query: query, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}, Limits: exec.Limits{Expressions: limits, BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 8 << 20}}, shared
}
func rows(t *testing.T, portal *exec.Portal) [][]model.Value {
	t.Helper()
	var result [][]model.Value
	var batch columnar.Batch
	for {
		err := portal.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range batch.Rows() {
			row := make([]model.Value, len(batch.Columns))
			for j := range row {
				var err error
				row[j], _, err = columnar.CopyValue(t.Context(), &batch.Columns[j], batch.Lane(i), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
			}
			result = append(result, row)
		}
	}
	if err := portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("repeated EOF = %v", err)
	}
	return result
}
func closePortal(t *testing.T, portal *exec.Portal) {
	t.Helper()
	if err := portal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLStreamingFilterProjectLimit(t *testing.T) {
	graph := sqlGraph(t, "SELECT x * 10 FROM (VALUES (1), (2), (3), (4), (5)) AS v(x) WHERE x > 2 LIMIT 2 OFFSET 1", nil)
	env, shared := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(40)}, {model.Int64Val(50)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if usage := shared.Usage(); usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("completed portal retains query memory: %+v", usage)
	}
	closePortal(t, portal)
}

func TestSQLTypedParametersOwnVariablePayload(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	graph := sqlGraph(t, "SELECT $1::bytea", []model.TypeLayout{layout})
	env, _ := environment(t)
	payload := []byte{1, 2, 3}
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.BytesVal(payload)}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	payload[0] = 9
	got := rows(t, portal)
	if len(got) != 1 || len(got[0]) != 1 || !reflect.DeepEqual(got[0][0].BytesVal, []byte{1, 2, 3}) {
		t.Fatalf("parameter snapshot = %v", got)
	}
}

func TestSQLCasePreservesActiveMask(t *testing.T) {
	graph := sqlGraph(t, "SELECT CASE WHEN x = 0 THEN 0 ELSE 10 / x END FROM (VALUES (0), (2), (NULL)) AS v(x)", nil)
	for _, width := range []int{1, 17, 1024} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			env, _ := environment(t)
			env.Limits.BatchRows, env.Limits.Expressions.MaxRows = width, width
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			got := rows(t, portal)
			want := [][]model.Value{{model.Int64Val(0)}, {model.Int64Val(5)}, {model.NullVal(model.TypeInt64)}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("CASE rows=%v want=%v", got, want)
			}
		})
	}
}

func TestSQLEmptyFilteredBatchIsNotEOF(t *testing.T) {
	graph := sqlGraph(t, "SELECT x FROM (VALUES (1), (2), (3)) AS v(x) WHERE x > 2", nil)
	env, _ := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	if err = portal.Next(t.Context(), &batch); err != nil || batch.Rows() != 0 {
		t.Fatalf("empty intermediate = %d rows, %v", batch.Rows(), err)
	}
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after empty batch = %v", got)
	}
}

func TestSQLRuntimeLimitBounds(t *testing.T) {
	typ := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	for _, sample := range []struct {
		name  string
		value model.Value
		want  int
		code  string
	}{
		{"negative", model.Int64Val(-1), 0, "2201W"},
		{"null unbounded", model.NullVal(model.TypeInt64), 3, ""},
		{"zero", model.Int64Val(0), 0, ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT x FROM (VALUES (1), (2), (3)) AS v(x) LIMIT $1", []model.TypeLayout{typ})
			env, shared := environment(t)
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: typ, Value: sample.value}}
			portal, err := exec.Open(t.Context(), graph, env)
			if sample.code != "" {
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != sample.code {
					t.Fatalf("limit error = %v, want %s", err, sample.code)
				}
				if shared.Usage().Live != 0 {
					t.Fatal("failed Open retained memory")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); len(got) != sample.want {
				t.Fatalf("limit rows = %v, want %d", got, sample.want)
			}
		})
	}
}

func TestUnsupportedJoinFailsBeforeInvocationEffects(t *testing.T) {
	graph := sqlGraph(t, "SELECT a.x FROM (VALUES (1)) AS a(x) CROSS JOIN (VALUES (2)) AS b(y)", nil)
	env, shared := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	var gap *exec.RequiredExecutionError
	if portal != nil || !errors.As(err, &gap) {
		t.Fatalf("unsupported join = %v, %v", portal, err)
	}
	if shared.Usage().Live != 0 {
		t.Fatal("unsupported graph retained resources")
	}
}

func TestSharedBudgetRejectsOpenAndReleasesOwnership(t *testing.T) {
	graph := sqlGraph(t, "SELECT 1", nil)
	env, shared := environment(t)
	blocker, err := shared.TryReserve((128 << 20) - 16)
	if err != nil {
		t.Fatal(err)
	}
	portal, err := exec.Open(t.Context(), graph, env)
	if portal != nil || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("shared memory rejection = %v, %v", portal, err)
	}
	blocker.Release()
	if usage := shared.Usage(); usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("failed Open leaked ownership: %+v", usage)
	}
}

func TestCanceledNextClosesPipeline(t *testing.T) {
	graph := sqlGraph(t, "SELECT x FROM (VALUES (1), (2), (3)) AS v(x)", nil)
	env, shared := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var batch columnar.Batch
	if err = portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Next = %v", err)
	}
	closePortal(t, portal)
	if shared.Usage().Live != 0 {
		t.Fatal("canceled portal retained query memory")
	}
}

func TestSQLUnionAllPreservesDuplicateNullRows(t *testing.T) {
	graph := sqlGraph(t, "SELECT 1::int8 UNION ALL SELECT 1::int8 UNION ALL SELECT NULL::int8", nil)
	env, _ := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(1)}, {model.NullVal(model.TypeInt64)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UNION ALL rows = %v, want %v", got, want)
	}
}

func TestSQLMixedNumericComparisonPrefersFloat8(t *testing.T) {
	graph := sqlGraph(t, `SELECT 16777217::float8=16777217,16777217::float8=16777216,CASE 16777217::float8 WHEN 16777217 THEN 7 ELSE 9 END`, nil)
	env, _ := environment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.BoolVal(true), model.BoolVal(false), model.Int64Val(7)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed numeric comparison rows=%v, want %v", got, want)
	}
}
