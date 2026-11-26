package builtin_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func dateFieldsDatum(t *testing.T, year, month, day int64) model.Value {
	t.Helper()
	value, err := jsonCallable(t, 3846).Call(t.Context(), []model.Value{model.Int64Val(year), model.Int64Val(month), model.Int64Val(day)}, builtin.CallBudget{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDateFieldsCalendarBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, unit       string
		year, month, day int64
		want             string
		floatWant        float64
	}{
		{"ISO previous year", "isoyear", 2000, 1, 1, "1999", 1999},
		{"ISO previous week", "week", 2000, 1, 1, "52", 52},
		{"ISO next year", "isoyear", 2014, 12, 29, "2015", 2015},
		{"ISO week one", "week", 2014, 12, 29, "1", 1},
		{"ISO week fifty three", "week", 2016, 1, 3, "53", 53},
		{"Sunday", "dow", 2016, 1, 3, "0", 0},
		{"ISO Sunday", "isodow", 2016, 1, 3, "7", 7},
		{"leap day of year", "doy", 2000, 12, 31, "366", 366},
		{"ordinary day of year", "doy", 1900, 12, 31, "365", 365},
		{"quarter", "qtr", 2000, 10, 1, "4", 4},
		{"month alias", "mon", 2000, 2, 29, "2", 2},
		{"day", "day", 2000, 2, 29, "29", 29},
		{"BC year", "year", -1, 1, 1, "-1", -1},
		{"BC ISO year", "isoyear", -1, 1, 1, "-2", -2},
		{"BC zero decade", "decade", -1, 1, 1, "0", 0},
		{"BC first negative decade", "decade", -2, 1, 1, "-1", -1},
		{"BC decade endpoint", "decade", -11, 1, 1, "-1", -1},
		{"BC next decade", "decade", -12, 1, 1, "-2", -2},
		{"BC century endpoint", "century", -100, 1, 1, "-1", -1},
		{"BC next century", "century", -101, 1, 1, "-2", -2},
		{"BC millennium endpoint", "millennium", -1000, 1, 1, "-1", -1},
		{"BC next millennium", "millennium", -1001, 1, 1, "-2", -2},
		{"AD century endpoint", "century", 2000, 1, 1, "20", 20},
		{"AD next century", "century", 2001, 1, 1, "21", 21},
		{"AD millennium endpoint", "millennium", 2000, 1, 1, "2", 2},
		{"AD next millennium", "millennium", 2001, 1, 1, "3", 3},
		{"Julian origin", "julian", -4714, 11, 24, "0", 0},
		{"minimum epoch", "epoch", -4714, 11, 24, "-210866803200", -210866803200},
		{"Unix epoch", "epoch", 1970, 1, 1, "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.unit), dateFieldsDatum(t, tc.year, tc.month, tc.day)}
			exact, err := jsonCallable(t, 6199).Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: uint64(len(tc.want))})
			if err != nil || exact.Type != model.TypeNumeric || exact.StrVal != tc.want {
				t.Fatalf("extract=%v,%v; want %s", exact, err, tc.want)
			}
			floating, err := jsonCallable(t, 1384).Call(t.Context(), args, builtin.CallBudget{})
			if err != nil || floating.Type != model.TypeFloat64 || floating.FloatVal != tc.floatWant {
				t.Fatalf("date_part=%v,%v; want %g", floating, err, tc.floatWant)
			}
		})
	}
}

func TestDatePartTimestampPromotionBoundary(t *testing.T) {
	last := dateFieldsDatum(t, 294276, 12, 31)
	value, err := jsonCallable(t, 1384).Call(t.Context(), []model.Value{model.StringVal("year"), last}, builtin.CallBudget{})
	if err != nil || value.FloatVal != 294276 {
		t.Fatalf("last timestamp year=%v,%v", value, err)
	}
	for _, unit := range []string{"year", "unknown"} {
		_, err := jsonCallable(t, 1384).Call(t.Context(), []model.Value{model.StringVal(unit), dateFieldsDatum(t, 294277, 1, 1)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
	maxDate := dateFieldsDatum(t, 5874897, 12, 31)
	for _, tc := range []struct{ unit, want string }{
		{"year", "5874897"}, {"julian", "2147483493"}, {"epoch", "185331706992000"},
	} {
		value, err := jsonCallable(t, 6199).Call(t.Context(), []model.Value{model.StringVal(tc.unit), maxDate}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 32})
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("maximum date %s=%v,%v; want %s", tc.unit, value, err, tc.want)
		}
	}
}

func TestDateFieldsUnitRecognitionAndClockDifferences(t *testing.T) {
	date := dateFieldsDatum(t, 2000, 1, 1)
	for _, unit := range []string{"hour", "m", "mm", "second", "microseconds", "millisecond", "MICROSECOND" + strings.Repeat("x", 100)} {
		args := []model.Value{model.StringVal(unit), date}
		value, err := jsonCallable(t, 1384).Call(t.Context(), args, builtin.CallBudget{})
		if err != nil || value.FloatVal != 0 || value.IsNull {
			t.Fatalf("date_part %q=%v,%v", unit, value, err)
		}
		_, err = jsonCallable(t, 6199).Call(t.Context(), args, builtin.CallBudget{})
		requireSQLState(t, err, "0A000")
	}
	for _, id := range []model.FunctionID{1384, 6199} {
		for _, unit := range []string{"timezone", "timezone_hour", "now", "infinity", "allballs"} {
			_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{model.StringVal(unit), date}, builtin.CallBudget{})
			requireSQLState(t, err, "0A000")
		}
		for _, unit := range []string{"unknown", " year", "years ", "monday", "ago", "@", "ad"} {
			_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{model.StringVal(unit), date}, builtin.CallBudget{})
			requireSQLState(t, err, "22023")
		}
	}
	value, err := jsonCallable(t, 6199).Call(t.Context(), []model.Value{model.StringVal("YeAr\x00garbage"), date}, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 4})
	if err != nil || value.StrVal != "2000" {
		t.Fatalf("cstring field=%v,%v", value, err)
	}
}

func TestDateFieldsInfinityNullsAndErrors(t *testing.T) {
	for _, days := range []int64{math.MinInt32, math.MaxInt32} {
		date := model.Value{Type: model.TypeTimestamp, IntVal: days}
		for _, id := range []model.FunctionID{1384, 6199} {
			function := jsonCallable(t, id)
			for _, unit := range []string{"day", "month", "quarter", "week", "dow", "isodow", "doy"} {
				value, err := function.Call(t.Context(), []model.Value{model.StringVal(unit), date}, builtin.CallBudget{})
				if err != nil || !value.IsNull {
					t.Fatalf("%d infinite %s=%v,%v", id, unit, value, err)
				}
			}
			for _, unit := range []string{"year", "decade", "century", "millennium", "julian", "isoyear", "epoch"} {
				value, err := function.Call(t.Context(), []model.Value{model.StringVal(unit), date}, builtin.CallBudget{OutputBytes: 9, MemoryBytes: 9})
				if err != nil {
					t.Fatal(err)
				}
				want, sign := "Infinity", 1
				if days < 0 {
					want, sign = "-Infinity", -1
				}
				wrongNumeric := id == 6199 && value.StrVal != want
				wrongFloat := id == 1384 && !math.IsInf(value.FloatVal, sign)
				if wrongNumeric || wrongFloat {
					t.Fatalf("%d infinite %s=%v; want %s", id, unit, value, want)
				}
			}
			for _, unit := range []string{"hour", "timezone_hour"} {
				value, err := function.Call(t.Context(), []model.Value{model.StringVal(unit), date}, builtin.CallBudget{})
				if id == 6199 {
					requireSQLState(t, err, "0A000")
				} else if err != nil || !value.IsNull {
					t.Fatalf("infinite clock=%v,%v", value, err)
				}
			}
			_, err := function.Call(t.Context(), []model.Value{model.StringVal("unknown"), date}, builtin.CallBudget{})
			requireSQLState(t, err, "22023")
		}
	}
}

func TestDateFieldsQuotaAndCancellation(t *testing.T) {
	args := []model.Value{model.StringVal("year"), dateFieldsDatum(t, 2000, 1, 1)}
	for _, budget := range []builtin.CallBudget{{OutputBytes: 3, MemoryBytes: 4}, {OutputBytes: 4, MemoryBytes: 3}} {
		_, err := jsonCallable(t, 6199).Call(t.Context(), args, budget)
		requireSQLState(t, err, "54000")
	}
	args[1] = model.Value{Type: model.TypeTimestamp, IntVal: math.MinInt32}
	_, err := jsonCallable(t, 6199).Call(t.Context(), args, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 9})
	requireSQLState(t, err, "54000")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, id := range []model.FunctionID{1384, 6199} {
		_, err := jsonCallable(t, id).Call(ctx, args, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 32})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%d cancellation=%v", id, err)
		}
	}
}
