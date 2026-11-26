package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRangeUnionKeepsSelectedNumericScale(t *testing.T) {
	graph := sqlGraph(t, `SELECT range_union(numrange(1.0,2.0),numrange(1.00,2.00))::text,range_merge(numrange(1.0,2.0),numrange(1.00,2.00))::text,'{[1.0,2.0),[1.00,2.00)}'::nummultirange::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("[1.00,2.00)"), model.StringVal("[1.00,2.00)"), model.StringVal("{[1.00,2.00)}")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericMultirangeUnionUsesPostgresTieOrder(t *testing.T) {
	graph := sqlGraph(t, `SELECT ('{[0.0,1.0),[3.0,4.0),[6.0,7.0),[9.0,10.0)}'::nummultirange + '{[0.00,1.00),[3.00,4.00),[6.00,7.00),[9.00,10.00)}'::nummultirange)::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("{[0.00,1.00),[3.0,4.0),[6.00,7.00),[9.00,10.00)}")}})
	aggregateProjectionReleased(t, env.Query)
}
