package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLArrayPrependMaximumSubscriptSurvivesConsumers(t *testing.T) {
	graph := sqlGraph(t, `SELECT a::text,(a::bigint[])::text,array_upper(a,1),a[2147483647],array_cat(a,NULL)::text,a=a FROM (SELECT array_prepend(0,'[2147483646:2147483646]={1}'::integer[]) AS a)t`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	text := model.StringVal("[2147483646:2147483647]={0,1}")
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 23, 23, 25, 16}, [][]model.Value{{text, text, model.Int64Val(2147483647), model.Int64Val(1), text, model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLArrayConstructorBoundsRemainStricterThanRepresentation(t *testing.T) {
	for _, tc := range []struct{ name, expression, code string }{
		{"stack", "ARRAY[a]", "54000"},
		{"aggregate", "array_agg(a)", "54000"},
		{"append index overflow", "array_append(a,2)", "22003"},
		{"prepend temporary bounds", "array_prepend(2,a)", "54000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT "+tc.expression+` FROM (SELECT array_prepend(0,'[2147483646:2147483646]={1}'::integer[]) AS a)t`, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
