package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestamptzAgeUsesLocalFieldsAndAbsoluteOrder(t *testing.T) {
	call, err := builtin.Resolve(1199, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		left, right model.Value
		want        model.Interval
	}{
		{
			name:  "fold reverses local clocks",
			left:  timestampCalendarDatum(t, 2024, 11, 3, 6, 15, 0),
			right: timestampCalendarDatum(t, 2024, 11, 3, 5, 45, 0),
			want:  model.Interval{Months: -1, Days: 29, Microseconds: 84600000000},
		},
		{
			name:  "spring day has twenty three elapsed hours",
			left:  timestampCalendarDatum(t, 2024, 3, 10, 16, 0, 0),
			right: timestampCalendarDatum(t, 2024, 3, 9, 17, 0, 0),
			want:  model.Interval{Days: 1},
		},
		{
			name:  "autumn day has twenty five elapsed hours",
			left:  timestampCalendarDatum(t, 2024, 11, 3, 17, 0, 0),
			right: timestampCalendarDatum(t, 2024, 11, 2, 16, 0, 0),
			want:  model.Interval{Days: 1},
		},
		{
			name:  "borrows April rather than May",
			left:  timestampCalendarDatum(t, 2004, 6, 1, 16, 0, 0),
			right: timestampCalendarDatum(t, 2004, 4, 30, 16, 0, 0),
			want:  model.Interval{Months: 1, Days: 1},
		},
		{
			name:  "borrows leap February across DST",
			left:  timestampCalendarDatum(t, 2024, 3, 28, 16, 0, 0),
			right: timestampCalendarDatum(t, 2024, 2, 29, 17, 0, 0),
			want:  model.Interval{Days: 28},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := call.Call(t.Context(), []model.Value{test.left, test.right}, builtin.CallBudget{})
			if err != nil || got.Type != model.TypeInterval || got.IntervalVal != test.want {
				t.Fatalf("age = %+v,%v; want %+v", got, err, test.want)
			}
			wantReverse := model.Interval{Months: -test.want.Months, Days: -test.want.Days, Microseconds: -test.want.Microseconds}
			got, err = call.Call(t.Context(), []model.Value{test.right, test.left}, builtin.CallBudget{})
			if err != nil || got.Type != model.TypeInterval || got.IntervalVal != wantReverse {
				t.Fatalf("reverse age = %+v,%v; want %+v", got, err, wantReverse)
			}
		})
	}
}

func TestTimestamptzAgeInfinity(t *testing.T) {
	call := jsonCallable(t, 1199)
	for _, test := range []struct {
		left, right int64
		positive    bool
	}{
		{math.MaxInt64, 0, true},
		{0, math.MinInt64, true},
		{math.MaxInt64, math.MinInt64, true},
		{math.MinInt64, 0, false},
		{0, math.MaxInt64, false},
		{math.MinInt64, math.MaxInt64, false},
	} {
		got, err := call.Call(t.Context(), []model.Value{timestampDatum(test.left), timestampDatum(test.right)}, builtin.CallBudget{})
		want := model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
		if test.positive {
			want = model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
		}
		if err != nil || got.Type != model.TypeInterval || got.IntervalVal != want {
			t.Fatalf("age(%d,%d) = %+v,%v; want %+v", test.left, test.right, got, err, want)
		}
	}
	for _, value := range []int64{math.MinInt64, math.MaxInt64} {
		_, err := call.Call(t.Context(), []model.Value{timestampDatum(value), timestampDatum(value)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}

func TestTimestamptzAgeValidatesUTCNotLocalBounds(t *testing.T) {
	const minimum int64 = -211813488000000000
	const end int64 = 9223371331200000000
	for _, test := range []struct {
		name        string
		zone        builtin.TimeZoneContext
		left, right int64
	}{
		{"local day before minimum", builtin.TimeZoneContext{Name: "<-12>12", Fixed: true, OffsetSeconds: -43200}, minimum + 1, minimum},
		{"local day beyond maximum", builtin.TimeZoneContext{Name: "<+14>-14", Fixed: true, OffsetSeconds: 50400}, end - 1, end - 2},
		{"rounded UTC beyond input maximum", builtin.TimeZoneContext{Name: "<+14>-14", Fixed: true, OffsetSeconds: 50400}, end, end - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			call, err := builtin.Resolve(1199, builtin.ResolveOptions{TimeZone: test.zone})
			if err != nil {
				t.Fatal(err)
			}
			got, err := call.Call(t.Context(), []model.Value{timestampDatum(test.left), timestampDatum(test.right)}, builtin.CallBudget{})
			if err != nil || got.Type != model.TypeInterval || got.IntervalVal != (model.Interval{Microseconds: 1}) {
				t.Fatalf("boundary age = %+v,%v; want one microsecond", got, err)
			}
			for _, args := range [][]model.Value{{timestampDatum(minimum - 1), timestampDatum(0)}, {timestampDatum(0), timestampDatum(minimum - 1)}} {
				_, err := call.Call(t.Context(), args, builtin.CallBudget{})
				requireSQLState(t, err, "22008")
			}
		})
	}
}
