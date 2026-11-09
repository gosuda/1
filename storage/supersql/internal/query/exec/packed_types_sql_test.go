package exec_test

import (
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntegerBitCastsUseDestinationLengthBeforeConversion(t *testing.T) {
	graph := sqlGraph(t, `SELECT (x::bit(4))::text, (x::bit(65))::text, (ARRAY[x]::bit(4)[])::text FROM (VALUES (1::bigint), (-1::bigint)) AS v(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{
		{model.StringVal("0001"), model.StringVal(strings.Repeat("0", 64) + "1"), model.StringVal("{0001}")},
		{model.StringVal("1111"), model.StringVal(strings.Repeat("1", 65)), model.StringVal("{1111}")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLPackedTypesOrderByLogicalPrefixes(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
		want []string
	}{
		{"network", `SELECT x::text FROM (VALUES ('10.0.0.0/9'::inet), ('10.0.0.1/8'::inet), ('10.0.0.1/32'::inet), ('::/0'::inet)) AS v(x) ORDER BY v.x`, []string{"10.0.0.1/8", "10.0.0.0/9", "10.0.0.1/32", "::/0"}},
		{"cidr", `SELECT x::text FROM (VALUES ('10.0.0.0/9'::cidr), ('10.0.0.0/8'::cidr), ('::/0'::cidr)) AS v(x) ORDER BY v.x`, []string{"10.0.0.0/8", "10.0.0.0/9", "::/0"}},
		{"bits", `SELECT x::text FROM (VALUES (B'1'::varbit), (B'01'::varbit), (B'10'::varbit), (B''::varbit), (B'10000000'::varbit), (B'100000000'::varbit)) AS v(x) ORDER BY v.x`, []string{"", "01", "1", "10", "10000000", "100000000"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := make([][]model.Value, len(scenario.want))
			for i, text := range scenario.want {
				want[i] = []model.Value{model.StringVal(text)}
			}
			assertAnyValueRows(t, portal, []model.TypeID{25}, want)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
