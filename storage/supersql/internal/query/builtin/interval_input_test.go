package builtin

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func intervalInputCallable(t *testing.T, style IntervalStyle) Callable {
	t.Helper()
	function, err := Resolve(1160, ResolveOptions{IntervalStyle: style})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func intervalInputArguments(text string, typmod int32) []model.Value {
	return []model.Value{model.StringVal(text), model.Int64Val(0), model.Int64Val(int64(typmod))}
}

func TestIntervalInputSubnormalUnderflowDistinguishesNumericSyntax(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	value, err := function.Call(t.Context(), intervalInputArguments("PT0x1p-1074S", -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if value.IntervalVal != (model.Interval{}) {
		t.Fatalf("subnormal interval=%+v, want zero", value.IntervalVal)
	}
	decimal := "PT" + strconv.FormatFloat(math.SmallestNonzeroFloat64, 'f', 1074, 64) + "S"
	_, err = function.Call(t.Context(), intervalInputArguments(decimal, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22007")
}

func TestIntervalInputPreservesIndependentComponents(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct {
		name, text string
		want       model.Interval
	}{
		{"verbose units", "@ 1 yr 2 mons -3 days 4 hrs 5 mins 6.123456 secs", model.Interval{Months: 14, Days: -3, Microseconds: 14_706_123_456}},
		{"SQL year month and day time", "-1-2 +3 -04:05:06.123456", model.Interval{Months: -14, Days: 3, Microseconds: -14_706_123_456}},
		{"ago reverses mixed signs", "1 year -2 days 03:00:00 ago", model.Interval{Months: -12, Days: 2, Microseconds: -10_800_000_000}},
		{"fractional years become months", "1.5 years", model.Interval{Months: 18}},
		{"fractional months cascade", "1.25 months", model.Interval{Months: 1, Days: 7, Microseconds: 43_200_000_000}},
		{"fractional weeks cascade", "-1.5 weeks", model.Interval{Days: -10, Microseconds: -43_200_000_000}},
		{"unbounded hours stay time", "36:00:00", model.Interval{Microseconds: 129_600_000_000}},
		{"token prefix lookup", "7 microsecondsuffix", model.Interval{Microseconds: 7}},
		{"sign whitespace", "- \t2 days", model.Interval{Days: -2}},
		{"punctuation and filler", "@ 2 days, at 03:00", model.Interval{Days: 2, Microseconds: 10_800_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.Type != model.TypeInterval || got.IntervalVal != tc.want {
				t.Fatalf("interval %q = %v, %v; want %v", tc.text, got, err, tc.want)
			}
		})
	}
}

func TestIntervalInputDistinguishesFractionRoundingRules(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct {
		text   string
		micros int64
		months int32
	}{
		{"0.0000015 seconds", 1, 0},
		{"00:00:00.0000015", 2, 0},
		{"-0.0000015 seconds", -1, 0},
		{"0.125 years", 0, 2},
	} {
		got, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || got.IntervalVal.Microseconds != tc.micros || got.IntervalVal.Months != tc.months || got.IntervalVal.Days != 0 {
			t.Errorf("fraction %q = %v, %v; want months=%d micros=%d", tc.text, got, err, tc.months, tc.micros)
		}
	}
}

func TestIntervalInputISODesignatorsAndAlternativeForms(t *testing.T) {
	function := intervalInputCallable(t, IntervalISO8601)
	for _, tc := range []struct {
		name, text string
		want       model.Interval
	}{
		{"designators", "P2Y6M7DT1H30M", model.Interval{Months: 30, Days: 7, Microseconds: 5_400_000_000}},
		{"extended alternative", "P0002-06-07T01:30:00", model.Interval{Months: 30, Days: 7, Microseconds: 5_400_000_000}},
		{"basic alternative", "P00020607T013000", model.Interval{Months: 30, Days: 7, Microseconds: 5_400_000_000}},
		{"signed fractional fields", "P-1.5Y2.25M-3DT4.5H", model.Interval{Months: -16, Days: 4, Microseconds: 59_400_000_000}},
		{"scientific notation", "PT1e2S", model.Interval{Microseconds: 100_000_000}},
		{"hexadecimal strtod", "PT0x1.8p2S", model.Interval{Microseconds: 6_000_000}},
		{"duplicate designators sum", "P1Y2Y1W2D", model.Interval{Months: 36, Days: 9}},
		{"truncated alternate", "P2-6T1:30", model.Interval{Months: 30, Microseconds: 5_400_000_000}},
		{"basic fractional tail", "PT010203.75", model.Interval{Microseconds: 3_723_000_001}},
		{"empty time part", "PT", model.Interval{}},
		{"repeated time markers", "PT1HT2H", model.Interval{Microseconds: 10_800_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IntervalVal != tc.want {
				t.Fatalf("ISO interval %q = %v, %v; want %v", tc.text, got, err, tc.want)
			}
		})
	}
}

func TestIntervalInputSQLStandardLeadingSign(t *testing.T) {
	for _, style := range []IntervalStyle{IntervalPostgres, IntervalPostgresVerbose, IntervalSQLStandard, IntervalISO8601} {
		function := intervalInputCallable(t, style)
		for _, explicit := range []bool{false, true} {
			text := "-1 02:03:04"
			if explicit {
				text = "-1 +02:03:04"
			}
			want := int64(7_384_000_000)
			if style == IntervalSQLStandard && !explicit {
				want = -want
			}
			got, err := function.Call(t.Context(), intervalInputArguments(text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IntervalVal.Days != -1 || got.IntervalVal.Microseconds != want {
				t.Errorf("style=%d interval=%q: %v, %v; want day=-1 micros=%d", style, text, got, err, want)
			}
		}
	}
}

func TestIntervalInputTypmodControlsBareUnitsAndFinalRounding(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct {
		name, text string
		typmod     int32
		want       model.Interval
	}{
		{"bare year", "2", intervalYearMask<<16 | intervalFullPrecision, model.Interval{Months: 24}},
		{"bare hour", "2", (intervalDayMask|intervalHourMask)<<16 | intervalFullPrecision, model.Interval{Microseconds: 7_200_000_000}},
		{"minute second two fields", "12:34", (intervalMinuteMask|intervalSecondMask)<<16 | intervalFullPrecision, model.Interval{Microseconds: 754_000_000}},
		{"unqualified two fields", "12:34", -1, model.Interval{Microseconds: 45_240_000_000}},
		{"fraction selects minute second", "12:34.5", -1, model.Interval{Microseconds: 754_500_000}},
		{"discard smaller fields", "1 year 2 mons 3 days 04:05:06", intervalDayMask<<16 | intervalFullPrecision, model.Interval{Months: 14, Days: 3}},
		{"precision carries", "1.999999 seconds", intervalFullRange<<16 | 3, model.Interval{Microseconds: 2_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), intervalInputArguments(tc.text, tc.typmod), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IntervalVal != tc.want {
				t.Fatalf("%q typmod=%d: %v, %v; want %v", tc.text, tc.typmod, got, err, tc.want)
			}
		})
	}
}

func TestIntervalInputNumericLimitsAndInfinity(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct {
		text string
		want model.Interval
	}{
		{"2147483647 mons -2147483648 days -9223372036854775808 usecs", model.Interval{Months: math.MaxInt32, Days: math.MinInt32, Microseconds: math.MinInt64}},
		{"178956970 years 7 months", model.Interval{Months: math.MaxInt32}},
		{"INFINITY", intervalInfinityValue(1)},
		{"- infinity", intervalInfinityValue(-1)},
		{"2147483647 mons 2147483647 days 9223372036854775807 usecs", intervalInfinityValue(1)},
	} {
		got, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || got.IntervalVal != tc.want {
			t.Errorf("boundary %q = %v, %v; want %v", tc.text, got, err, tc.want)
		}
	}
}

func TestIntervalInputErrorPrecedence(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct{ text, state string }{
		{"2147483648 days", "22015"},
		{"178956971 years", "22008"},
		{"1-12", "22015"},
		{"9223372036854775808 usecs", "22015"},
		{"-2147483648 days ago", "22015"},
		{"1 day 2147483648 days", "22015"},
		{"2147483648 days 1 day", "22015"},
		{"1 day 2 days", "22007"},
		{"invalid 2147483648 days", "22015"},
		{"2147483648 days invalid", "22007"},
		{"00:99", "22015"},
		{"-00:99", "22007"},
		{"infinity ago", "22007"},
		{"1 second 1 millisecond", ""},
		{"1.5 seconds 1 millisecond", "22007"},
		{"PT1000000000000001S", "22015"},
		{"PT1e999S", "22007"},
		{"PT1e-999S", "22007"},
		{"P+1D", "22007"},
		{"P1D trailing", "22007"},
	} {
		_, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if tc.state == "" {
			if err != nil {
				t.Errorf("interval %q: %v", tc.text, err)
			}
			continue
		}
		var sql *model.Error
		if !errors.As(err, &sql) || sql.Code != tc.state {
			t.Errorf("interval %q error=%v; want SQLSTATE %s", tc.text, err, tc.state)
		}
	}
}

func TestIntervalInputScannerLimitsDoNotConstrainISOOrWhitespace(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, tc := range []struct {
		name, text string
		want       model.Interval
	}{
		{"workspace boundary", strings.Repeat("0", 255), model.Interval{}},
		{"field count boundary", strings.Repeat("at ", 24) + "1", model.Interval{Microseconds: 1_000_000}},
		{"skipped whitespace", strings.Repeat(" ", 1<<16) + "1 day", model.Interval{Days: 1}},
		{"long ISO number", "P" + strings.Repeat("0", 4096) + "1D", model.Interval{Days: 1}},
		{"many ISO fields", "PT" + strings.Repeat("0S", 1000) + "1S", model.Interval{Microseconds: 1_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), intervalInputArguments(tc.text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IntervalVal != tc.want {
				t.Fatalf("bounded input len=%d: %v, %v; want %v", len(tc.text), got, err, tc.want)
			}
		})
	}
	for _, text := range []string{strings.Repeat("0", 256), strings.Repeat("at ", 25) + "1"} {
		_, err := function.Call(t.Context(), intervalInputArguments(text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireReplacementSQLState(t, err, "22007")
	}
}

func TestIntervalInputCancellationDuringLongScans(t *testing.T) {
	function := intervalInputCallable(t, IntervalPostgres)
	for _, text := range []string{
		strings.Repeat(" ", 1<<16) + "1 day",
		"-" + strings.Repeat(" ", 1<<16) + "1 day",
		"P" + strings.Repeat("0", 1<<16) + "1D",
		"PT" + strings.Repeat("0S", 1<<15),
	} {
		ctx, cancel := context.WithCancel(t.Context())
		interrupted := &replacementCancelContext{Context: ctx, cancel: cancel, remaining: 6}
		got, err := function.Call(interrupted, intervalInputArguments(text, -1), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) || got.Type == model.TypeInterval {
			t.Errorf("cancelled input len=%d = %v, %v", len(text), got, err)
		}
	}
}
