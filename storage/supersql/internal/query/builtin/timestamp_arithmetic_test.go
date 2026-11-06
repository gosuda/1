package builtin_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func timestampDatum(value int64) model.Value {
	return model.Value{Type: model.TypeTimestamp, IntVal: value}
}

func timestampCall(t *testing.T, id model.FunctionID, args ...model.Value) (model.Value, error) {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
}

func timestampConstruct(t *testing.T, year, month, day, hour, minute int64, seconds float64) (model.Value, error) {
	t.Helper()
	return timestampCall(t, 3461, model.Int64Val(year), model.Int64Val(month), model.Int64Val(day), model.Int64Val(hour), model.Int64Val(minute), model.Float64Val(seconds))
}

func TestTimestampConstructorCalendarAndRounding(t *testing.T) {
	for _, test := range []struct {
		name                           string
		year, month, day, hour, minute int64
		seconds                        float64
		want                           int64
	}{
		{"epoch", 2000, 1, 1, 0, 0, 0, 0},
		{"leap", 2000, 2, 29, 0, 0, 0, 5_097_600_000_000},
		{"BC leap year", -1, 2, 29, 0, 0, 0, -63_108_806_400_000_000},
		{"minimum", -4714, 11, 24, 0, 0, 0, -211_813_488_000_000_000},
		{"minimum rollover", -4714, 11, 23, 24, 0, 0, -211_813_488_000_000_000},
		{"maximum", 294276, 12, 31, 23, 59, 59.999999, 9_223_371_331_199_999_999},
		{"midnight", 1999, 12, 31, 24, 0, 0, 0},
		{"rounded midnight", 1999, 12, 31, 23, 59, 59.9999996, 0},
		{"even half", 2000, 1, 1, 0, 0, 0.0000005, 0},
		{"odd half", 2000, 1, 1, 0, 0, 0.0000015, 2},
		{"negative rounds zero", 2000, 1, 1, 0, 0, -0.0000005, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := timestampConstruct(t, test.year, test.month, test.day, test.hour, test.minute, test.seconds)
			if err != nil || got.IsNull || got.IntVal != test.want {
				t.Fatalf("timestamp = %+v, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestTimestampConstructorRejectsInvalidFields(t *testing.T) {
	for _, test := range []struct {
		name                           string
		year, month, day, hour, minute int64
		seconds                        float64
	}{
		{"year zero", 0, 1, 1, 0, 0, 0},
		{"minimum int year", math.MinInt32, 1, 1, 0, 0, 0},
		{"common century", 1900, 2, 29, 0, 0, 0},
		{"month zero", 2000, 0, 1, 0, 0, 0},
		{"before minimum", -4714, 11, 23, 23, 59, 59},
		{"after maximum", 294277, 1, 1, 0, 0, 0},
		{"roll beyond maximum", 294276, 12, 31, 24, 0, 0},
		{"negative fraction", 2000, 1, 1, 0, 0, -0.0000006},
		{"past midnight", 2000, 1, 1, 24, 0, 0.000001},
		{"NaN", 2000, 1, 1, 0, 0, math.NaN()},
		{"infinity", 2000, 1, 1, 0, 0, math.Inf(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := timestampConstruct(t, test.year, test.month, test.day, test.hour, test.minute, test.seconds)
			requireSQLState(t, err, "22008")
		})
	}
}

func TestTimestampPrecisionSignEndpointsAndModifiers(t *testing.T) {
	for _, test := range []struct{ value, precision, want int64 }{
		{500000, 0, 1000000}, {-500000, 0, -1000000}, {-499999, 0, 0},
		{9223371331199999999, 0, 9223371331200000000},
		{-211813488000000000, 0, -211813488000000000},
		{1234567, -1, 1234567}, {math.MaxInt64, -2, math.MaxInt64},
		{math.MinInt64, 7, math.MinInt64},
	} {
		got, err := timestampCall(t, 1961, timestampDatum(test.value), model.Int64Val(test.precision))
		if err != nil || got.IntVal != test.want {
			t.Errorf("scale(%d,%d) = %+v,%v; want %d", test.value, test.precision, got, err, test.want)
		}
	}
	for _, precision := range []int64{-2, 7} {
		_, err := timestampCall(t, 1961, timestampDatum(0), model.Int64Val(precision))
		requireSQLState(t, err, "22023")
	}
}

func TestTimestampConversionFloorsAndInfinity(t *testing.T) {
	got, err := timestampCall(t, 2029, timestampDatum(-1))
	if err != nil || got.IntVal != -1 {
		t.Fatalf("negative date = %+v,%v", got, err)
	}
	got, err = timestampCall(t, 1316, timestampDatum(-1))
	if err != nil || got.IntVal != 86_399_999_999 {
		t.Fatalf("negative time = %+v,%v", got, err)
	}
	for _, infinity := range []int64{math.MinInt64, math.MaxInt64} {
		got, err = timestampCall(t, 1316, timestampDatum(infinity))
		if err != nil || !got.IsNull {
			t.Fatalf("infinite time = %+v,%v", got, err)
		}
		dateInfinity := int64(math.MaxInt32)
		if infinity < 0 {
			dateInfinity = math.MinInt32
		}
		got, err = timestampCall(t, 2029, timestampDatum(infinity))
		if err != nil || got.IntVal != dateInfinity {
			t.Fatalf("infinite date = %+v,%v", got, err)
		}
		got, err = timestampCall(t, 2024, timestampDatum(dateInfinity))
		if err != nil || got.IntVal != infinity {
			t.Fatalf("infinite timestamp = %+v,%v", got, err)
		}
	}
	for _, id := range []model.FunctionID{2025, 1272, 1296} {
		args := []model.Value{timestampDatum(-1), timestampDatum(86_400_000_000)}
		if id == 1296 {
			args[0], args[1] = args[1], args[0]
		}
		got, err = timestampCall(t, id, args...)
		if err != nil || got.IntVal != 0 {
			t.Fatalf("date/time ID%d = %+v,%v", id, got, err)
		}
	}
	_, err = timestampCall(t, 2024, timestampDatum(106751983))
	requireSQLState(t, err, "22008")
}

func TestTimestampCalendarArithmeticOrder(t *testing.T) {
	january, err := timestampConstruct(t, 2000, 1, 31, 23, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	span := intervalArithmeticDatum(model.Interval{Months: 1, Days: 1, Microseconds: 7_200_000_000})
	for _, id := range []model.FunctionID{2032, 2548} {
		args := []model.Value{january, span}
		if id == 2548 {
			args[0], args[1] = args[1], args[0]
		}
		got, err := timestampCall(t, id, args...)
		if err != nil || got.IntVal != 5_274_000_000_000 {
			t.Fatalf("month/day/time order = %+v,%v", got, err)
		}
	}
	got, err := timestampCall(t, 2033, january, intervalArithmeticDatum(model.Interval{Months: 1}))
	if err != nil || got.IntVal != -3_600_000_000 {
		t.Fatalf("previous month = %+v,%v", got, err)
	}
	for _, span := range []model.Interval{
		{Months: 1, Days: -31}, {Days: 1, Microseconds: -86_400_000_000}, {Microseconds: 1},
	} {
		_, err := timestampCall(t, 2032, timestampDatum(9223371331199999999), intervalArithmeticDatum(span))
		requireSQLState(t, err, "22008")
	}
}

func TestTimestampDifferenceJustifiesHoursAndChecksOverflow(t *testing.T) {
	for _, test := range []struct {
		left, right int64
		want        model.Interval
	}{
		{90_000_000_001, 0, model.Interval{Days: 1, Microseconds: 3_600_000_001}},
		{0, 90_000_000_001, model.Interval{Days: -1, Microseconds: -3_600_000_001}},
		{0, math.MinInt64, model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}},
	} {
		got, err := timestampCall(t, 2031, timestampDatum(test.left), timestampDatum(test.right))
		requireIntervalArithmeticResult(t, got, err, test.want)
	}
	for _, pair := range [][2]int64{{math.MinInt64, math.MinInt64}, {math.MaxInt64, math.MaxInt64}, {9223371331199999999, -211813488000000000}, {-211813488000000000, 9223371331199999999}} {
		_, err := timestampCall(t, 2031, timestampDatum(pair[0]), timestampDatum(pair[1]))
		requireSQLState(t, err, "22008")
	}
}

func TestTimestampInfinityAndNegationPrecedence(t *testing.T) {
	positive := intervalArithmeticDatum(model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64})
	negative := intervalArithmeticDatum(model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64})
	for _, test := range []struct {
		id        model.FunctionID
		timestamp int64
		span      model.Value
		want      int64
	}{
		{2032, 0, positive, math.MaxInt64}, {2032, 0, negative, math.MinInt64},
		{2033, 0, positive, math.MinInt64}, {2033, 0, negative, math.MaxInt64},
		{2032, math.MaxInt64, intervalArithmeticDatum(model.Interval{Months: math.MinInt32}), math.MaxInt64},
	} {
		got, err := timestampCall(t, test.id, timestampDatum(test.timestamp), test.span)
		if err != nil || got.IntVal != test.want {
			t.Fatalf("infinity arithmetic = %+v,%v; want %d", got, err, test.want)
		}
	}
	for _, span := range []model.Interval{{Months: math.MinInt32}, {Days: math.MinInt32}, {Microseconds: math.MinInt64}, {Months: -math.MaxInt32, Days: -math.MaxInt32, Microseconds: -math.MaxInt64}} {
		_, err := timestampCall(t, 2033, timestampDatum(math.MaxInt64), intervalArithmeticDatum(span))
		requireSQLState(t, err, "22008")
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Message != "interval out of range" {
			t.Fatalf("negation must precede timestamp infinity: %v", err)
		}
	}
	_, err := timestampCall(t, 2032, timestampDatum(math.MaxInt64), negative)
	requireSQLState(t, err, "22008")
	_, err = timestampCall(t, 2033, timestampDatum(math.MinInt64), negative)
	requireSQLState(t, err, "22008")
}

func TestTimestampSendBudgetAndCancellation(t *testing.T) {
	function, err := builtin.Resolve(2475, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{timestampDatum(-1)}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 7, MemoryBytes: 64 << 20})
	if err == nil {
		t.Fatal("seven-byte budget accepted eight-byte result")
	}
	got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 64 << 20})
	if err != nil || len(got.BytesVal) != 8 || binary.BigEndian.Uint64(got.BytesVal) != math.MaxUint64 {
		t.Fatalf("send = %+v,%v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, args, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
}

func TestTimestampComparisonsOrderInfinityAndRoundedEndpoint(t *testing.T) {
	for _, pair := range [][2]int64{{math.MinInt64, -211813488000000000}, {-1, 0}, {9223371331200000000, math.MaxInt64}} {
		for _, test := range []struct {
			id   model.FunctionID
			want bool
		}{
			{2052, false}, {2053, true}, {2054, true}, {2055, true}, {2056, false}, {2057, false},
		} {
			got, err := timestampCall(t, test.id, timestampDatum(pair[0]), timestampDatum(pair[1]))
			if err != nil || got.IntVal != model.BoolVal(test.want).IntVal {
				t.Fatalf("comparison %d(%v) = %+v,%v", test.id, pair, got, err)
			}
		}
		for _, test := range []struct {
			id   model.FunctionID
			want int64
		}{{2035, pair[0]}, {2036, pair[1]}, {2045, -1}} {
			got, err := timestampCall(t, test.id, timestampDatum(pair[0]), timestampDatum(pair[1]))
			if err != nil || got.IntVal != test.want {
				t.Fatalf("ordering %d(%v) = %+v,%v", test.id, pair, got, err)
			}
		}
	}
	for _, value := range []int64{math.MinInt64, math.MaxInt64, 9223371331200000000} {
		got, err := timestampCall(t, 2048, timestampDatum(value))
		want := value != math.MinInt64 && value != math.MaxInt64
		if err != nil || got.IntVal != model.BoolVal(want).IntVal {
			t.Fatalf("finite(%d) = %+v,%v", value, got, err)
		}
	}
}

func TestTimestampMonthArithmeticCrossesBCAndClamps(t *testing.T) {
	before, err := timestampConstruct(t, -1, 12, 31, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := timestampCall(t, 2032, before, intervalArithmeticDatum(model.Interval{Months: 2}))
	if err != nil {
		t.Fatal(err)
	}
	want, err := timestampConstruct(t, 1, 2, 28, 0, 0, 0)
	if err != nil || got.IntVal != want.IntVal {
		t.Fatalf("BC transition = %+v; want %+v,%v", got, want, err)
	}
	for _, span := range []model.Interval{{Months: -1, Days: 31}, {Days: -1, Microseconds: 86_400_000_000}, {Microseconds: -1}} {
		_, err := timestampCall(t, 2032, timestampDatum(-211813488000000000), intervalArithmeticDatum(span))
		requireSQLState(t, err, "22008")
	}
}
