package exec_test

import (
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

func recursiveEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env := spillingWindowEnvironment(t)
	env.Join = exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2}
	env.Recursive = recursive.Options{MaxIterations: 10000,
		Spool: rowstore.Limits{MemoryBytes: 0, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 8},
		Seen:  distinctset.Options{MemoryBytes: 4096, WorkBytes: 256 << 10, MaxKeyBytes: 4096, MaxKeys: 10000, MaxLevels: 8},
	}
	return env
}

func TestSQLRecursiveDeltasPreserveUnionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"UNION ALL sequence", `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM t WHERE n<5) SELECT n FROM t`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}, {model.Int64Val(4)}, {model.Int64Val(5)}}},
		{"UNION cycle", `WITH RECURSIVE t(n) AS (VALUES(1),(1) UNION SELECT n FROM t) SELECT n FROM t`, [][]model.Value{{model.Int64Val(1)}}},
		{"NULL cycle", `WITH RECURSIVE t(n) AS (VALUES(NULL::integer) UNION SELECT n FROM t) SELECT n FROM t`, [][]model.Value{{model.NullVal(model.TypeInt64)}}},
		{"branching deltas", `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT t.n*2+b.x FROM t CROSS JOIN (VALUES(0),(1)) b(x) WHERE t.n<3) SELECT n FROM t ORDER BY n`, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}, {model.Int64Val(4)}, {model.Int64Val(5)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := recursiveEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("recursive rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLRecursiveSearchCycleGeneratedColumns(t *testing.T) {
	record := func(values ...model.Value) model.Value {
		return model.Value{Type: model.TypeComposite, ArrayVal: values}
	}
	path := func(records ...model.Value) model.Value {
		return model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: int32(len(records)), LowerBound: 1}}, ArrayVal: records}
	}
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			"breadth first",
			`WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n<3) SEARCH BREADTH FIRST BY n SET ord SELECT n,ord FROM t`,
			[][]model.Value{
				{model.Int64Val(1), record(model.Int64Val(0), model.Int64Val(1))},
				{model.Int64Val(2), record(model.Int64Val(1), model.Int64Val(2))},
				{model.Int64Val(3), record(model.Int64Val(2), model.Int64Val(3))},
			},
		},
		{
			"depth first parent lineage",
			`WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n+x FROM t CROSS JOIN (VALUES(1),(2)) AS v(x) WHERE n<3) SEARCH DEPTH FIRST BY n SET ord SELECT n,ord FROM t`,
			[][]model.Value{
				{model.Int64Val(1), path(record(model.Int64Val(1)))},
				{model.Int64Val(2), path(record(model.Int64Val(1)), record(model.Int64Val(2)))},
				{model.Int64Val(3), path(record(model.Int64Val(1)), record(model.Int64Val(3)))},
				{model.Int64Val(3), path(record(model.Int64Val(1)), record(model.Int64Val(2)), record(model.Int64Val(3)))},
				{model.Int64Val(4), path(record(model.Int64Val(1)), record(model.Int64Val(2)), record(model.Int64Val(4)))},
			},
		},
		{
			"cycle custom marks and union",
			`WITH RECURSIVE t(n) AS (SELECT 1 UNION SELECT CASE WHEN n=1 THEN 2 ELSE 1 END FROM t) CYCLE n SET cycled TO 'hit' DEFAULT 'go' USING trail SELECT n,cycled,trail FROM t`,
			[][]model.Value{
				{model.Int64Val(1), model.StringVal("go"), path(record(model.Int64Val(1)))},
				{model.Int64Val(2), model.StringVal("go"), path(record(model.Int64Val(1)), record(model.Int64Val(2)))},
				{model.Int64Val(1), model.StringVal("hit"), path(record(model.Int64Val(1)), record(model.Int64Val(2)), record(model.Int64Val(1)))},
			},
		},
		{
			"cycle null key",
			`WITH RECURSIVE t(n) AS (SELECT NULL::integer UNION ALL SELECT n FROM t) CYCLE n SET cycled USING trail SELECT n,cycled,trail FROM t`,
			[][]model.Value{
				{model.NullVal(model.TypeInt64), model.BoolVal(false), path(record(model.NullVal(model.TypeInt64)))},
				{model.NullVal(model.TypeInt64), model.BoolVal(true), path(record(model.NullVal(model.TypeInt64)), record(model.NullVal(model.TypeInt64)))},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := recursiveEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("recursive rows=%#v want=%#v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLRecursiveLimitDoesNotDrainInfiniteMember(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM t) SELECT n FROM t LIMIT 1`, nil)
	env := recursiveEnvironment(t)
	env.Recursive.MaxIterations = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("limited recursion=%v", got)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveEarlyCloseReleasesWorktable(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM t) SELECT n FROM t`, nil)
	env := recursiveEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	batch := columnar.Batch{MaxRows: 1}
	for _, want := range []int64{1, 2} {
		if err := portal.Next(t.Context(), &batch); err != nil {
			t.Fatal(err)
		}
		if batch.Rows() != 1 || batch.Columns[0].Int64[0] != want {
			t.Fatalf("recursive batch=%+v want=%d", batch, want)
		}
	}
	if err := portal.Close(); err != nil {
		t.Fatal(err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveIterationLimitPropagates(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n FROM t) SELECT n FROM t`, nil)
	env := recursiveEnvironment(t)
	env.Recursive.MaxIterations = 2
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	for {
		err = portal.Next(t.Context(), &batch)
		if err != nil {
			break
		}
	}
	var sqlErr *model.Error
	if err == io.EOF || !errors.As(err, &sqlErr) || sqlErr.Code != "53200" {
		t.Fatalf("recursion limit error=%v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveDistinctSpillsSeenRows(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION SELECT n+1 FROM t WHERE n<100) SELECT n FROM t`, nil)
	env := recursiveEnvironment(t)
	env.Recursive.Seen.MemoryBytes = 0
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	if len(got) != 100 {
		t.Fatalf("recursive rows=%d want=100", len(got))
	}
	for i, row := range got {
		if len(row) != 1 || row[0].IsNull || row[0].IntVal != int64(i+1) {
			t.Fatalf("row %d=%v", i, row)
		}
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveStepUsesOuterCaptures(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x,b.n FROM (VALUES(1),(3)) a(x) CROSS JOIN LATERAL (WITH RECURSIVE t(n) AS (SELECT a.x UNION ALL SELECT n+1 FROM t WHERE n<a.x+2) SELECT n FROM t) b`, nil)
	env := recursiveEnvironment(t)
	env.Apply.MaxRowBytes, env.Apply.WorkBytes = 4096, 64<<10
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(1)}, {model.Int64Val(1), model.Int64Val(2)}, {model.Int64Val(1), model.Int64Val(3)}, {model.Int64Val(3), model.Int64Val(3)}, {model.Int64Val(3), model.Int64Val(4)}, {model.Int64Val(3), model.Int64Val(5)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured recursion=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveStepBorrowsExternalCTE(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE step AS MATERIALIZED (SELECT 1 AS inc), t(n) AS (VALUES(1) UNION ALL SELECT n+step.inc FROM t CROSS JOIN step WHERE n<3) SELECT n FROM t`, nil)
	env := recursiveEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive CTE rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecursiveStepErrorReleasesAllState(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT 10/(n-1) FROM t) SELECT n FROM t`, nil)
	env := recursiveEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	batch := columnar.Batch{MaxRows: 1}
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
		t.Fatalf("recursive error=%v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}
