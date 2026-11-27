package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
)

func setWindowEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env := statefulEnvironment(t)
	return configureSetWindowEnvironment(env)
}

func configureSetWindowEnvironment(env exec.Environment) exec.Environment {
	env.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4}
	env.Set = setop.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, PartitionMemoryBytes: 64 << 10, MaxSpillBytes: 16 << 20, WorkBytes: 16 << 20, MaxPasses: 16}
	env.Window = window.Options{MaxRecordBytes: 4096, PartitionMemoryBytes: 64 << 10, MaxPartitionBytes: 16 << 20, MaxPartitionRows: 10000, WorkBytes: 16 << 20}
	return env
}

func TestSQLExceptAllPreservesRemainingMultiplicity(t *testing.T) {
	graph := sqlGraph(t, `SELECT x FROM (VALUES(1),(1),(2),(NULL)) a(x) EXCEPT ALL SELECT x FROM (VALUES(1),(NULL)) b(x) ORDER BY x`, nil)
	portal, err := exec.Open(t.Context(), graph, setWindowEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EXCEPT ALL rows=%v want=%v", got, want)
	}
}

func TestSQLDistinctOnRetainsFirstOrderedRepresentative(t *testing.T) {
	graph := sqlGraph(t, `SELECT DISTINCT ON (k) k,v FROM (VALUES(1,10),(1,20),(2,30)) t(k,v) ORDER BY k,v DESC`, nil)
	portal, err := exec.Open(t.Context(), graph, setWindowEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(20)}, {model.Int64Val(2), model.Int64Val(30)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DISTINCT ON rows=%v want=%v", got, want)
	}
}

func TestSQLRankResetsAtPartitionAndPreservesPeers(t *testing.T) {
	graph := sqlGraph(t, `SELECT p,x,rank() OVER (PARTITION BY p ORDER BY x) AS r FROM (VALUES(1,2),(1,2),(1,3),(2,9)) t(p,x) ORDER BY p,x`, nil)
	portal, err := exec.Open(t.Context(), graph, setWindowEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(2), model.Int64Val(1)}, {model.Int64Val(1), model.Int64Val(2), model.Int64Val(1)}, {model.Int64Val(1), model.Int64Val(3), model.Int64Val(3)}, {model.Int64Val(2), model.Int64Val(9), model.Int64Val(1)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rank rows=%v want=%v", got, want)
	}
}
