package exec_test

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLFloatingStatistics(t *testing.T) {
	f, b := model.Float64Val, model.BoolVal
	null := model.NullVal(model.TypeFloat64)
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"sample and population", `SELECT var_pop(x),var_samp(x),variance(x),stddev_pop(x),stddev_samp(x),stddev(x) FROM (VALUES(0::float8),(2::float8),(NULL::float8)) v(x)`, [][]model.Value{{f(1), f(2), f(2), f(1), f(math.Sqrt(2)), f(math.Sqrt(2))}}},
		{"real uses double moments", `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x) FROM (VALUES(0::real),(2::real)) v(x)`, [][]model.Value{{f(1), f(2), f(1), f(math.Sqrt(2))}}},
		{"large offset", `SELECT var_pop(x),var_samp(x) FROM (VALUES(1000000000001::float8),(1000000000002::float8),(1000000000003::float8)) v(x)`, [][]model.Value{{f(2.0 / 3), f(1)}}},
		{"singleton", `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x) FROM (VALUES(4::float8)) v(x)`, [][]model.Value{{f(0), null, f(0), null}}},
		{"all null", `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x) FROM (VALUES(NULL::float8)) v(x)`, [][]model.Value{{null, null, null, null}}},
		{"empty", `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x) FROM (VALUES(4::float8)) v(x) WHERE false`, [][]model.Value{{null, null, null, null}}},
		{"singleton infinity", `SELECT var_pop(x)='NaN'::float8,var_samp(x) IS NULL,stddev_pop(x)='NaN'::float8,stddev_samp(x) IS NULL FROM (VALUES('Infinity'::float8)) v(x)`, [][]model.Value{{b(true), b(true), b(true), b(true)}}},
		{"nonfinite moments", `SELECT var_pop(x)='NaN'::float8,var_samp(x)='NaN'::float8,stddev_pop(x)='NaN'::float8,stddev_samp(x)='NaN'::float8 FROM (VALUES('-Infinity'::float8),(2::float8)) v(x)`, [][]model.Value{{b(true), b(true), b(true), b(true)}}},
		{"nan singleton", `SELECT var_pop(x)='NaN'::float8,var_samp(x) IS NULL FROM (VALUES('NaN'::float8)) v(x)`, [][]model.Value{{b(true), b(true)}}},
		{"distinct ordering", `SELECT var_pop(DISTINCT x ORDER BY x),var_samp(DISTINCT x ORDER BY x DESC) FROM (VALUES(0::float8),(2::float8),(2::float8)) v(x)`, [][]model.Value{{f(1), f(2)}}},
		{"filter masks expression", `SELECT var_pop(10::float8/x ORDER BY 100/x) FILTER(WHERE x<>0),stddev_pop(10::float8/x) FILTER(WHERE x<>0) FROM (VALUES(0),(2),(5)) v(x)`, [][]model.Value{{f(2.25), f(1.5)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v, want %v", got, scenario.want)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLFloatingStatisticsMovingWindows(t *testing.T) {
	graph := sqlGraph(t, `SELECT var_pop(x) OVER w,var_samp(x) OVER w,stddev_pop(x) OVER w FROM (VALUES(1,1,0::float8),(1,2,2::float8),(1,3,4::float8),(2,1,8::float8)) v(g,k,x) WINDOW w AS (PARTITION BY g ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY g,k`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Window.PartitionMemoryBytes = 0
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	f, null := model.Float64Val, model.NullVal(model.TypeFloat64)
	want := [][]model.Value{{f(0), null, f(0)}, {f(1), f(2), f(1)}, {f(1), f(2), f(1)}, {f(0), null, f(0)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("moving moments=%v, want %v", got, want)
	}
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("window did not spill")
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLFloatingStatisticsFiniteOverflow(t *testing.T) {
	for _, function := range []string{"avg", "var_pop", "stddev_samp"} {
		t.Run(function, func(t *testing.T) {
			graph := sqlGraph(t, fmt.Sprintf(`SELECT %s(x) FROM (VALUES(1e200::float8),(-1e200::float8)) v(x)`, function), nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22003" {
				t.Fatalf("error=%v, want finite moment overflow", err)
			}
			if batch.Rows() != 0 {
				t.Fatalf("overflow returned %d rows", batch.Rows())
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLFloatingStatisticsSpillPartitions(t *testing.T) {
	source := "(VALUES "
	for group := range 9 {
		if group > 0 {
			source += ","
		}
		source += fmt.Sprintf("(%d,0::float8),(%d,2::float8),(%d,4::float8)", group, group, group)
	}
	source += ") v(g,x)"
	graph := sqlGraph(t, "SELECT g,var_samp(x),stddev_pop(x) FROM "+source+" GROUP BY g ORDER BY g", nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprint(spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, false)
			if spill {
				env.Aggregate.GroupBytes = 16 << 10
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			got := rows(t, portal)
			if len(got) != 9 {
				t.Fatalf("groups=%d, want 9", len(got))
			}
			for group, row := range got {
				want := []model.Value{model.Int64Val(int64(group)), model.Float64Val(4), model.Float64Val(math.Sqrt(8.0 / 3))}
				if !reflect.DeepEqual(row, want) {
					t.Fatalf("group %d=%v, want %v", group, row, want)
				}
			}
			if spill && env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("constrained statistical aggregation did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}
