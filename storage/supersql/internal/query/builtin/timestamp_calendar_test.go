package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func timestampCalendarDatum(t *testing.T, year, month, day, hour, minute int64, second float64) model.Value {
	t.Helper()
	value, err := timestampConstruct(t, year, month, day, hour, minute, second)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestTimestampTruncCalendarBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, unit                   string
		year, month, day             int64
		wantYear, wantMonth, wantDay int64
	}{
		{"ISO previous year", "week", 2021, 1, 1, 2020, 12, 28},
		{"ISO next year", "week", 2018, 12, 31, 2018, 12, 31},
		{"quarter", "quarter", 2024, 5, 31, 2024, 4, 1},
		{"leap month", "month", 2024, 2, 29, 2024, 2, 1},
		{"AD century", "century", 2000, 12, 31, 1901, 1, 1},
		{"AD millennium", "millennium", 2000, 12, 31, 1001, 1, 1},
		{"BC century", "century", -1, 5, 1, -100, 1, 1},
		{"BC millennium", "millennium", -1001, 5, 1, -2000, 1, 1},
		{"BC decade", "decade", -2, 5, 1, -11, 1, 1},
		{"decade crosses era", "decade", 1, 5, 1, -1, 1, 1},
		{"last year", "year", 294276, 12, 31, 294276, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := timestampCalendarDatum(t, test.year, test.month, test.day, 23, 59, 59.999999)
			want := timestampCalendarDatum(t, test.wantYear, test.wantMonth, test.wantDay, 0, 0, 0)
			got, err := timestampCall(t, 2020, model.StringVal(test.unit), input)
			if err != nil || got.Type != model.TypeTimestamp || got.IntVal != want.IntVal {
				t.Fatalf("date_trunc(%s) = %+v,%v; want %d", test.unit, got, err, want.IntVal)
			}
		})
	}
}

func TestTimestampTruncFractionAndRange(t *testing.T) {
	for _, test := range []struct {
		unit        string
		input, want int64
	}{
		{"microseconds", -1, -1},
		{"milliseconds", -1, -1000},
		{"second", -1, -1000000},
		{"minute", -1, -60000000},
		{"hour", -1, -3600000000},
		{"day", -1, -86400000000},
		{"milliseconds", 999999, 999000},
		{"day", -211813488000000000, -211813488000000000},
	} {
		got, err := timestampCall(t, 2020, model.StringVal(test.unit), timestampDatum(test.input))
		if err != nil || got.IntVal != test.want {
			t.Fatalf("date_trunc(%s,%d) = %+v,%v; want %d", test.unit, test.input, got, err, test.want)
		}
	}
	for _, unit := range []string{"month", "year", "century", "millennium"} {
		_, err := timestampCall(t, 2020, model.StringVal(unit), timestampDatum(-211813488000000000))
		requireSQLState(t, err, "22008")
	}
	_, err := timestampCall(t, 2020, model.StringVal("microsecond"), timestampDatum(9223371331200000000))
	requireSQLState(t, err, "22008")
}

func TestTimestampTruncUnitValidationBeforeInfinity(t *testing.T) {
	for _, input := range []int64{0, math.MinInt64, math.MaxInt64} {
		for _, test := range []struct{ unit, state string }{
			{"timezone", "0A000"}, {"timezone_hour", "0A000"},
			{"epoch", "22023"}, {"isoyear", "22023"}, {"mm", "22023"}, {"ago", "22023"},
		} {
			_, err := timestampCall(t, 2020, model.StringVal(test.unit), timestampDatum(input))
			requireSQLState(t, err, test.state)
		}
		got, err := timestampCall(t, 2020, model.StringVal("WeEk"), timestampDatum(input))
		if input != 0 && (err != nil || got.IntVal != input) {
			t.Fatalf("infinite truncation = %+v,%v; want %d", got, err, input)
		}
	}
	_, err := timestampCall(t, 2020, model.StringVal("timezone"), timestampDatum(math.MinInt64+1))
	requireSQLState(t, err, "22008")
}

func TestTimestampAgeCalendarBorrowing(t *testing.T) {
	for _, test := range []struct {
		name                                  string
		laterYear, laterMonth, laterDay       int64
		earlierYear, earlierMonth, earlierDay int64
		want                                  model.Interval
	}{
		{"April borrows thirty", 2004, 6, 1, 2004, 4, 30, model.Interval{Months: 1, Days: 1}},
		{"leap February", 2024, 3, 1, 2024, 2, 29, model.Interval{Days: 1}},
		{"ordinary February", 2023, 3, 1, 2023, 2, 28, model.Interval{Days: 1}},
		{"January source month", 2024, 2, 29, 2024, 1, 31, model.Interval{Days: 29}},
		{"era transition", 1, 1, 1, -1, 1, 1, model.Interval{Months: 12}},
		{"full timestamp range", 294276, 12, 31, -4714, 11, 24, model.Interval{Months: 3587869, Days: 7}},
	} {
		t.Run(test.name, func(t *testing.T) {
			later := timestampCalendarDatum(t, test.laterYear, test.laterMonth, test.laterDay, 0, 0, 0)
			earlier := timestampCalendarDatum(t, test.earlierYear, test.earlierMonth, test.earlierDay, 0, 0, 0)
			got, err := timestampCall(t, 2058, later, earlier)
			if err != nil || got.Type != model.TypeInterval || got.IntervalVal != test.want {
				t.Fatalf("age = %+v,%v; want %+v", got, err, test.want)
			}
			got, err = timestampCall(t, 2058, earlier, later)
			wantNegative := model.Interval{Months: -test.want.Months, Days: -test.want.Days, Microseconds: -test.want.Microseconds}
			if err != nil || got.IntervalVal != wantNegative {
				t.Fatalf("reverse age = %+v,%v; want %+v", got, err, wantNegative)
			}
		})
	}
	later := timestampCalendarDatum(t, 2024, 3, 1, 0, 0, 0)
	earlier := timestampCalendarDatum(t, 2024, 2, 29, 23, 59, 59.999999)
	got, err := timestampCall(t, 2058, later, earlier)
	if err != nil || got.IntervalVal != (model.Interval{Microseconds: 1}) {
		t.Fatalf("sub-day borrowing = %+v,%v", got, err)
	}
}

func TestTimestampAgeInfinity(t *testing.T) {
	for _, test := range []struct {
		left, right int64
		sign        int
	}{
		{math.MaxInt64, 0, 1}, {0, math.MinInt64, 1}, {math.MaxInt64, math.MinInt64, 1},
		{math.MinInt64, 0, -1}, {0, math.MaxInt64, -1}, {math.MinInt64, math.MaxInt64, -1},
	} {
		got, err := timestampCall(t, 2058, timestampDatum(test.left), timestampDatum(test.right))
		want := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
		if test.sign < 0 {
			want = model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
		}
		if err != nil || got.IntervalVal != want {
			t.Fatalf("infinite age = %+v,%v; want %+v", got, err, want)
		}
	}
	for _, input := range []int64{math.MinInt64, math.MaxInt64} {
		_, err := timestampCall(t, 2058, timestampDatum(input), timestampDatum(input))
		requireSQLState(t, err, "22008")
	}
}

func TestTimestampCalendarZeroQuotaAndCancellation(t *testing.T) {
	for _, test := range []struct {
		id   model.FunctionID
		args []model.Value
		want model.Value
	}{
		{2020, []model.Value{model.StringVal("milliseconds"), timestampDatum(-1)}, timestampDatum(-1000)},
		{2058, []model.Value{timestampDatum(1), timestampDatum(0)}, intervalArithmeticDatum(model.Interval{Microseconds: 1})},
	} {
		function, err := builtin.Resolve(test.id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := function.Call(t.Context(), test.args, builtin.CallBudget{})
		if err != nil || got.Type != test.want.Type || got.IntVal != test.want.IntVal || got.IntervalVal != test.want.IntervalVal {
			t.Fatalf("zero-quota call %d = %+v,%v; want %+v", test.id, got, err, test.want)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = function.Call(ctx, test.args, builtin.CallBudget{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled call %d = %v", test.id, err)
		}
	}
}
