package exec_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

const pairedStatisticsProjection = `regr_count(y,x),regr_avgx(y,x),regr_avgy(y,x),
	regr_sxx(y,x),regr_syy(y,x),regr_sxy(y,x),regr_slope(y,x),regr_intercept(y,x),
	regr_r2(y,x),corr(y,x),covar_pop(y,x),covar_samp(y,x)`

func TestSQLPairedStatisticsCompletePairs(t *testing.T) {
	f, i := model.Float64Val, model.Int64Val
	null := model.NullVal(model.TypeFloat64)
	for _, scenario := range []struct {
		name, source string
		want         []model.Value
	}{
		{
			name:   "linear pairs ignore either null argument",
			source: `(VALUES(3::float8,1::float8),(5,2),(7,3),(100,NULL),(NULL,200),(NULL,NULL)) v(y,x)`,
			want:   []model.Value{i(3), f(2), f(5), f(2), f(8), f(4), f(2), f(1), f(1), f(1), f(4.0 / 3), f(2)},
		},
		{
			name:   "reversed asymmetric arguments",
			source: `(VALUES(1::float8,3::float8),(2,5),(3,7)) v(y,x)`,
			want:   []model.Value{i(3), f(5), f(2), f(8), f(2), f(4), f(0.5), f(-0.5), f(1), f(1), f(4.0 / 3), f(2)},
		},
		{
			name:   "no rows",
			source: `(VALUES(3::float8,1::float8)) v(y,x) WHERE false`,
			want:   []model.Value{i(0), null, null, null, null, null, null, null, null, null, null, null},
		},
		{
			name:   "no complete pairs",
			source: `(VALUES(NULL::float8,1::float8),(3,NULL),(NULL,NULL)) v(y,x)`,
			want:   []model.Value{i(0), null, null, null, null, null, null, null, null, null, null, null},
		},
		{
			name:   "singleton",
			source: `(VALUES(3::float8,1::float8)) v(y,x)`,
			want:   []model.Value{i(1), f(1), f(3), f(0), f(0), f(0), null, null, null, null, f(0), null},
		},
		{
			name:   "horizontal line",
			source: `(VALUES(5::float8,1::float8),(5,2),(5,3)) v(y,x)`,
			want:   []model.Value{i(3), f(2), f(5), f(2), f(0), f(0), f(0), f(5), f(1), null, f(0), f(0)},
		},
		{
			name:   "vertical line",
			source: `(VALUES(3::float8,2::float8),(5,2),(7,2)) v(y,x)`,
			want:   []model.Value{i(3), f(2), f(5), f(0), f(8), f(0), null, null, null, null, f(0), f(0)},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT "+pairedStatisticsProjection+" FROM "+scenario.source, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != len(scenario.want) {
				t.Fatalf("result shape=%d rows, %d columns, want 1 row, %d columns", batch.Rows(), len(batch.Columns), len(scenario.want))
			}
			for column, want := range scenario.want {
				oid := model.TypeID(701)
				if column == 0 {
					oid = 20
				}
				vector := &batch.Columns[column]
				if vector.Type.ID != oid || vector.Type.Storage != want.Type {
					t.Errorf("column %d type=%+v, want OID %d storage %v", column, vector.Type, oid, want.Type)
				}
				got, _, err := columnar.CopyValue(t.Context(), vector, batch.Lane(0), 1024)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("column %d=%v, error=%v, want %v", column, got, err, want)
				}
			}
			if err := portal.Next(t.Context(), &batch); err != io.EOF {
				t.Fatalf("after scalar result error=%v, want EOF", err)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLPairedStatisticsNonfinitePrecedence(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
	}{
		{
			"nonfinite singleton retains sample null",
			`SELECT regr_count(y,x)=1,regr_avgx(y,x)='Infinity'::float8,regr_avgy(y,x)='NaN'::float8,
				regr_sxx(y,x)='NaN'::float8,regr_syy(y,x)='NaN'::float8,regr_sxy(y,x)='NaN'::float8,
				regr_slope(y,x)='NaN'::float8,regr_intercept(y,x)='NaN'::float8,regr_r2(y,x)='NaN'::float8,
				corr(y,x)='NaN'::float8,covar_pop(y,x)='NaN'::float8,covar_samp(y,x) IS NULL
				FROM (VALUES('NaN'::float8,'Infinity'::float8)) v(y,x)`,
		},
		{
			"horizontal precedence over nonfinite x",
			`SELECT corr(y,x) IS NULL,regr_r2(y,x)=1,regr_slope(y,x)='NaN'::float8,
				regr_intercept(y,x)='NaN'::float8,covar_samp(y,x)='NaN'::float8
				FROM (VALUES(5::float8,'NaN'::float8),(5,2)) v(y,x)`,
		},
		{
			"vertical precedence over nonfinite y",
			`SELECT corr(y,x) IS NULL,regr_r2(y,x) IS NULL,regr_slope(y,x) IS NULL,
				regr_intercept(y,x) IS NULL,covar_samp(y,x)='NaN'::float8
				FROM (VALUES('-Infinity'::float8,2::float8),(5,2)) v(y,x)`,
		},
		{
			"incomplete nonfinite pairs do not contaminate",
			`SELECT regr_count(y,x)=2,covar_samp(y,x)=1,regr_slope(y,x)=2,corr(y,x)=1
				FROM (VALUES(3::float8,1::float8),(5,2),('Infinity',NULL),(NULL,'NaN')) v(y,x)`,
		},
		{
			"finite finalizer products overflow without transition error",
			`SELECT corr(y,x)=0,regr_r2(y,x)='NaN'::float8,regr_slope(y,x)=1
				FROM (VALUES(1e100::float8,1e100::float8),(-1e100,-1e100)) v(y,x)`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			got := rows(t, portal)
			if len(got) != 1 {
				t.Fatalf("rows=%v, want one boolean result row", got)
			}
			for column, value := range got[0] {
				if !reflect.DeepEqual(value, model.BoolVal(true)) {
					t.Errorf("column %d=%v, want true", column, value)
				}
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLPairedStatisticsTupleDistinctAndOrdering(t *testing.T) {
	f, i := model.Float64Val, model.Int64Val
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			"distinct compares both arguments",
			`SELECT regr_count(DISTINCT y,x),regr_count(DISTINCT x,y ORDER BY x,y),
				regr_avgx(DISTINCT y,x ORDER BY y,x),regr_sxx(DISTINCT y,x ORDER BY y,x),
				regr_slope(DISTINCT y,x ORDER BY y,x),covar_samp(DISTINCT y,x ORDER BY y,x)
				FROM (VALUES(3::float8,1::float8),(3,2),(5,3),(3,1),(3,NULL),(NULL,2)) v(y,x)`,
			[][]model.Value{{i(3), i(3), f(2), f(2), f(1), f(1)}},
		},
		{
			"aggregate order controls floating sums",
			`SELECT regr_avgx(y,x ORDER BY k),regr_avgx(y,x ORDER BY k DESC),regr_count(y,x ORDER BY k)
				FROM (VALUES(1,0::float8,1e16::float8),(2,0,-1e16),(3,0,1)) v(k,y,x)`,
			[][]model.Value{{f(1.0 / 3), f(0), i(3)}},
		},
		{
			"filter masks both arguments and order expression",
			`SELECT regr_count(10::float8/x,20::float8/x ORDER BY 100/x) FILTER(WHERE x<>0),
				regr_slope(10::float8/x,20::float8/x ORDER BY 100/x) FILTER(WHERE x<>0),
				regr_count(10::float8/x,20::float8/x ORDER BY 100/x) FILTER(WHERE NULL::boolean),
				covar_pop(10::float8/x,20::float8/x ORDER BY 100/x) FILTER(WHERE false)
				FROM (VALUES(0),(2),(5),(NULL::int4)) v(x)`,
			[][]model.Value{{i(2), f(0.5), i(0), model.NullVal(model.TypeFloat64)}},
		},
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

func TestSQLPairedStatisticsErrorsRemainVisible(t *testing.T) {
	for _, scenario := range []struct {
		name, expression, code string
	}{
		{"first argument despite null second", `regr_count(10::float8/z,NULL::float8)`, "22012"},
		{"second argument despite null first", `covar_pop(NULL::float8,10::float8/z)`, "22012"},
		{"order despite incomplete pair", `regr_count(NULL::float8,1::float8 ORDER BY 100/z)`, "22012"},
		{"window first argument despite null second", `regr_count(10::float8/z,NULL::float8) OVER ()`, "22012"},
		{"window second argument despite null first", `corr(NULL::float8,10::float8/z) OVER ()`, "22012"},
		{"finite paired moment overflow", `regr_avgx(y,x)`, "22003"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT "+scenario.expression+` FROM (VALUES(0,1::float8,1e200::float8),(0,2,-1e200)) v(z,y,x)`, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != scenario.code {
				t.Fatalf("error=%v, want SQLSTATE %s", err, scenario.code)
			}
			if batch.Rows() != 0 {
				t.Fatalf("failed aggregate returned %d rows", batch.Rows())
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLPairedStatisticsGroupedSpill(t *testing.T) {
	var values []string
	var want [][]model.Value
	for group := range 16 {
		values = append(values, fmt.Sprintf("(%d,3::float8,1::float8),(%d,3,2),(%d,5,3),(%d,3,1),(%d,NULL,200)", group, group, group, group, group))
		want = append(want, []model.Value{model.Int64Val(int64(group)), model.Int64Val(3), model.Float64Val(1), model.Float64Val(1)})
	}
	graph := sqlGraph(t, `SELECT g,regr_count(DISTINCT y,x ORDER BY y,x),
		regr_slope(DISTINCT y,x ORDER BY y,x),covar_samp(DISTINCT y,x ORDER BY y,x)
		FROM (VALUES`+strings.Join(values, ",")+`) v(g,y,x) GROUP BY g ORDER BY g`, nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 64 << 10
			if spill {
				env.Aggregate.GroupBytes = 64 << 10
				env.Aggregate.PartitionMemoryBytes = 1024
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("grouped paired statistics=%v, want %v", got, want)
			}
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLPairedStatisticsGroupingSetsReplay(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,regr_count(10::float8/x,20::float8/x ORDER BY 100/x) FILTER(WHERE x<>0),
		regr_slope(DISTINCT 10::float8/x,20::float8/x ORDER BY 10::float8/x,20::float8/x) FILTER(WHERE x<>0),GROUPING(g)
		FROM (VALUES(1,0),(1,2),(1,5),(2,0),(2,2),(2,5)) v(g,x)
		GROUP BY GROUPING SETS ((g),(),(g),())`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Aggregate.PartitionMemoryBytes = 1024
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, f := model.Int64Val, model.Float64Val
	group1 := []model.Value{i(1), i(2), f(0.5), i(0)}
	group2 := []model.Value{i(2), i(2), f(0.5), i(0)}
	total := []model.Value{model.NullVal(model.TypeInt64), i(4), f(0.5), i(1)}
	assertGroupingSetsBag(t, rows(t, portal), [][]model.Value{group1, group2, total, group1, group2, total})
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("grouping-set paired input did not replay from spill")
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLPairedStatisticsWindows(t *testing.T) {
	i, f, b := model.Int64Val, model.Float64Val, model.BoolVal
	null := model.NullVal(model.TypeFloat64)
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			"growing frame ignores incomplete pairs",
			`SELECT regr_count(y,x) OVER w,regr_avgx(y,x) OVER w,regr_avgy(y,x) OVER w,
				regr_sxx(y,x) OVER w,regr_syy(y,x) OVER w,regr_sxy(y,x) OVER w,
				regr_slope(y,x) OVER w,regr_intercept(y,x) OVER w,regr_r2(y,x) OVER w,corr(y,x) OVER w,
				covar_pop(y,x) OVER w,covar_samp(y,x) OVER w
				FROM (VALUES(1,3::float8,1::float8),(2,100,NULL),(3,NULL,200),(4,5,2),(5,7,3)) v(k,y,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) ORDER BY k`,
			[][]model.Value{
				{i(1), f(1), f(3), f(0), f(0), f(0), null, null, null, null, f(0), null},
				{i(1), f(1), f(3), f(0), f(0), f(0), null, null, null, null, f(0), null},
				{i(1), f(1), f(3), f(0), f(0), f(0), null, null, null, null, f(0), null},
				{i(2), f(1.5), f(4), f(0.5), f(2), f(1), f(2), f(1), f(1), f(1), f(0.5), f(1)},
				{i(3), f(2), f(5), f(2), f(8), f(4), f(2), f(1), f(1), f(1), f(4.0 / 3), f(2)},
			},
		},
		{
			"moving frame resets per partition",
			`SELECT regr_count(y,x) OVER w,regr_slope(y,x) OVER w,regr_intercept(y,x) OVER w,covar_samp(y,x) OVER w
				FROM (VALUES(1,1,3::float8,1::float8),(1,2,5,2),(1,3,7,3),(2,1,9,4)) v(g,k,y,x)
				WINDOW w AS (PARTITION BY g ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY g,k`,
			[][]model.Value{{i(1), null, null, null}, {i(2), f(2), f(1), f(1)}, {i(2), f(2), f(1), f(1)}, {i(1), null, null, null}},
		},
		{
			"excluded current row changes covariance",
			`SELECT regr_count(y,x) OVER w,covar_samp(y,x) OVER w,regr_slope(y,x) OVER w
				FROM (VALUES(1,3::float8,1::float8),(2,5,2),(3,7,3)) v(k,y,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) ORDER BY k`,
			[][]model.Value{{i(2), f(1), f(2)}, {i(2), f(4), f(2)}, {i(2), f(1), f(2)}},
		},
		{
			"following frame becomes empty",
			`SELECT regr_count(y,x) OVER w,covar_pop(y,x) OVER w,covar_samp(y,x) OVER w
				FROM (VALUES(1,3::float8,1::float8),(2,5,2),(3,7,3)) v(k,y,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING) ORDER BY k`,
			[][]model.Value{{i(1), f(0), null}, {i(1), f(0), null}, {i(0), null, null}},
		},
		{
			"nonfinite pair leaves moving frame",
			`SELECT regr_count(y,x) OVER w,covar_pop(y,x) OVER w='NaN'::float8,
				regr_slope(y,x) OVER w=CASE WHEN k=2 THEN 'NaN'::float8 ELSE 2::float8 END
				FROM (VALUES(1,'NaN'::float8,1::float8),(2,5,2),(3,7,3)) v(k,y,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY k`,
			[][]model.Value{{i(1), b(true), model.NullVal(model.TypeBoolean)}, {i(2), b(true), b(true)}, {i(2), b(false), b(true)}},
		},
		{
			"filter masks both growing frame arguments",
			`SELECT regr_count(10::float8/x,20::float8/x) FILTER(WHERE x<>0) OVER w,
				regr_slope(10::float8/x,20::float8/x) FILTER(WHERE x<>0) OVER w
				FROM (VALUES(1,0),(2,2),(3,NULL::int4),(4,5)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) ORDER BY k`,
			[][]model.Value{{i(0), null}, {i(1), null}, {i(1), null}, {i(2), f(0.5)}},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Window.WorkBytes = 32 << 20
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("window paired statistics=%v, want %v", got, scenario.want)
			}
			if env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("paired window partition did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLPairedStatisticsActiveSpillCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      model.Value
	}{
		{"grouping replay", `SELECT covar_pop(y,x) FROM (VALUES(1,3::float8,1::float8),(2,5,2)) v(g,y,x) GROUP BY GROUPING SETS ((),(g))`, model.Float64Val(0.5)},
		{"window partition", `SELECT covar_pop(y,x) OVER (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM (VALUES(1,3::float8,1::float8),(2,5,2),(3,7,3)) v(k,y,x)`, model.Float64Val(0)},
	} {
		for _, ending := range []string{"early close", "cancel"} {
			t.Run(scenario.name+"/"+ending, func(t *testing.T) {
				graph := sqlGraph(t, scenario.sql, nil)
				env, dir := groupingSetsEnvironment(t, true)
				env.Window.PartitionMemoryBytes = 0
				env.Limits.BatchRows = 1
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				portal, err := exec.Open(ctx, graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				var batch columnar.Batch
				if err := portal.Next(ctx, &batch); err != nil {
					t.Fatal(err)
				}
				if batch.Rows() != 1 {
					t.Fatalf("first batch rows=%d, want 1", batch.Rows())
				}
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(0), 1024)
				if err != nil || !reflect.DeepEqual(value, scenario.want) {
					t.Fatalf("first statistic=%v, error=%v, want %v", value, err, scenario.want)
				}
				if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
					t.Fatalf("no active spill before %s: %+v", ending, usage)
				}
				if ending == "early close" {
					closePortal(t, portal)
				} else {
					cancel()
					if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled paired statistics error=%v, want context cancellation", err)
					}
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}
