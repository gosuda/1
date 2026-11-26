package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestampExtractExactFractions(t *testing.T) {
	function := jsonCallable(t, 6202)
	for _, tc := range []struct {
		name, unit string
		micros     int64
		want       string
	}{
		{"before Unix epoch", "epoch", -946684800000001, "-0.000001"},
		{"after Unix epoch", "epoch", -946684799999999, "0.000001"},
		{"negative second floor", "second", -1, "59.999999"},
		{"milliseconds scale", "milliseconds", -1, "59999.999"},
		{"microseconds integral", "microseconds", -1, "59999999"},
		{"midnight Julian scale", "julian", 0, "2451545.0000000000000000000000000000"},
		{"Julian repeating fraction", "julian", 1, "2451545.0000000000115740740740740741"},
		{"Julian noon scale", "julian", 43200000000, "2451545.50000000000000000000"},
		{"minimum calendar", "year", -211813488000000000, "-4714"},
		{"internal end accepted", "year", 9223371331200000000, "294277"},
		{"epoch int64 overflow", "epoch", math.MaxInt64 - 1, "9224318721654.775800"},
		{"epoch division before six-place rounding", "epoch", 9223371331199999999, "9224318016000.000000"},
		{"epoch outside calendar", "epoch", math.MinInt64 + 1, "-9222425352054.775807"},
		{"minute alias", "MM", 123456789, "2"},
		{"Julian alias", "JD", 43200000000, "2451545.50000000000000000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.unit), timestampDatum(tc.micros)}, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 10})
			if err != nil || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("extract(%s, %d)=%v,%v; want %s", tc.unit, tc.micros, got, err, tc.want)
			}
		})
	}
}

func TestTimestampFieldsISOAndBC(t *testing.T) {
	for _, tc := range []struct {
		name, unit       string
		year, month, day int64
		want             string
		floatWant        float64
	}{
		{"previous ISO year", "isoyear", 2005, 1, 1, "2004", 2004},
		{"previous ISO week", "week", 2005, 1, 1, "53", 53},
		{"next ISO year", "isoyear", 2019, 12, 30, "2020", 2020},
		{"BC year", "year", -1, 12, 31, "-1", -1},
		{"BC decade", "decade", -11, 6, 1, "-1", -1},
		{"BC century", "century", -101, 6, 1, "-2", -2},
		{"BC millennium", "millennium", -1001, 6, 1, "-2", -2},
		{"Sunday ISO weekday", "isodow", 2000, 1, 2, "7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamp, err := timestampConstruct(t, tc.year, tc.month, tc.day, 0, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []model.FunctionID{2021, 6202} {
				got, err := jsonCallable(t, id).Call(t.Context(), []model.Value{model.StringVal(tc.unit), stamp}, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64 << 10})
				wrongNumeric := id == 6202 && got.StrVal != tc.want
				wrongFloat := id == 2021 && got.FloatVal != tc.floatWant
				if err != nil || got.IsNull || wrongNumeric || wrongFloat {
					t.Fatalf("%d %s=%v,%v", id, tc.unit, got, err)
				}
			}
		})
	}
}

func TestTimestampPartFloatOperationOrder(t *testing.T) {
	for _, tc := range []struct {
		unit   string
		micros int64
		want   float64
	}{
		{"second", -1, 59 + float64(999999)/1e6},
		{"milliseconds", -1, 59000 + float64(999999)/1000},
		{"julian", 123456789, 2451545 + (123+float64(456789)/1e6)/86400},
		{"epoch", -946684800000001, -0.000001},
		{"epoch", math.MaxInt64 - 1, (float64(math.MaxInt64-1) + float64(946684800000000)) / 1e6},
	} {
		got, err := jsonCallable(t, 2021).Call(t.Context(), []model.Value{model.StringVal(tc.unit), timestampDatum(tc.micros)}, builtin.CallBudget{})
		if err != nil || math.Float64bits(got.FloatVal) != math.Float64bits(tc.want) {
			t.Fatalf("date_part(%s,%d)=%v,%v; want %.17g", tc.unit, tc.micros, got, err, tc.want)
		}
	}
}

func TestTimestampFieldsInfinityAndErrorOrder(t *testing.T) {
	for _, id := range []model.FunctionID{2021, 6202} {
		function := jsonCallable(t, id)
		for _, micros := range []int64{math.MinInt64, math.MaxInt64} {
			for _, unit := range []string{"microseconds", "milliseconds", "second", "minute", "hour", "day", "month", "quarter", "week", "dow", "isodow", "doy", "timezone", "timezone_hour", "timezone_minute"} {
				got, err := function.Call(t.Context(), []model.Value{model.StringVal(unit), timestampDatum(micros)}, builtin.CallBudget{})
				if err != nil || !got.IsNull {
					t.Fatalf("%d infinite %s=%v,%v", id, unit, got, err)
				}
			}
			for _, unit := range []string{"year", "decade", "century", "millennium", "isoyear", "julian", "epoch"} {
				got, err := function.Call(t.Context(), []model.Value{model.StringVal(unit), timestampDatum(micros)}, builtin.CallBudget{OutputBytes: 9, MemoryBytes: 9})
				want, sign := "Infinity", 1
				if micros < 0 {
					want, sign = "-Infinity", -1
				}
				wrongNumeric := id == 6202 && got.StrVal != want
				wrongFloat := id == 2021 && !math.IsInf(got.FloatVal, sign)
				if err != nil || got.IsNull || wrongNumeric || wrongFloat {
					t.Fatalf("%d infinite %s=%v,%v", id, unit, got, err)
				}
			}
		}
		for _, tc := range []struct {
			unit   string
			micros int64
			state  string
		}{
			{"unknown", math.MinInt64 + 1, "22023"},
			{"now", math.MinInt64 + 1, "0A000"},
			{"timezone", math.MinInt64 + 1, "22008"},
			{"year", -211813488000000001, "22008"},
			{"timezone", 0, "0A000"},
			{"infinity", math.MaxInt64, "0A000"},
			{"ago", math.MaxInt64, "22023"},
		} {
			_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.unit), timestampDatum(tc.micros)}, builtin.CallBudget{})
			requireSQLState(t, err, tc.state)
		}
	}
}

func TestTimestampFieldsBudgetsCancellationAndOwnership(t *testing.T) {
	function := jsonCallable(t, 6202)
	args := []model.Value{model.StringVal("julian"), timestampDatum(1)}
	for _, budget := range []builtin.CallBudget{{OutputBytes: 4, MemoryBytes: 64 << 10}, {OutputBytes: 128, MemoryBytes: 1}} {
		_, err := function.Call(t.Context(), args, budget)
		requireSQLState(t, err, "54000")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, id := range []model.FunctionID{2021, 6202} {
		_, err := jsonCallable(t, id).Call(ctx, args, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 10})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%d cancellation=%v", id, err)
		}
	}
	first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	args[1] = timestampDatum(43200000000)
	second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 10})
	if err != nil || second.StrVal != "2451545.50000000000000000000" || first.StrVal != "2451545.0000000000115740740740740741" {
		t.Fatalf("owned results=%v,%v,%v", first, second, err)
	}
}

func TestTimestampJulianWorkspaceReleasesAllocator(t *testing.T) {
	query := numericRangeQuery(t, 64<<10)
	allocator := numericInvocationAllocator{Allocator: query.Allocator(runtime.WorkMemory), query: query}
	function := jsonCallable(t, 6202)
	args := []model.Value{model.StringVal("julian"), timestampDatum(1)}
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 10, Allocator: allocator})
	if err != nil || value.StrVal != "2451545.0000000000115740740740740741" {
		t.Fatalf("Julian with allocator=%v,%v", value, err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("successful Julian workspace leaked: %+v", usage)
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 10, Allocator: allocator})
	requireSQLState(t, err, "54000")
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("failed Julian workspace leaked: %+v", usage)
	}
}
