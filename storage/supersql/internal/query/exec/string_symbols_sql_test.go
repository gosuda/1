package exec_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLStringSearchReplacementAndTranslation(t *testing.T) {
	graph := sqlGraph(t, `SELECT position('β' IN s),strpos(s,'β'),replace(s,'β','XY'),translate(s,'αβ','az'),split_part(s,'β',-1)
 FROM (VALUES('αββ尾'::text)) t(s)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25, 25, 25}, [][]model.Value{{i(2), i(2), s("αXYXY尾"), s("azz尾"), s("尾")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLStringPaddingAndTrimGrammar(t *testing.T) {
	graph := sqlGraph(t, `SELECT lpad(s,5,'界🙂'),rpad(s,5,'界🙂'),trim(BOTH '界' FROM s),trim(LEADING '界' FROM s),
 trim(TRAILING '界' FROM s),lpad(s,-1),rpad(s,5,'') FROM (VALUES('界a界'::text)) t(s)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25, 25, 25}, [][]model.Value{{s("界🙂界a界"), s("界a界界🙂"), s("a"), s("a界"), s("界a"), s(""), s("界a界")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLStringCasingRepetitionAndReverse(t *testing.T) {
	graph := sqlGraph(t, `SELECT lower(s),upper(s),initcap(s),repeat('界',n),reverse('A界🙂')
 FROM (VALUES('hELLO-éCOLE 42FOO'::text,2)) t(s,n)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25, 25}, [][]model.Value{{s("hello-école 42foo"), s("HELLO-éCOLE 42FOO"), s("Hello-éCole 42foo"), s("界界"), s("🙂界A")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLByteaStringSymbolsKeepByteSemantics(t *testing.T) {
	graph := sqlGraph(t, `SELECT position('\x61'::bytea IN x),trim(BOTH '\x00ff'::bytea FROM x),
 ltrim(x,'\x00ff'::bytea),rtrim(x,'\x00ff'::bytea),reverse(x)
 FROM (VALUES('\x00ff610100'::bytea)) t(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	b := model.BytesVal
	assertAnyValueRows(t, portal, []model.TypeID{23, 17, 17, 17, 17}, [][]model.Value{{model.Int64Val(3), b([]byte{'a', 1}), b([]byte{'a', 1, 0}), b([]byte{0, 255, 'a', 1}), b([]byte{0, 1, 'a', 255, 0})}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLSplitPartZeroIndexRespectsCaseMask(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN n=0 THEN '' ELSE split_part('a,b,c',',',n) END
 FROM (VALUES(1,0),(2,1),(3,-1)) t(k,n) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{s("")}, {s("a")}, {s("c")}})
	aggregateProjectionReleased(t, env.Query)
}
