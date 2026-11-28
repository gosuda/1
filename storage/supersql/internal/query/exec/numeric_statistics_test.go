package exec_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLNumericStatisticsPrecision(t *testing.T) {
	n := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			name: "population and sample output scales",
			sql: `SELECT var_pop(x),var_samp(x),variance(x),stddev_pop(x),stddev_samp(x),stddev(x)
				FROM (VALUES(0::numeric),(2::numeric),(NULL::numeric)) v(x)`,
			want: [][]model.Value{{n("1.00000000000000000000"), n("2.0000000000000000"), n("2.0000000000000000"), n("1.00000000000000000000"), n("1.4142135623730950"), n("1.4142135623730950")}},
		},
		{
			name: "unit differences beyond float precision",
			sql: `SELECT var_pop(x),var_samp(x),stddev_samp(x)
				FROM (VALUES(9007199254740992::numeric),(9007199254740993::numeric),(9007199254740994::numeric)) v(x)`,
			want: [][]model.Value{{n("0.66666666666666666667"), n("1.00000000000000000000"), n("1.00000000000000000000")}},
		},
		{
			name: "input scale survives squared moments",
			sql: `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x)
				FROM (VALUES(1.000000000000::numeric),(3::numeric)) v(x)`,
			want: [][]model.Value{{n("1.000000000000000000000000"), n("2.000000000000000000000000"), n("1.000000000000000000000000"), n("1.414213562373095048801689")}},
		},
		{
			name: "fractional scale alignment",
			sql:  `SELECT var_pop(x),stddev_pop(x) FROM (VALUES(0.10::numeric),(0.3000::numeric)) v(x)`,
			want: [][]model.Value{{n("0.01000000000000000000"), n("0.10000000000000000000")}},
		},
		{
			name: "squared values exceed float range",
			sql:  `SELECT var_pop(x),stddev_pop(x) FROM (VALUES(1e200::numeric),(-1e200::numeric)) v(x)`,
			want: [][]model.Value{{n("1" + strings.Repeat("0", 400)), n("1" + strings.Repeat("0", 200))}},
		},
		{
			name: "squared differences do not underflow",
			sql: `SELECT var_pop(x)=1e-400::numeric,stddev_pop(x)=1e-200::numeric
				FROM (VALUES(1e-200::numeric),(3e-200::numeric)) v(x)`,
			want: [][]model.Value{{model.BoolVal(true), model.BoolVal(true)}},
		},
		{
			name: "zero moments have zero scale",
			sql: `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x)
				FROM (VALUES(123456789012345678901234567890.0000::numeric),(123456789012345678901234567890::numeric)) v(x)`,
			want: [][]model.Value{{n("0"), n("0"), n("0"), n("0")}},
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
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("numeric statistics=%v, want %v", got, scenario.want)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLIntegerStatisticsFamilies(t *testing.T) {
	for _, scenario := range []struct {
		typ         string
		first, last int64
	}{
		{"int2", -32768, -32764},
		{"int4", 2147483643, 2147483647},
		{"int8", 9223372036854775803, 9223372036854775807},
	} {
		t.Run(scenario.typ, func(t *testing.T) {
			sql := fmt.Sprintf(`SELECT var_pop(x)=4::numeric,var_samp(x)=8::numeric,variance(x)=8::numeric,
				stddev_pop(x)=2::numeric,stddev_samp(x)=2.8284271247461901::numeric,stddev(x)=2.8284271247461901::numeric
				FROM (VALUES((%d)::%s),((%d)::%s),(NULL::%s)) v(x)`, scenario.first, scenario.typ, scenario.last, scenario.typ, scenario.typ)
			graph := sqlGraph(t, sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			b := model.BoolVal(true)
			want := [][]model.Value{{b, b, b, b, b, b}}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s statistics=%v, want %v", scenario.typ, got, want)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLNumericStatisticsCardinalityBeforeNonfinite(t *testing.T) {
	null := model.NullVal(model.TypeNumeric)
	n := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	for _, scenario := range []struct {
		name, source string
		want         []model.Value
	}{
		{"empty", `(VALUES(1::numeric)) v(x) WHERE false`, []model.Value{null, null, null, null}},
		{"all null", `(VALUES(NULL::numeric),(NULL::numeric)) v(x)`, []model.Value{null, null, null, null}},
		{"finite singleton", `(VALUES(9.00::numeric),(NULL::numeric)) v(x)`, []model.Value{n("0"), null, n("0"), null}},
		{"nan singleton", `(VALUES('NaN'::numeric),(NULL::numeric)) v(x)`, []model.Value{n("NaN"), null, n("NaN"), null}},
		{"positive infinity singleton", `(VALUES('Infinity'::numeric)) v(x)`, []model.Value{n("NaN"), null, n("NaN"), null}},
		{"negative infinity singleton", `(VALUES('-Infinity'::numeric)) v(x)`, []model.Value{n("NaN"), null, n("NaN"), null}},
		{"nan counts toward sample size", `(VALUES('NaN'::numeric),(1::numeric)) v(x)`, []model.Value{n("NaN"), n("NaN"), n("NaN"), n("NaN")}},
		{"infinities count toward sample size", `(VALUES('-Infinity'::numeric),('Infinity'::numeric)) v(x)`, []model.Value{n("NaN"), n("NaN"), n("NaN"), n("NaN")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT var_pop(x),var_samp(x),stddev_pop(x),stddev_samp(x) FROM `+scenario.source, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{scenario.want}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("cardinality and nonfinite precedence=%v, want %v", got, want)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLNumericStatisticsOrderedFilterMasks(t *testing.T) {
	graph := sqlGraph(t, `SELECT var_pop(DISTINCT 10::numeric/x ORDER BY 10::numeric/x DESC) FILTER(WHERE x<>0)=2.25::numeric,
		stddev_pop(10::numeric/x ORDER BY 100/x) FILTER(WHERE x<>0)=1.5::numeric,
		var_samp(10::numeric/x ORDER BY 100/x) FILTER(WHERE NULL::boolean),
		stddev_samp(10::numeric/x ORDER BY 100/x) FILTER(WHERE false)
		FROM (VALUES(0),(2),(2),(5),(5),(NULL::int4)) v(x)`, nil)
	env, dir := groupingSetsEnvironment(t, false)
	env.Aggregate.PartitionMemoryBytes = 1024
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	null := model.NullVal(model.TypeNumeric)
	want := [][]model.Value{{model.BoolVal(true), model.BoolVal(true), null, null}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered masked statistics=%v, want %v", got, want)
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLNumericStatisticsGroupedSpill(t *testing.T) {
	var values []string
	var want [][]model.Value
	for group := range 16 {
		values = append(values, fmt.Sprintf("(%d,9007199254740992::numeric),(%d,9007199254740994::numeric),(%d,9007199254740994::numeric),(%d,9007199254740996::numeric)", group, group, group, group))
		want = append(want, []model.Value{model.Int64Val(int64(group)), model.BoolVal(true), model.BoolVal(true)})
	}
	graph := sqlGraph(t, `SELECT g,var_samp(DISTINCT x ORDER BY x)=4::numeric,
		stddev_samp(DISTINCT x ORDER BY x DESC)=2::numeric
		FROM (VALUES`+strings.Join(values, ",")+`) v(g,x) GROUP BY g ORDER BY g`, nil)
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
				t.Fatalf("grouped numeric statistics=%v, want %v", got, want)
			}
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLNumericStatisticsGroupingSetsReplay(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,var_pop(10::numeric/x ORDER BY 100/x) FILTER(WHERE x<>0)=2.25::numeric,
		stddev_pop(DISTINCT 10::numeric/x ORDER BY 10::numeric/x DESC) FILTER(WHERE x<>0)=1.5::numeric,GROUPING(g)
		FROM (VALUES(1,0),(1,2),(1,5),(2,0),(2,2),(2,5)) v(g,x)
		GROUP BY GROUPING SETS ((g),(),(g),())`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Aggregate.PartitionMemoryBytes = 1024
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, b := model.Int64Val, model.BoolVal(true)
	group1 := []model.Value{i(1), b, b, i(0)}
	group2 := []model.Value{i(2), b, b, i(0)}
	total := []model.Value{model.NullVal(model.TypeInt64), b, b, i(1)}
	assertGroupingSetsBag(t, rows(t, portal), [][]model.Value{group1, group2, total, group1, group2, total})
	if env.Query.Usage().Spill.Peak == 0 {
		t.Fatal("grouping-set numeric input did not replay from spill")
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLNumericStatisticsWindows(t *testing.T) {
	n := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	null, b := model.NullVal(model.TypeNumeric), model.BoolVal(true)
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			name: "growing sample preserves adjacent large values",
			sql: `SELECT var_samp(x) OVER w,stddev_samp(x) OVER w
				FROM (VALUES(1,9007199254740992::numeric),(2,9007199254740994::numeric),(3,9007199254740996::numeric),(4,NULL::numeric)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) ORDER BY k`,
			want: [][]model.Value{{null, null}, {n("2.0000000000000000"), n("1.4142135623730950")}, {n("4.0000000000000000"), n("2.0000000000000000")}, {n("4.0000000000000000"), n("2.0000000000000000")}},
		},
		{
			name: "moving int8 frame resets per partition",
			sql: `SELECT var_pop(x) OVER w=CASE WHEN k=1 THEN 0::numeric ELSE 1::numeric END,
				stddev_pop(x) OVER w=CASE WHEN k=1 THEN 0::numeric ELSE 1::numeric END,
				var_samp(x) OVER w=2::numeric
				FROM (VALUES(1,1,9007199254740992::int8),(1,2,9007199254740994::int8),(1,3,9007199254740996::int8),(2,1,7::int8)) v(g,k,x)
				WINDOW w AS (PARTITION BY g ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY g,k`,
			want: [][]model.Value{{b, b, model.NullVal(model.TypeBoolean)}, {b, b, b}, {b, b, b}, {b, b, model.NullVal(model.TypeBoolean)}},
		},
		{
			name: "excluded current row changes sample denominator",
			sql: `SELECT var_samp(x) OVER w=CASE WHEN k=2 THEN 8::numeric ELSE 2::numeric END,
				stddev_pop(x) OVER w=CASE WHEN k=2 THEN 2::numeric ELSE 1::numeric END
				FROM (VALUES(1,0::numeric),(2,2::numeric),(3,4::numeric)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) ORDER BY k`,
			want: [][]model.Value{{b, b}, {b, b}, {b, b}},
		},
		{
			name: "following frame becomes empty",
			sql: `SELECT var_pop(x) OVER w,stddev_samp(x) OVER w
				FROM (VALUES(1,2::numeric),(2,4::numeric),(3,6::numeric)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING) ORDER BY k`,
			want: [][]model.Value{{n("0"), null}, {n("0"), null}, {null, null}},
		},
		{
			name: "nonfinite values leave moving frame",
			sql: `SELECT var_pop(x) OVER w,stddev_samp(x) OVER w
				FROM (VALUES(1,'NaN'::numeric),(2,'Infinity'::numeric),(3,2::numeric),(4,4::numeric)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY k`,
			want: [][]model.Value{{n("NaN"), null}, {n("NaN"), n("NaN")}, {n("NaN"), n("NaN")}, {n("1.00000000000000000000"), n("1.4142135623730950")}},
		},
		{
			name: "filter masks growing frame division",
			sql: `SELECT var_pop(10::numeric/x) FILTER(WHERE x<>0) OVER w=2.25::numeric,
				stddev_pop(10::numeric/x) FILTER(WHERE x<>0) OVER w=1.5::numeric
				FROM (VALUES(1,0),(2,2),(3,NULL::int4),(4,5)) v(k,x)
				WINDOW w AS (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) ORDER BY k`,
			want: [][]model.Value{{model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)}, {model.BoolVal(false), model.BoolVal(false)}, {model.BoolVal(false), model.BoolVal(false)}, {b, b}},
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
				t.Fatalf("window numeric statistics=%v, want %v", got, scenario.want)
			}
			if env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("numeric window partition did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLNumericStatisticsActiveSpillCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
	}{
		{"grouping replay", `SELECT var_pop(x)=1::numeric FROM (VALUES(1,9007199254740992::numeric),(2,9007199254740994::numeric)) v(g,x) GROUP BY GROUPING SETS ((),(g))`},
		{"window partition", `SELECT var_pop(x) OVER (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)=0::numeric FROM (VALUES(1,9007199254740992::numeric),(2,9007199254740994::numeric),(3,9007199254740996::numeric)) v(k,x)`},
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
				if err != nil || !reflect.DeepEqual(value, model.BoolVal(true)) {
					t.Fatalf("first statistic=%v, error=%v, want true", value, err)
				}
				if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
					t.Fatalf("no active spill before %s: %+v", ending, usage)
				}
				if ending == "early close" {
					closePortal(t, portal)
				} else {
					cancel()
					if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled statistics error=%v, want context cancellation", err)
					}
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}
