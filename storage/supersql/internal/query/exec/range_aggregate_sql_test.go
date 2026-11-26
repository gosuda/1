package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRangeAggregatesDistinguishEmptyFromFilteredInput(t *testing.T) {
	graph := sqlGraph(t, `SELECT range_agg(v)::text,range_intersect_agg(v)::text,(range_agg(v) FILTER(WHERE false))::text FROM (VALUES ('empty'::int4range),(NULL::int4range))t(v)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("{}"), model.StringVal("empty"), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRangeWindowInvalidatesFinalizedUnionState(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,(range_agg(v) OVER(ORDER BY n ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW))::text FROM (VALUES (1,'[1,2)'::int4range),(2,'[2,3)'::int4range),(3,'[5,6)'::int4range))t(n,v) ORDER BY n`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25}, [][]model.Value{{model.Int64Val(1), model.StringVal("{[1,2)}")}, {model.Int64Val(2), model.StringVal("{[1,3)}")}, {model.Int64Val(3), model.StringVal("{[1,3),[5,6)}")}})
	aggregateProjectionReleased(t, env.Query)
}
