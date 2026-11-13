package tzrule

import (
	"strings"
	"testing"
)

func TestPostgreSQLOffsetsAndDefaults(t *testing.T) {
	r, err := Parse("EST5EDT")
	if err != nil {
		t.Fatal(err)
	}
	if r.StandardOffset != -18000 || r.DaylightOffset != -14400 || !r.Daylight {
		t.Fatalf("offsets: %+v", r)
	}
	start := Transition{Kind: MonthWeekDay, Month: 3, Week: 2, Weekday: 0, Seconds: 7200}
	end := Transition{Kind: MonthWeekDay, Month: 11, Week: 1, Weekday: 0, Seconds: 7200}
	if r.Start != start || r.End != end {
		t.Fatalf("default rules: %+v", r)
	}
}

func TestPostgreSQLNonPOSIXExtensions(t *testing.T) {
	r, err := Parse("<>-167:59:60<X>+0;J365/-167:59:60,365/167:59:60")
	if err != nil {
		t.Fatal(err)
	}
	want := Rule{
		StandardName: "", DaylightName: "X", StandardOffset: 604800, DaylightOffset: 0, Daylight: true,
		Start: Transition{Kind: JulianNoLeap, Day: 365, Seconds: -604800},
		End:   Transition{Kind: DayOfYear, Day: 365, Seconds: 604800},
	}
	if r != want {
		t.Fatalf("rule: %+v", r)
	}
}

func TestPOSIXNamesAndAllocationBound(t *testing.T) {
	s := "<standard>3<daylight>2,M3.2.0,M11.1.0"
	r, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.StandardName != "standard" || r.DaylightName != "daylight" {
		t.Fatalf("parsed names: %q, %q", r.StandardName, r.DaylightName)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, err := Parse(s)
		if err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("successful parse allocations = %g", n)
	}
}

func TestRejectMalformedRules(t *testing.T) {
	for _, s := range []string{"", "UTC", "UTC168", "UTC0DST,M0.1.0,M11.1.0", "UTC0DST,M3.6.0,M11.1.0", "UTC0DST,M3.1.7,M11.1.0", "UTC0DST,J0,J365", "UTC0DST,366,0", "UTC0DST,0/1:60,1", "UTC0DST,0/1:00:61,1", "UTC0DST,0/2u,1", "UTC0DST,0,1/2s", "UTC0DST,0,1,2", "UTC0\x00", "<UTC0", "UTC0<>"} {
		t.Run(s, func(t *testing.T) {
			if _, err := Parse(s); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
}

func TestTimezoneNameLengthBoundary(t *testing.T) {
	if _, err := Parse(strings.Repeat("x", 254) + "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(strings.Repeat("x", 255) + "0"); err == nil {
		t.Fatal("accepted 256 bytes")
	}
}

func TestLowercaseDateMarkersMatchSessionLookup(t *testing.T) {
	r, err := Parse("std0dst,j60/0,m11.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Start.Kind != JulianNoLeap || r.Start.Day != 60 || r.End.Kind != MonthWeekDay || r.End.Month != 11 {
		t.Fatalf("lowercase rules: %+v", r)
	}
}
