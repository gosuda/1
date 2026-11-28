package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayTrimResetsAllBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT trim_array(a,n)::text,array_ndims(trim_array(a,n))
 FROM (VALUES(0),(1),(2)) t(n) CROSS JOIN (VALUES('[-2:-1][4:5]={{1,2},{3,4}}'::integer[])) v(a) ORDER BY n`, nil)
	env := configureSetWindowEnvironment(statefulEnvironment(t))
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, i := model.StringVal, model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{25, 23}, [][]model.Value{{s("{{1,2},{3,4}}"), i(2)}, {s("{{1,2}}"), i(2)}, {s("{}"), model.NullVal(model.TypeInt64)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayTrimPreservesNullAndMaskedErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN n<0 THEN '' ELSE array_to_string(trim_array(ARRAY['a','b','c'],n),',') END,trim_array(NULL::integer[],n)
 FROM (VALUES(-1),(0),(2),(3)) t(n) ORDER BY n`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, n := model.StringVal, model.NullVal(model.TypeArray)
	assertAnyValueRows(t, portal, []model.TypeID{25, 1007}, [][]model.Value{{s(""), n}, {s("a,b,c"), n}, {s("a"), n}, {s(""), n}})
	aggregateProjectionReleased(t, env.Query)
}
