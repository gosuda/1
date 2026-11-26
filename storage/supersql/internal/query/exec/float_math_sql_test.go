package exec_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLFloatingMathUsesTypedScalarKernels(t *testing.T) {
	graph := sqlGraph(t, `SELECT sqrt(4), cbrt(-8), power(-2,3),
		exp(0), ln(1), log(100), pi()`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	f := model.Float64Val
	assertAnyValueRows(t, portal, []model.TypeID{701, 701, 701, 701, 701, 701, 701}, [][]model.Value{{f(2), f(-2), f(-8), f(1), f(0), f(2), f(math.Pi)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLFloatingMathMasksInvalidArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT x,
		CASE WHEN x<0 THEN NULL::float8 ELSE sqrt(x::float8) END,
		CASE WHEN x=0 THEN NULL::float8 ELSE ln((x*x)::float8) END
		FROM (VALUES(-1),(0),(1)) t(x) ORDER BY x`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, f := model.Int64Val, model.Float64Val
	null := model.NullVal(model.TypeFloat64)
	assertAnyValueRows(t, portal, []model.TypeID{23, 701, 701}, [][]model.Value{{i(-1), null, f(0)}, {i(0), f(0), null}, {i(1), f(1), f(0)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLDegreeAndHyperbolicExactLandmarks(t *testing.T) {
	graph := sqlGraph(t, `SELECT sind(a),cosd(b),tand(c),atan2d(d,e),cotd(f),acosd(e),tanh(g),acosh(d)
		FROM (VALUES(30::float8,60::float8,45::float8,1::float8,(-1)::float8,90::float8,'Infinity'::float8))
		t(a,b,c,d,e,f,g)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	f := model.Float64Val
	assertAnyValueRows(t, portal, []model.TypeID{701, 701, 701, 701, 701, 701, 701, 701}, [][]model.Value{{f(.5), f(.5), f(1), f(135), f(0), f(180), f(1), f(0)}})
	aggregateProjectionReleased(t, env.Query)
}
