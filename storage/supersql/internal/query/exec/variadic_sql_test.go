package exec_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLVariadicTextUsesRawOutputAndPositions(t *testing.T) {
	graph := sqlGraph(t, `SELECT concat(true,'|','x  '::char(3),NULL,ARRAY[1,2]),concat_ws(':','',NULL,'a',''),format('%2$*1$s|%3$L',4,'é','x''y')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{s("t|x  {1,2}"), s(":a:"), s("   é|'x''y'")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLVariadicFlagKeepsArrayValueSeparateFromSpread(t *testing.T) {
	graph := sqlGraph(t, `SELECT format('%s',ARRAY[1,2]),format('%s',VARIADIC ARRAY[1,2]),concat(ARRAY[1,2]),concat(VARIADIC ARRAY[1,2]),concat_ws('/',VARIADIC '[-1:0]={a,b}'::text[])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25}, [][]model.Value{{s("{1,2}"), s("1"), s("{1,2}"), s("12"), s("a/b")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLVariadicNullAndEmptyArraysRemainDistinct(t *testing.T) {
	graph := sqlGraph(t, `SELECT concat(VARIADIC NULL::text[]),concat(VARIADIC ARRAY[]::text[]),concat_ws(',',VARIADIC NULL::text[]),format('fixed',VARIADIC NULL::text[])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, n := model.StringVal, model.NullVal(model.TypeString)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{n, s(""), n, s("fixed")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLFormatNullVariadicArrayStillRequiresUsedArgument(t *testing.T) {
	graph := sqlGraph(t, `SELECT format('%s',VARIADIC NULL::text[])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "22023" {
		t.Fatalf("NULL variadic array error=%v, want 22023", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLVariadicCallExecutesOneHundredArguments(t *testing.T) {
	graph := sqlGraph(t, "SELECT concat("+strings.Repeat("'x',", 99)+"'x')", nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal(strings.Repeat("x", 100))}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLFormatConsumesPerRowArrayValues(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,format('%s/%s',VARIADIC ARRAY[n,n+1]) FROM generate_series(1,2)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25}, [][]model.Value{{model.Int64Val(1), model.StringVal("1/2")}, {model.Int64Val(2), model.StringVal("2/3")}})
	aggregateProjectionReleased(t, env.Query)
}
