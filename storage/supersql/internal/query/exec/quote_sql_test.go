package exec_test

import (
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLQuoteIdentifiersAndEscapedLiterals(t *testing.T) {
	graph := sqlGraph(t, `SELECT quote_ident('select'),quote_ident('abort'),quote_ident('a$b'),quote_ident('界'),quote_literal(E'x\\y''z'),quote_nullable(NULL::text),quote_literal(NULL::text)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25, 25, 25}, [][]model.Value{{s(`"select"`), s("abort"), s(`"a$b"`), s(`"界"`), s(`E'x\\y''z'`), s("NULL"), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLPolymorphicQuoteUsesTextCastRatherThanRawOutput(t *testing.T) {
	graph := sqlGraph(t, `SELECT quote_literal(true),quote_literal('x  '::char(3)),quote_nullable(NULL::integer),quote_literal(ARRAY[1,NULL,2]),quote_literal(current_user)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25}, [][]model.Value{{s("'true'"), s("'x'"), s("NULL"), s("'{1,NULL,2}'"), s("'embedded'")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNameClippingAndArrayOutput(t *testing.T) {
	graph := sqlGraph(t, `SELECT (repeat('x',60)||'界z')::name::text,(repeat('x',61)||'界')::name::text,(repeat('x',62)||' z')::char(65)::name::text,array_to_string(ARRAY['ABC'::name,NULL::name,'x '::name],'|','null')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{s(strings.Repeat("x", 60) + "界"), s(strings.Repeat("x", 61)), s(strings.Repeat("x", 62)), s("ABC|null|x ")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLQuoteRetainsCaseMasking(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN n=0 THEN quote_nullable(NULL::integer) ELSE quote_literal(1/n) END FROM generate_series(0,1)t(n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("NULL")}, {model.StringVal("'1'")}})
	aggregateProjectionReleased(t, env.Query)
}
