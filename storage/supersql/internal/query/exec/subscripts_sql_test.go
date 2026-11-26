package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLGenerateSubscriptsUsesBoundsAndOrdinality(t *testing.T) {
	graph := sqlGraph(t, `SELECT s,n FROM generate_subscripts('[-2:-1][4:6]={{1,2,3},{4,5,6}}'::integer[],2,true) WITH ORDINALITY AS g(s,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 20}, [][]model.Value{{i(6), i(1)}, {i(5), i(2)}, {i(4), i(3)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLGenerateSubscriptsCapturesLateralArrays(t *testing.T) {
	graph := sqlGraph(t, `SELECT v.id,g.s,g.n FROM (VALUES(1,'[-1:0]={8,9}'::integer[]),(2,NULL::integer[]),(3,ARRAY[7]))v(id,a) LEFT JOIN LATERAL generate_subscripts(v.a,1,true) WITH ORDINALITY AS g(s,n) ON true ORDER BY v.id,g.n`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, n := model.Int64Val, model.NullVal(model.TypeInt64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 20}, [][]model.Value{{i(1), i(0), i(1)}, {i(1), i(-1), i(2)}, {i(2), n, n}, {i(3), i(1), i(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLGenerateSubscriptsEmptyDimensionsAndStrictNulls(t *testing.T) {
	for _, args := range []string{`ARRAY[]::integer[],1`, `ARRAY[1],0`, `ARRAY[[1]],3`, `NULL::integer[],1`, `ARRAY[1],NULL::integer,true`, `ARRAY[1],1,NULL::boolean`} {
		t.Run(args, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT * FROM generate_subscripts("+args+")", nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{23}, nil)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLGenerateSubscriptsEvaluatesDimensionDespiteNullArray(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM generate_series(0,0)t(n),LATERAL generate_subscripts(NULL::integer[],1/n,true)g(s)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "22012" {
		t.Fatalf("dimension error=%v, want 22012", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLGenerateSubscriptsIncludesMaximumSubscript(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM generate_subscripts(array_prepend(0,'[2147483646:2147483646]={1}'::integer[]),1)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(2147483646)}, {model.Int64Val(2147483647)}})
	aggregateProjectionReleased(t, env.Query)
}
