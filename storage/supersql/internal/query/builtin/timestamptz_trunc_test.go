package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestamptzTruncTransitionOffsets(t *testing.T) {
	for _, tc := range []struct {
		name, zone, unit                                   string
		year, month, day, hour, minute                     int64
		wantYear, wantMonth, wantDay, wantHour, wantMinute int64
	}{
		{"first fold hour", "America/New_York", "hour", 2024, 11, 3, 5, 45, 2024, 11, 3, 5, 0},
		{"second fold hour", "America/New_York", "hour", 2024, 11, 3, 6, 45, 2024, 11, 3, 6, 0},
		{"half hour fold", "Australia/Lord_Howe", "hour", 2024, 4, 6, 15, 15, 2024, 4, 6, 14, 30},
		{"spring day offset", "America/New_York", "day", 2024, 3, 10, 16, 0, 2024, 3, 10, 5, 0},
		{"spring midnight gap", "America/Santiago", "day", 2019, 9, 8, 15, 0, 2019, 9, 8, 4, 0},
		{"dynamic abbreviation full zone", "MSK", "day", 2010, 7, 1, 12, 0, 2010, 6, 30, 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := timestampCalendarDatum(t, tc.year, tc.month, tc.day, tc.hour, tc.minute, 59.123456)
			want := timestampCalendarDatum(t, tc.wantYear, tc.wantMonth, tc.wantDay, tc.wantHour, tc.wantMinute, 0)
			for _, id := range []model.FunctionID{1217, 1284} {
				zone := tc.zone
				args := []model.Value{model.StringVal(tc.unit), input}
				if id == 1284 {
					zone = "Asia/Tokyo"
					args = append(args, model.StringVal(tc.zone))
				}
				// MSK is a dynamic abbreviation for an explicit argument, not a session zone name.
				if id == 1217 && tc.zone == "MSK" {
					zone = "Europe/Moscow"
				}
				call, err := builtin.Resolve(id, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: zone}})
				if err != nil {
					t.Fatal(err)
				}
				got, err := call.Call(t.Context(), args, builtin.CallBudget{})
				if err != nil || got.Type != model.TypeTimestamp || got.IntVal != want.IntVal {
					t.Fatalf("date_trunc %d = %v,%v; want %d", id, got, err, want.IntVal)
				}
			}
		})
	}
}

func TestTimestamptzTruncUTCCalendarParity(t *testing.T) {
	inputs := []model.Value{
		timestampCalendarDatum(t, 2021, 1, 1, 23, 59, 59.999999),
		timestampCalendarDatum(t, -2, 5, 1, 23, 59, 59.999999),
		timestampDatum(-1),
	}
	for _, unit := range []string{"millennium", "century", "decade", "year", "quarter", "month", "week", "day", "hour", "minute", "second", "milliseconds", "microseconds"} {
		for _, input := range inputs {
			want, err := timestampCall(t, 2020, model.StringVal(unit), input)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []model.FunctionID{1217, 1284} {
				args := []model.Value{model.StringVal(unit), input}
				if id == 1284 {
					args = append(args, model.StringVal("UTC"))
				}
				got, err := timestampCall(t, id, args...)
				if err != nil || got.IntVal != want.IntVal {
					t.Fatalf("date_trunc %d (%s,%d) = %v,%v; want %d", id, unit, input.IntVal, got, err, want.IntVal)
				}
			}
		}
	}
}

func TestTimestamptzTruncLocalCalendarOutsideInputBounds(t *testing.T) {
	const minimum int64 = -211813488000000000
	const end int64 = 9223371331200000000
	for _, tc := range []struct {
		zone, unit  string
		input, want int64
		state       string
	}{
		{"<-30>30", "hour", minimum, minimum, ""},
		{"<-30>30", "microseconds", minimum, minimum, ""},
		{"<-30>30", "day", minimum, 0, "22008"},
		{"UTC", "week", minimum, minimum, ""},
		{"UTC", "month", minimum, 0, "22008"},
		{"<+30>-30", "microseconds", end - 1, end - 1, ""},
		{"<+30>-30", "day", end - 1, end - 6*3600000000, ""},
		{"UTC", "microseconds", end, 0, "22008"},
		{"UTC", "microseconds", math.MaxInt64 - 1, 0, "22008"},
	} {
		got, err := timestampCall(t, 1284, model.StringVal(tc.unit), timestampDatum(tc.input), model.StringVal(tc.zone))
		if tc.state != "" {
			requireSQLState(t, err, tc.state)
		} else if err != nil || got.IntVal != tc.want {
			t.Fatalf("date_trunc(%s,%d,%s) = %v,%v; want %d", tc.unit, tc.input, tc.zone, got, err, tc.want)
		}
	}
}

func TestTimestamptzTruncErrorPrecedence(t *testing.T) {
	for _, input := range []int64{0, math.MinInt64, math.MaxInt64} {
		_, err := timestampCall(t, 1284, model.StringVal("timezone"), timestampDatum(input), model.StringVal("missing/zone"))
		requireSQLState(t, err, "22023")
		for _, id := range []model.FunctionID{1217, 1284} {
			for _, tc := range []struct{ unit, state string }{{"timezone", "0A000"}, {"epoch", "22023"}, {"unknown_unit", "22023"}} {
				args := []model.Value{model.StringVal(tc.unit), timestampDatum(input)}
				if id == 1284 {
					args = append(args, model.StringVal("UTC"))
				}
				_, err := timestampCall(t, id, args...)
				requireSQLState(t, err, tc.state)
			}
		}
		if input != 0 {
			got, err := timestampCall(t, 1284, model.StringVal("WeEk"), timestampDatum(input), model.StringVal("UTC"))
			if err != nil || got.IntVal != input {
				t.Fatalf("infinite truncation = %v,%v; want %d", got, err, input)
			}
		}
	}
	_, err := timestampCall(t, 1217, model.StringVal("timezone"), timestampDatum(math.MinInt64+1))
	requireSQLState(t, err, "22008")
	_, err = timestampCall(t, 1217, model.StringVal("epoch"), timestampDatum(math.MinInt64+1))
	requireSQLState(t, err, "22023")
}

func TestTimestamptzTruncOutputRetainsFoldSide(t *testing.T) {
	options := builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
	trunc, err := builtin.Resolve(1217, options)
	if err != nil {
		t.Fatal(err)
	}
	output, err := builtin.Resolve(1151, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hour int64
		want string
	}{{5, "2024-11-03 01:00:00-04"}, {6, "2024-11-03 01:00:00-05"}} {
		input := timestampCalendarDatum(t, 2024, 11, 3, tc.hour, 45, 0)
		value, err := trunc.Call(t.Context(), []model.Value{model.StringVal("hour"), input}, builtin.CallBudget{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := output.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64})
		if err != nil || got.StrVal != tc.want {
			t.Fatalf("fold output = %v,%v; want %s", got, err, tc.want)
		}
	}
}
