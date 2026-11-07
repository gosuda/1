package builtin_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func namedZoneStamp(year int, month time.Month, day, hour, minute int) int64 {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC).UnixMicro() - 946684800000000
}

func TestNamedTimezoneCivilTransitions(t *testing.T) {
	call := jsonCallable(t, 2069)
	for _, tc := range []struct {
		name, zone string
		civil, utc int64
	}{
		{"spring gap", "America/New_York", namedZoneStamp(2024, 3, 10, 2, 30), namedZoneStamp(2024, 3, 10, 7, 30)},
		{"autumn fold", "America/New_York", namedZoneStamp(2024, 11, 3, 1, 30), namedZoneStamp(2024, 11, 3, 6, 30)},
		{"southern half hour gap", "Australia/Lord_Howe", namedZoneStamp(2024, 10, 6, 2, 15), namedZoneStamp(2024, 10, 5, 15, 45)},
		{"negative DST fold", "Europe/Dublin", namedZoneStamp(2024, 10, 27, 1, 30), namedZoneStamp(2024, 10, 27, 1, 30)},
		{"dateline gap", "Pacific/Apia", namedZoneStamp(2011, 12, 30, 12, 0), namedZoneStamp(2011, 12, 30, 22, 0)},
		{"historical seconds", "Europe/Paris", namedZoneStamp(1900, 1, 1, 12, 0), namedZoneStamp(1900, 1, 1, 12, 0) - 561000000},
		{"future recurrence", "America/New_York", namedZoneStamp(10000, 7, 1, 12, 0), namedZoneStamp(10000, 7, 1, 16, 0)},
		{"POSIX southern recurrence", "AEST-10AEDT-11,M10.1.0,M4.1.0/3", namedZoneStamp(2400, 1, 15, 12, 0), namedZoneStamp(2400, 1, 15, 1, 0)},
		{"POSIX perpetual daylight", "AAA0BBB,J1/0,J365/26", namedZoneStamp(2024, 7, 1, 12, 0), namedZoneStamp(2024, 7, 1, 11, 0)},
		{"case folded POSIX rule", "aaa0bbb,m3.2.0,m11.1.0", namedZoneStamp(2400, 7, 1, 12, 0), namedZoneStamp(2400, 7, 1, 11, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.zone), timestampDatum(tc.civil + 123456)}, builtin.CallBudget{})
			if err != nil || got.IntVal != tc.utc+123456 {
				t.Fatalf("timezone(%s): got %d, %v; want %d", tc.zone, got.IntVal, err, tc.utc+123456)
			}
		})
	}
}

func TestNamedTimezoneAbbreviationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, zone, argument string
		year                 int
		offset               int64
	}{
		{"default IST", "UTC", "IST", 2024, 7200},
		{"session overrides default", "Asia/Kolkata", "IST", 2024, 19800},
		{"session historical LMT", "Europe/Paris", "LMT", 2024, 561},
		{"dynamic MSK old meaning", "UTC", "MSK", 2012, 14400},
		{"dynamic MSK new meaning", "UTC", "MSK", 2024, 10800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := builtin.Resolve(1159, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: tc.zone}})
			if err != nil {
				t.Fatal(err)
			}
			stamp := namedZoneStamp(tc.year, 7, 1, 12, 0)
			got, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.argument), timestampDatum(stamp)}, builtin.CallBudget{})
			if err != nil || got.IntVal != stamp+tc.offset*1000000 {
				t.Fatalf("got %d, %v; want %d", got.IntVal, err, stamp+tc.offset*1000000)
			}
		})
	}
}

func TestNamedTimezoneInfinityBoundsAndCancellation(t *testing.T) {
	for _, id := range []model.FunctionID{1159, 2069} {
		call := jsonCallable(t, id)
		for _, infinity := range []int64{math.MinInt64, math.MaxInt64} {
			got, err := call.Call(t.Context(), []model.Value{model.StringVal("not/a/zone"), timestampDatum(infinity)}, builtin.CallBudget{})
			if err != nil || got.IntVal != infinity {
				t.Fatalf("infinity: %v, %v", got, err)
			}
		}
		_, err := call.Call(t.Context(), []model.Value{model.StringVal("not/a/zone"), timestampDatum(0)}, builtin.CallBudget{})
		requireSQLState(t, err, "22023")
		_, err = call.Call(t.Context(), []model.Value{model.StringVal("UTC"), timestampDatum(9223371331200000000)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = call.Call(ctx, []model.Value{model.StringVal("UTC"), timestampDatum(0)}, builtin.CallBudget{})
		if err != context.Canceled {
			t.Fatalf("cancelled call: %v", err)
		}
	}
	for _, zone := range []builtin.TimeZoneContext{{Name: "not/a/zone"}, {Name: "ABC3:30:30"}, {Fixed: true, OffsetSeconds: 604801}} {
		_, err := builtin.Resolve(1159, builtin.ResolveOptions{TimeZone: zone})
		requireSQLState(t, err, "22023")
	}
}

func TestNamedTimezoneFixedContextIdentity(t *testing.T) {
	for _, tc := range []struct {
		zone builtin.TimeZoneContext
		name string
		want int64
	}{
		{builtin.TimeZoneContext{Name: "Etc/GMT-5", Fixed: true, OffsetSeconds: 18000}, "+05", 18000000000},
		{builtin.TimeZoneContext{Name: "<+30>-30", Fixed: true, OffsetSeconds: 108000}, "+30", 108000000000},
		{builtin.TimeZoneContext{Name: "<>3", Fixed: true, OffsetSeconds: -10800}, "", -10800000000},
	} {
		call, err := builtin.Resolve(1159, builtin.ResolveOptions{TimeZone: tc.zone})
		if err != nil {
			t.Fatal(err)
		}
		got, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.name), timestampDatum(0)}, builtin.CallBudget{})
		if err != nil || got.IntVal != tc.want {
			t.Fatalf("fixed context %s: %d, %v; want %d", tc.zone.Name, got.IntVal, err, tc.want)
		}
	}
}
