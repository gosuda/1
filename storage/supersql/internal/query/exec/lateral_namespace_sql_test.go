package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUncorrelatedOuterLateralUsesOrdinaryJoin(t *testing.T) {
	for _, kind := range []string{"RIGHT", "FULL"} {
		t.Run(kind, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT l.x,r.y FROM (VALUES(1)) l(x) `+kind+` JOIN LATERAL (SELECT 2 AS y) r ON FALSE ORDER BY l.x NULLS LAST`, nil)
			portal, err := exec.Open(t.Context(), graph, setWindowEnvironment(t))
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(2)}}
			if kind == "FULL" {
				want = append([][]model.Value{{model.Int64Val(1), model.NullVal(model.TypeInt64)}}, want...)
			}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("outer LATERAL rows %v, want %v", got, want)
			}
		})
	}
}

func TestSQLLateralCapturesQualifiedStar(t *testing.T) {
	graph := sqlGraph(t, `SELECT r.* FROM (VALUES(1,10),(2,20)) l(x,y) CROSS JOIN LATERAL (SELECT l.*) r ORDER BY r.x`, nil)
	portal, err := exec.Open(t.Context(), graph, applyEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(2), model.Int64Val(20)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured star rows %v, want %v", got, want)
	}
}

func TestSQLOuterLateralCanCaptureHigherQueryLevel(t *testing.T) {
	graph := sqlGraph(t, `SELECT o.x,z.v FROM (VALUES(1),(2)) o(x) CROSS JOIN LATERAL (SELECT r.v FROM (VALUES(0)) l(k) RIGHT JOIN LATERAL (SELECT o.x*10 AS v) r ON TRUE) z ORDER BY o.x`, nil)
	portal, err := exec.Open(t.Context(), graph, applyEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(2), model.Int64Val(20)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("higher-level captures %v, want %v", got, want)
	}
}
