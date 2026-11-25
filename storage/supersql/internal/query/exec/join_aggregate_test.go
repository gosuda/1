package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

func statefulEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env, _ := environment(t)
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env.Join = exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2}
	env.Aggregate = exaggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4}
	env.Sort = exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2}
	return env
}

func TestSQLLeftJoinPreservesDuplicateMatchesAndUnmatchedRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x AS x,b.y AS y FROM (VALUES(1),(1),(2)) a(x) LEFT JOIN (VALUES(1,10),(1,20),(3,30)) b(x,y) ON a.x=b.x ORDER BY x,y`, nil)
	portal, err := exec.Open(t.Context(), graph, statefulEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(1), model.Int64Val(20)}, {model.Int64Val(1), model.Int64Val(20)}, {model.Int64Val(2), model.NullVal(model.TypeInt64)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("left join rows=%v want=%v", got, want)
	}
}

func TestSQLGroupedSumAndCountIgnoreNullArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,sum(x) AS total,count(x) AS n FROM (VALUES(1,2),(1,NULL),(1,4),(2,9)) v(g,x) GROUP BY g ORDER BY g`, nil)
	portal, err := exec.Open(t.Context(), graph, statefulEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(6), model.Int64Val(2)}, {model.Int64Val(2), model.Int64Val(9), model.Int64Val(1)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregate rows=%v want=%v", got, want)
	}
}
