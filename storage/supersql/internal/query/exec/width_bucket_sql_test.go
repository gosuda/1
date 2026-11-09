package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLWidthBucketAscendingDescendingAndThresholds(t *testing.T) {
	graph := sqlGraph(t, `SELECT width_bucket(x::float8,0,10,5),width_bucket(x::numeric,10::numeric,0::numeric,5),width_bucket(x::bigint,ARRAY[0,2,4,10]) FROM (VALUES(-1),(0),(2),(4),(10),(11))t(x) ORDER BY x`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23}, [][]model.Value{{i(0), i(6), i(0)}, {i(1), i(6), i(1)}, {i(2), i(5), i(2)}, {i(3), i(4), i(3)}, {i(6), i(1), i(4)}, {i(6), i(0), i(4)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWidthBucketPreservesFloatAndNumericBoundaries(t *testing.T) {
	graph := sqlGraph(t, `SELECT width_bucket(0.58::float8,0,1,100),width_bucket(0.58::numeric,0::numeric,1::numeric,100),width_bucket(0.9999999999999999::float8,-1,1,2147483647)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23}, [][]model.Value{{model.Int64Val(58), model.Int64Val(59), model.Int64Val(2147483647)}})
	aggregateProjectionReleased(t, env.Query)
}
