package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLCompositeFieldsPreserveRowAndArrayEvaluation(t *testing.T) {
	graph := sqlGraph(t, `SELECT (ROW(1,'x'::text)).f2,(ROW(ARRAY[3,4])).f1[2],(q.r).f1 FROM (SELECT ROW(7) AS r)q`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 23, 23}, [][]model.Value{{model.StringVal("x"), model.Int64Val(4), model.Int64Val(7)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCompositeFieldDescriptorSurvivesCTEAndUnion(t *testing.T) {
	graph := sqlGraph(t, `WITH q AS (SELECT ROW(1)r UNION ALL SELECT ROW(2)) SELECT (q.r).f1 FROM q`, nil)
	env := configureSetWindowEnvironment(statefulEnvironment(t))
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCompositeFieldSkipsUnselectedRuntimeErrors(t *testing.T) {
	for _, sql := range []string{
		`SELECT (ROW(1/n,2)).f2 FROM generate_series(0,0)t(n)`,
		`SELECT (s.r).f2 FROM (SELECT ROW(1/n,2)r FROM generate_series(0,0)t(n))s`,
	} {
		t.Run(sql, func(t *testing.T) {
			graph := sqlGraph(t, sql, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(2)}})
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
