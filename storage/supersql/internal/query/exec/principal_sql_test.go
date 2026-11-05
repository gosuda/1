package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLPrincipalKeywordsIgnoreQuotedColumnShadowing(t *testing.T) {
	graph := sqlGraph(t, `SELECT CURRENT_USER, SESSION_USER, USER, CURRENT_ROLE, "current_user"
		FROM (VALUES ('shadow'::text)) AS t("current_user")`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	name := model.StringVal("embedded")
	assertAnyValueRows(t, portal, []model.TypeID{19, 19, 19, 19, 25}, [][]model.Value{{name, name, name, name, model.StringVal("shadow")}})
	aggregateProjectionReleased(t, env.Query)
}
