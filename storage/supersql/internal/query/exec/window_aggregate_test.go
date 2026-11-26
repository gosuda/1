package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLAggregateWindowFilterMasksArgumentsAcrossSpill(t *testing.T) {
	graph := sqlGraph(t, `SELECT id,sum(10/x) FILTER(WHERE x<>0) OVER(ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM (VALUES(1,0),(2,2),(3,NULL),(4,5)) t(id,x) ORDER BY id`, nil)
	env := spillingWindowEnvironment(t)
	env.Window.PartitionMemoryBytes = 0
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.NullVal(model.TypeInt64)}, {model.Int64Val(2), model.Int64Val(5)}, {model.Int64Val(3), model.Int64Val(5)}, {model.Int64Val(4), model.Int64Val(7)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLAggregateWindowMovingFrameBecomesEmpty(t *testing.T) {
	graph := sqlGraph(t, `SELECT id,sum(x) OVER(ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING) FROM (VALUES(1,2),(2,5),(3,8)) t(id,x) ORDER BY id`, nil)
	env := spillingWindowEnvironment(t)
	env.Window.PartitionMemoryBytes = 0
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(5)}, {model.Int64Val(2), model.Int64Val(8)}, {model.Int64Val(3), model.NullVal(model.TypeInt64)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWindowCountStarIncludesNullRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT count(*) OVER(ORDER BY id ROWS BETWEEN CURRENT ROW AND CURRENT ROW),count(x) OVER(ORDER BY id ROWS BETWEEN CURRENT ROW AND CURRENT ROW) FROM (VALUES(1,NULL),(2,5)) t(id,x) ORDER BY id`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(0)}, {model.Int64Val(1), model.Int64Val(1)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}
