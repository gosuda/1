package exec_test

import (
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLParseIdentifierUsesDefaultAndNamedArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT parse_ident('S."Mixed"')::text,parse_ident(strict=>false,str=>'Function(a,b)')::text,parse_ident('"'||repeat('x',70)||'"')::name[]::text`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("{s,Mixed}"), model.StringVal("{function}"), model.StringVal("{" + strings.Repeat("x", 63) + "}")}})
	aggregateProjectionReleased(t, env.Query)
}
