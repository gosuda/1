package exec_test

import (
	"errors"
	"testing"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLLikeILikeAndEscape(t *testing.T) {
	graph := sqlGraph(t, `SELECT x LIKE '%_',x ILIKE 'a%',x NOT ILIKE 'a%',
 x LIKE 'A#_b#%' ESCAPE '#',x LIKE '%é_',x LIKE '%' ESCAPE ''
 FROM (VALUES(1,'A_b%'::text),(2,'ab'),(3,'éZ'),(4,NULL)) t(k,x) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes, no, n := model.BoolVal(true), model.BoolVal(false), model.NullVal(model.TypeBoolean)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16, 16}, [][]model.Value{
		{yes, yes, no, yes, no, yes}, {yes, yes, no, no, no, yes}, {yes, no, yes, no, yes, yes}, {n, n, n, n, n, n},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLikeCharacterPaddingByteaAndUnicodeEscape(t *testing.T) {
	graph := sqlGraph(t, `SELECT 'a'::char(3) LIKE 'a','a'::char(3) LIKE 'a__',
 'a'::char(3) ILIKE 'A__','É' COLLATE "C" ILIKE 'é',
 '\x0061ff'::bytea LIKE '\x005fff'::bytea,
 'a_b' LIKE 'a§_b' ESCAPE '§','a\b' LIKE 'a\b' ESCAPE ''`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	yes, no := model.BoolVal(true), model.BoolVal(false)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 16, 16, 16, 16}, [][]model.Value{{no, yes, yes, no, yes, yes, yes}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLikeMasksUnreachedPatternErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN k=0 THEN true ELSE x LIKE E'\\' END
 FROM (VALUES(0,'text'::text),(1,'')) t(k,x) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(false)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLikeReportsReachedInvalidEscape(t *testing.T) {
	graph := sqlGraph(t, `SELECT x LIKE E'\\' FROM (VALUES('x'::text)) t(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	defer batch.Reset()
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22025" {
		t.Fatalf("LIKE escape error=%v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLikeAcceptsNameTypedPrincipal(t *testing.T) {
	graph := sqlGraph(t, `SELECT CURRENT_USER LIKE 'embed%', CURRENT_USER ILIKE 'EMBED%'`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16}, [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
