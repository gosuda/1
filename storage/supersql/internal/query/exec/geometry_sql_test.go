package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLGeometryConsumersAcceptInternalConstructorValues(t *testing.T) {
	graph := sqlGraph(t, `SELECT radius(circle(point(0,0),-1::float8))=-1, width(box '(1,1),(0,0)' # box '(2,1),(1.0000005,0)')<0, box_add(box '(0,0),(-Infinity,0)',point('Infinity'::float8,0))::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 25}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true), model.StringVal("(Infinity,0),(NaN,0)")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLPointNaNComparisonRequiresExactOtherCoordinates(t *testing.T) {
	graph := sqlGraph(t, `SELECT point '(NaN,0)' ~= point '(NaN,0.0000005)', point '(0,0)' ~= point '(0,0.0000005)'`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(false), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLPolygonAxesRetainTrigonometricLowBits(t *testing.T) {
	graph := sqlGraph(t, `SELECT encode(substring(poly_send(polygon(circle(point(0,0),2))) from 109 for 8),'hex'),encode(float8send(2*sin(pi())),'hex')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{model.StringVal("3cb1a62633145c07"), model.StringVal("3cb1a62633145c07")}})
	aggregateProjectionReleased(t, env.Query)
}
