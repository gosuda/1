package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRegexpScalarsAndSimilarOperator(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
		oids []model.TypeID
		want [][]model.Value
	}{
		{"leftmost longest substring", `SELECT regexp_substr('aa','a|aa')`, []model.TypeID{25}, [][]model.Value{{model.StringVal("aa")}}},
		{"replacement captures", `SELECT regexp_replace('abc123','([a-z]+)([0-9]+)','\2-\1-\&')`, []model.TypeID{25}, [][]model.Value{{model.StringVal("123-abc-abc123")}}},
		{"unicode character position", `SELECT regexp_instr('é日x','日')`, []model.TypeID{23}, [][]model.Value{{model.Int64Val(2)}}},
		{"similar operator", `SELECT 'foobar' SIMILAR TO 'foo%'`, []model.TypeID{16}, [][]model.Value{{model.BoolVal(true)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := sqlGraph(t, test.sql, nil)
			environment := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, environment)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, test.oids, test.want)
			aggregateProjectionReleased(t, environment.Query)
		})
	}
}

func TestSQLRegexpRowSourcesStreamWithOrdinality(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"matches", `SELECT m[1],n FROM regexp_matches('ab ac','a(.)','g') WITH ORDINALITY AS r(m,n)`, [][]model.Value{{model.StringVal("b"), model.Int64Val(1)}, {model.StringVal("c"), model.Int64Val(2)}}},
		{"split", `SELECT x,n FROM regexp_split_to_table('é::日','::') WITH ORDINALITY AS r(x,n)`, [][]model.Value{{model.StringVal("é"), model.Int64Val(1)}, {model.StringVal("日"), model.Int64Val(2)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := sqlGraph(t, test.sql, nil)
			environment := applyEnvironment(t)
			environment.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, environment)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{25, 20}, test.want)
			aggregateProjectionReleased(t, environment.Query)
		})
	}
}
