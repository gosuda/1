package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func intervalOutputCallable(t *testing.T, style builtin.IntervalStyle) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(1161, builtin.ResolveOptions{IntervalStyle: style})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireIntervalOutput(t *testing.T, function builtin.Callable, input model.Interval, want string) model.Value {
	t.Helper()
	value, err := function.Call(t.Context(), []model.Value{{Type: model.TypeInterval, IntervalVal: input}}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.Type != model.TypeString || value.StrVal != want {
		t.Fatalf("interval_out(%+v) = %q (type %v, null %v), %v; want %q", input, value.StrVal, value.Type, value.IsNull, err, want)
	}
	return value
}

func requireIntervalOutputStyles(t *testing.T, input model.Interval, want [4]string) {
	t.Helper()
	for style, name := range [...]string{"postgres", "postgres_verbose", "sql_standard", "iso_8601"} {
		t.Run(name, func(t *testing.T) {
			requireIntervalOutput(t, intervalOutputCallable(t, builtin.IntervalStyle(style)), input, want[style])
		})
	}
}

func TestIntervalOutputCanonicalStyles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  [4]string
	}{
		{
			name: "zero",
			want: [4]string{"00:00:00", "@ 0", "0", "PT0S"},
		},
		{
			name:  "independent months days and extended hours",
			value: model.Interval{Months: 14, Days: 3, Microseconds: 90_061_120_000},
			want: [4]string{
				"1 year 2 mons 3 days 25:01:01.12",
				"@ 1 year 2 mons 3 days 25 hours 1 min 1.12 secs",
				"+1-2 +3 +25:01:01.12",
				"P1Y2M3DT25H1M1.12S",
			},
		},
		{
			name:  "mixed signs and negative singular units",
			value: model.Interval{Months: -13, Days: 1, Microseconds: -3_661_000_000},
			want: [4]string{
				"-1 years -1 mons +1 day -01:01:01",
				"@ 1 year 1 mon -1 days 1 hour 1 min 1 sec ago",
				"-1-1 +1 -1:01:01",
				"P-1Y-1M1DT-1H-1M-1S",
			},
		},
		{
			name:  "negative fraction trims only trailing zeros",
			value: model.Interval{Microseconds: -10},
			want: [4]string{
				"-00:00:00.00001",
				"@ 0.00001 secs ago",
				"-0:00:00.00001",
				"PT-0.00001S",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIntervalOutputStyles(t, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputPostgresSignsFollowLastNonzeroField(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalPostgres)
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  string
	}{
		{"positive day clears negative month", model.Interval{Months: -1, Days: 1, Microseconds: 1}, "-1 mons +1 day 00:00:00.000001"},
		{"zero day retains negative month", model.Interval{Months: -1, Microseconds: 1_000_000}, "-1 mons +00:00:01"},
		{"negative day prefixes positive clock", model.Interval{Months: 1, Days: -1, Microseconds: 1_000_000}, "1 mon -1 days +00:00:01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIntervalOutput(t, function, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputSQLStandardSignGroups(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalSQLStandard)
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  string
	}{
		{"negative year month", model.Interval{Months: -13}, "-1-1"},
		{"month without year", model.Interval{Months: 1}, "0-1"},
		{"negative day includes clock", model.Interval{Days: -1}, "-1 0:00:00"},
		{"negative day and time share sign", model.Interval{Days: -1, Microseconds: -3_661_000_000}, "-1 1:01:01"},
		{"mixed day and time force empty year month group", model.Interval{Days: -1, Microseconds: 1_000_000}, "+0-0 -1 +0:00:01"},
		{"year month and day force empty clock group", model.Interval{Months: 1, Days: 1}, "+0-1 +1 +0:00:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIntervalOutput(t, function, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputVerboseUsesFirstSignForAgo(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalPostgresVerbose)
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  string
	}{
		{"negative month reverses later positive fraction", model.Interval{Months: -1, Microseconds: 1}, "@ 1 mon -0.000001 secs ago"},
		{"positive month leaves later negative second singular", model.Interval{Months: 1, Microseconds: -1_000_000}, "@ 1 mon -1 sec"},
		{"positive month leaves later negative hour plural", model.Interval{Months: 1, Microseconds: -3_600_000_000}, "@ 1 mon -1 hours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIntervalOutput(t, function, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputIndividualMinimaRemainFinite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  [4]string
	}{
		{
			name:  "months",
			value: model.Interval{Months: math.MinInt32},
			want:  [4]string{"-178956970 years -8 mons", "@ 178956970 years 8 mons ago", "-178956970-8", "P-178956970Y-8M"},
		},
		{
			name:  "days",
			value: model.Interval{Days: math.MinInt32},
			want:  [4]string{"-2147483648 days", "@ 2147483648 days ago", "-2147483648 0:00:00", "P-2147483648D"},
		},
		{
			name:  "microseconds",
			value: model.Interval{Microseconds: math.MinInt64},
			want:  [4]string{"-2562047788:00:54.775808", "@ 2562047788 hours 54.775808 secs ago", "-2562047788:00:54.775808", "PT-2562047788H-54.775808S"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIntervalOutputStyles(t, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputInfinityIsStyleIndependent(t *testing.T) {
	t.Run("negative", func(t *testing.T) {
		requireIntervalOutputStyles(t,
			model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64},
			[4]string{"-infinity", "-infinity", "-infinity", "-infinity"})
	})
	t.Run("positive", func(t *testing.T) {
		requireIntervalOutputStyles(t,
			model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64},
			[4]string{"infinity", "infinity", "infinity", "infinity"})
	})
}

func TestIntervalOutputAdmitsExactPayloadBudget(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalPostgresVerbose)
	for _, tc := range []struct {
		name  string
		value model.Interval
		want  string
	}{
		{
			name:  "maximum verbose width",
			value: model.Interval{Months: -2_147_483_639, Days: math.MaxInt32, Microseconds: 1_234_567_890*3_600_000_000 + 59*60_000_000 + 59_123_456},
			want:  "@ 178956969 years 11 mons -2147483647 days -1234567890 hours -59 mins -59.123456 secs ago",
		},
		{
			name:  "infinity",
			value: model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64},
			want:  "-infinity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{{Type: model.TypeInterval, IntervalVal: tc.value}}
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			requireIntervalOutput(t, function, tc.value, tc.want)
		})
	}
}

func TestIntervalOutputRetainsResultsAcrossIndependentStyles(t *testing.T) {
	var functions [4]builtin.Callable
	var retained [4]model.Value
	want := [4]string{"1 mon", "@ 1 mon", "0-1", "P1M"}
	zero := [4]string{"00:00:00", "@ 0", "0", "PT0S"}
	for style := range functions {
		functions[style] = intervalOutputCallable(t, builtin.IntervalStyle(style))
		retained[style] = requireIntervalOutput(t, functions[style], model.Interval{Months: 1}, want[style])
	}
	for style, function := range functions {
		requireIntervalOutput(t, function, model.Interval{}, zero[style])
		if retained[style].StrVal != want[style] {
			t.Fatalf("later output changed retained style %d result to %q; want %q", style, retained[style].StrVal, want[style])
		}
	}
}

func TestIntervalOutputStrictNullNeedsNoOutputBudget(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalPostgres)
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeInterval)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull || value.Type != model.TypeString {
		t.Fatalf("interval_out(NULL) = %v, %v; want NULL cstring", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{{Type: model.TypeInterval}}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestIntervalOutputCancellation(t *testing.T) {
	function := intervalOutputCallable(t, builtin.IntervalPostgres)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 1}}}, builtin.CallBudget{OutputBytes: 100, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled interval_out = %v; want context cancellation", err)
	}
}

func TestIntervalOutputRejectsInvalidStyle(t *testing.T) {
	_, err := builtin.Resolve(1161, builtin.ResolveOptions{IntervalStyle: builtin.IntervalStyle(255)})
	requireSQLState(t, err, "22023")
}
