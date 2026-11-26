package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntervalAggregateWindowsResetFiniteState(t *testing.T) {
	graph := sqlGraph(t, `SELECT sum(t) OVER (ORDER BY n ROWS 1 PRECEDING),avg(t) OVER (ORDER BY n ROWS 1 PRECEDING) FROM (VALUES(1,interval '1 month'),(2,interval '1 day'),(3,NULL::interval),(4,interval '-1 day')) v(n,t) ORDER BY n`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	v := func(m, d int32, u int64) model.Value {
		return model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: m, Days: d, Microseconds: u}}
	}
	assertAnyValueRows(t, portal, []model.TypeID{1186, 1186}, [][]model.Value{{v(1, 0, 0), v(1, 0, 0)}, {v(1, 1, 0), v(0, 15, 43200000000)}, {v(0, 1, 0), v(0, 1, 0)}, {v(0, -1, 0), v(0, -1, 0)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLIntervalAggregateDistinctAndNullFilter(t *testing.T) {
	graph := sqlGraph(t, `SELECT extract(epoch FROM sum(DISTINCT t)),extract(epoch FROM avg(t)),sum(t) FILTER (WHERE false),extract(epoch FROM min(t)),extract(epoch FROM max(t)) FROM (VALUES(interval '1 month'),(interval '30 days'),(NULL::interval)) v(t)`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	numeric := model.Value{Type: model.TypeNumeric, StrVal: "2592000.000000"}
	assertAnyValueRows(t, portal, []model.TypeID{1700, 1700, 1186, 1700, 1700}, [][]model.Value{{numeric, numeric, model.NullVal(model.TypeInterval), numeric, numeric}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLIntervalAggregateFailureReleasesPortal(t *testing.T) {
	for _, sql := range []string{
		`SELECT sum(t) FROM (VALUES(interval 'infinity'),(interval '-infinity')) v(t)`,
		`SELECT avg(t) FROM (VALUES(interval 'infinity'),(interval '2147483647 months'),(interval '1 month')) v(t)`,
	} {
		graph := sqlGraph(t, sql, nil)
		env := setWindowEnvironment(t)
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		var batch columnar.Batch
		err = portal.Next(t.Context(), &batch)
		var sqlError *model.Error
		if !errors.As(err, &sqlError) || sqlError.Code != "22008" {
			t.Fatalf("interval aggregate error: %v", err)
		}
		aggregateProjectionReleased(t, env.Query)
	}
}
