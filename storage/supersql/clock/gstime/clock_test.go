package gstimeclock

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/gstime"
	rawclock "gosuda.org/gstime/clock"

	"github.com/gosuda/supersql-alpha/clock"
)

func testHistory(t *testing.T, entries ...gstime.LeapEntry) *gstime.LeapHistory {
	t.Helper()
	history, err := gstime.NewLeapHistory(10, entries)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func testService(t *testing.T, history *gstime.LeapHistory) (*gstime.ClockService, *rawclock.SimulatedRawClock, clock.Clock) {
	t.Helper()
	raw := rawclock.NewSimulatedRawClock(1_000_000_000)
	raw.SetScaleEnvelope(gstime.RateScale(gstime.OneQ48), gstime.RateScale(gstime.OneQ48))
	service := gstime.NewClockService(raw, history, [32]byte{1}, 1_000_000)
	adapter, err := New(service, history)
	if err != nil {
		t.Fatal(err)
	}
	return service, raw, adapter
}

func anchor(t *testing.T, service *gstime.ClockService, raw *rawclock.SimulatedRawClock) {
	t.Helper()
	now := raw.Read().Raw
	if err := service.PublishAssuranceRound(now, gstime.TimeInterval{Earliest: 1000, Latest: 1100}, 0, 1, 1, 1, now+1_000_000_000); err != nil {
		t.Fatal(err)
	}
}

func TestCertifiedClockFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, raw, adapter := testService(t, testHistory(t))
		anchor(t, service, raw)
		reading, err := adapter.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- adapter.WaitUntilAfter(ctx, reading.Earliest, reading.Epoch) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("equal fence returned %v; equality is not strict-after", err)
		default:
		}
		// Both anchor and current raw read contribute a 100ns error bound.
		raw.Advance(201)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		fresh, err := adapter.Read(t.Context())
		if err != nil || fresh.Earliest != reading.Earliest+1 {
			t.Fatalf("strict-after reading = %+v, %v", fresh, err)
		}
	})
}

func TestFenceCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, raw, adapter := testService(t, testHistory(t))
		anchor(t, service, raw)
		reading, err := adapter.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		done := make(chan error, 1)
		go func() { done <- adapter.WaitUntilAfter(ctx, clock.Instant(math.MaxInt64), reading.Epoch) }()
		synctest.Wait()
		cause := errors.New("transaction stopped")
		cancel(cause)
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("wait error = %v, want cancellation cause", err)
		}
	})
}

func TestFreshReadRejectsExpiredAnchor(t *testing.T) {
	service, raw, adapter := testService(t, testHistory(t))
	if _, err := adapter.Read(t.Context()); !errors.Is(err, gstime.ErrUnanchored) || !errors.Is(err, clock.ErrClockUnavailable) {
		t.Fatalf("unanchored read = %v", err)
	}
	anchor(t, service, raw)
	reading, err := adapter.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	watermark, _, err := service.PastWatermark(reading.Epoch.Assurance, reading.Epoch.LeapHistory, reading.Epoch.Config)
	if err != nil || watermark <= 0 {
		t.Fatalf("published watermark = %d, %v", watermark, err)
	}
	raw.Advance(1_000_000_001)
	if _, err := adapter.Read(t.Context()); !errors.Is(err, clock.ErrClockUnavailable) {
		t.Fatalf("fresh read retained expired evidence: %v", err)
	}
	if err := adapter.WaitUntilAfter(t.Context(), clock.Instant(watermark)-1, reading.Epoch); !errors.Is(err, clock.ErrClockUnavailable) || !errors.Is(err, gstime.ErrBoundExpired) {
		t.Fatalf("expired fence = %v", err)
	}
}

func TestFreshReadRejectsRawDiscontinuity(t *testing.T) {
	cases := []struct {
		name   string
		change func(*rawclock.SimulatedRawClock)
	}{
		{"token changed", func(raw *rawclock.SimulatedRawClock) { raw.SetContinuityToken(2) }},
		{"raw rewound", func(raw *rawclock.SimulatedRawClock) { raw.Rewind(1) }},
		{"scale changed", func(raw *rawclock.SimulatedRawClock) {
			raw.SetScaleEnvelope(gstime.RateScale(gstime.OneQ48-1), gstime.RateScale(gstime.OneQ48+1))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, raw, adapter := testService(t, testHistory(t))
			anchor(t, service, raw)
			tc.change(raw)
			if _, err := adapter.Read(t.Context()); !errors.Is(err, clock.ErrClockUnavailable) || !errors.Is(err, gstime.ErrRawFault) {
				t.Fatalf("discontinuous read = %v", err)
			}
		})
	}
}

func TestFenceRequiresExactEpoch(t *testing.T) {
	service, raw, adapter := testService(t, testHistory(t))
	anchor(t, service, raw)
	reading, err := adapter.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assurance", "history", "config"} {
		t.Run(field, func(t *testing.T) {
			epoch := reading.Epoch
			switch field {
			case "assurance":
				epoch.Assurance[0] ^= 1
			case "history":
				epoch.LeapHistory[0] ^= 1
			case "config":
				epoch.Config[0] ^= 1
			}
			if err := adapter.WaitUntilAfter(t.Context(), 0, epoch); !errors.Is(err, clock.ErrClockEpochChanged) {
				t.Fatalf("mismatched fence = %v", err)
			}
		})
	}
}

func TestCertifiedSampleValidation(t *testing.T) {
	service, raw, adapter := testService(t, testHistory(t))
	anchor(t, service, raw)
	c := adapter.(*certifiedClock)
	c.maxAge, c.maxHoldoverAge, c.maxWidth = 1000, 2000, 1000
	cases := []struct {
		name   string
		change func(*gstime.AssuredNow)
		want   error
	}{
		{"negative bound", func(s *gstime.AssuredNow) { s.Interval.Earliest = -1 }, clock.ErrClockRange},
		{"reversed bounds", func(s *gstime.AssuredNow) { s.Interval.Latest = 0 }, clock.ErrClockRange},
		{"missing interval", func(s *gstime.AssuredNow) { s.HasInterval = false }, clock.ErrClockUnavailable},
		{"expired age", func(s *gstime.AssuredNow) { s.Age = 1001 }, gstime.ErrBoundExpired},
		{"width cap", func(s *gstime.AssuredNow) { s.Interval.Latest = s.Interval.Earliest + 1000 }, gstime.ErrBoundExpired},
		{"expired holdover", func(s *gstime.AssuredNow) { s.Status, s.Age = gstime.StatusHoldover, 2001 }, gstime.ErrBoundExpired},
		{"changed assurance", func(s *gstime.AssuredNow) { s.AssuranceEpochID[0] ^= 1 }, clock.ErrClockEpochChanged},
		{"changed history", func(s *gstime.AssuredNow) { s.LeapHistoryID[0] ^= 1 }, gstime.ErrLeapHistoryMismatch},
		{"changed configuration", func(s *gstime.AssuredNow) { s.ConfigID[0] ^= 1 }, gstime.ErrConfigurationMismatch},
		{"arithmetic fault", func(s *gstime.AssuredNow) {
			s.HasInterval, s.Status, s.Reason = false, gstime.StatusDesync, gstime.ReasonArithmeticOverflow
		}, gstime.ErrArithmeticFault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sample := service.Now()
			tc.change(&sample)
			if _, err := c.validate(sample); !errors.Is(err, tc.want) {
				t.Fatalf("validation = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLeapConversionAndOwnedScale(t *testing.T) {
	history := testHistory(t, gstime.LeapEntry{TransitionUnixSecond: 1483228800, Delta: 1})
	_, _, adapter := testService(t, history)
	before := time.Date(2016, 12, 31, 23, 59, 59, 42, time.UTC)
	after := time.Date(2017, 1, 1, 0, 0, 0, 42, time.UTC)
	first, err := adapter.FromUTC(before)
	if err != nil || first != 1483228799000000042 {
		t.Fatalf("before leap = %d, %v", first, err)
	}
	last, err := adapter.FromUTC(after)
	if err != nil || last != 1483228801000000042 {
		t.Fatalf("after leap = %d, %v", last, err)
	}
	leap, err := adapter.ToUTC(first + 1_000_000_000)
	if err != nil || !leap.LeapSecond || !leap.Time.Equal(before) {
		t.Fatalf("leap projection = %+v, %v", leap, err)
	}
	ordinary, err := adapter.ToUTC(last)
	if err != nil || ordinary.LeapSecond || !ordinary.Time.Equal(after) {
		t.Fatalf("ordinary projection = %+v, %v", ordinary, err)
	}
	scale := adapter.Scale()
	scale.CanonicalLeapHistory[0] = 0
	history.Entries[0].Delta = -1
	fresh := adapter.Scale()
	if err := clock.ValidateScale(t.Context(), fresh, 1<<20); err != nil {
		t.Fatalf("scale alias corrupted history: %v", err)
	}
	if got, err := adapter.FromUTC(after); err != nil || got != last {
		t.Fatalf("history mutation changed conversion: %d, %v", got, err)
	}
}

func TestConversionBoundaries(t *testing.T) {
	_, _, adapter := testService(t, testHistory(t))
	for _, instant := range []clock.Instant{0, clock.Instant(math.MaxInt64)} {
		projection, err := adapter.ToUTC(instant)
		if err != nil {
			t.Fatal(err)
		}
		got, err := adapter.FromUTC(projection.Time)
		if err != nil || got != instant || projection.LeapSecond {
			t.Fatalf("boundary %d roundtrip = %d, %+v, %v", instant, got, projection, err)
		}
	}
	maxTime := time.Unix(9223372036, 854775807).UTC()
	for _, value := range []time.Time{time.Unix(-1, 999999999), maxTime.Add(time.Nanosecond), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(-10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := adapter.FromUTC(value); !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("FromUTC(%s) = %v", value, err)
		}
	}
	if _, err := adapter.ToUTC(-1); !errors.Is(err, clock.ErrClockRange) {
		t.Fatalf("negative instant = %v", err)
	}
}

func TestNegativeLeapRejectsRemovedLabel(t *testing.T) {
	_, _, adapter := testService(t, testHistory(t, gstime.LeapEntry{TransitionUnixSecond: 1483228800, Delta: -1}))
	removed := time.Date(2016, 12, 31, 23, 59, 59, 0, time.UTC)
	if _, err := adapter.FromUTC(removed); !errors.Is(err, clock.ErrClockRange) {
		t.Fatalf("removed label = %v", err)
	}
	got, err := adapter.FromUTC(removed.Add(time.Second))
	if err != nil || got != 1483228799000000000 {
		t.Fatalf("negative leap boundary = %d, %v", got, err)
	}
}

func TestLeapOffsetOverflow(t *testing.T) {
	_, _, adapter := testService(t, testHistory(t, gstime.LeapEntry{TransitionUnixSecond: 1483228800, Delta: 1}))
	maximum, err := adapter.ToUTC(clock.Instant(math.MaxInt64))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.FromUTC(maximum.Time); err != nil || got != clock.Instant(math.MaxInt64) {
		t.Fatalf("maximum SI conversion = %d, %v", got, err)
	}
	if _, err := adapter.FromUTC(maximum.Time.Add(time.Nanosecond)); !errors.Is(err, clock.ErrClockRange) {
		t.Fatalf("SI overflow after leap = %v", err)
	}
}

func TestNewRejectsInvalidHistory(t *testing.T) {
	for _, second := range []int64{-62135683200, 253402300800} {
		history := testHistory(t, gstime.LeapEntry{TransitionUnixSecond: second, Delta: 1})
		raw := rawclock.NewSimulatedRawClock(1_000_000_000)
		service := gstime.NewClockService(raw, history, [32]byte{1}, 1_000_000)
		if _, err := New(service, history); !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("transition %d construction = %v", second, err)
		}
	}
	// gstime.NewLeapHistory itself rejects non-midnight transitions, so 1483228801
	// never reaches the adapter's civil-range check.
	if _, err := gstime.NewLeapHistory(10, []gstime.LeapEntry{{TransitionUnixSecond: 1483228801, Delta: 1}}); err == nil {
		t.Fatal("non-midnight leap transition was accepted by gstime")
	}
	history := testHistory(t)
	service, _, _ := testService(t, history)
	history.ID[0] ^= 1
	if _, err := New(service, history); !errors.Is(err, gstime.ErrLeapHistoryMismatch) {
		t.Fatalf("mutated history construction = %v", err)
	}
}
