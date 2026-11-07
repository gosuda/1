package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLDateOutputAndISOFields(t *testing.T) {
	graph := sqlGraph(t, `SELECT make_date(2021,1,1)::text,extract(isoyear FROM make_date(2021,1,1)),extract(week FROM make_date(2021,1,1)),date_part('hour',make_date(2021,1,1)),extract(year FROM make_date(-1,1,1))`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 1700, 1700, 701, 1700}, [][]model.Value{{model.StringVal("2021-01-01"), {Type: model.TypeNumeric, StrVal: "2020"}, {Type: model.TypeNumeric, StrVal: "53"}, model.Float64Val(0), {Type: model.TypeNumeric, StrVal: "-1"}}})
	aggregateProjectionReleased(t, env.Query)
}

func TestPreparedDateOutputRejectsChangedOrder(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, DateStyle: builtin.DateSQL, DateOrder: builtin.DateDMY}
	prepared := prepareSemanticSQL(t, `SELECT make_date(2000,1,2)::text`, nil, options)
	environment := statefulEnvironment(t)
	environment.Builtin = options
	portal, err := prepared.Open(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("02/01/2000")}})
	closePortal(t, portal)
	changed := statefulEnvironment(t)
	changed.Builtin = options
	changed.Builtin.DateOrder = builtin.DateMDY
	_, err = prepared.Open(t.Context(), changed)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "0A000" {
		t.Fatalf("prepared DateOrder mismatch=%v; want 0A000", err)
	}
	aggregateProjectionReleased(t, changed.Query)
}
