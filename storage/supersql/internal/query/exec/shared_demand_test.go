package exec_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestSharedCTELimitDoesNotEvaluateUnneededRows(t *testing.T) {
	graph := sqlGraph(t, `WITH c AS MATERIALIZED (SELECT 10/x AS y FROM (VALUES(2),(0)) t(x)) SELECT y FROM (SELECT y FROM c UNION ALL SELECT y FROM c) q LIMIT 1`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(5)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("limited shared rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestMultipleRecursiveConsumersReturnFinitePrefix(t *testing.T) {
	graph := sqlGraph(t, `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM t) SELECT n FROM (SELECT n FROM t UNION ALL SELECT n FROM t) q LIMIT 3`, nil)
	env := recursiveEnvironment(t)
	env.Recursive.MaxIterations = 3
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive prefix=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSharedCTELaggingReaderSurvivesSpillMigration(t *testing.T) {
	var sql strings.Builder
	sql.WriteString("WITH c AS MATERIALIZED (SELECT x FROM (VALUES")
	for i := 1; i <= 100; i++ {
		if i != 1 {
			sql.WriteByte(',')
		}
		fmt.Fprintf(&sql, "(%d)", i)
	}
	sql.WriteString(") v(x)) SELECT a.x,b.total FROM c a CROSS JOIN LATERAL (SELECT sum(c.x+a.x) AS total FROM c) b LIMIT 2")
	graph := sqlGraph(t, sql.String(), nil)
	env := recursiveEnvironment(t)
	env.Spool.MemoryBytes = 512
	env.Apply.MaxRowBytes, env.Apply.WorkBytes = 4096, 64<<10
	env.Aggregate = aggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(5150)}, {model.Int64Val(2), model.Int64Val(5250)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("lagging shared rows=%v want=%v", got, want)
	}
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("shared reader migration did not exercise spill")
	}
	aggregateProjectionReleased(t, env.Query)
}
