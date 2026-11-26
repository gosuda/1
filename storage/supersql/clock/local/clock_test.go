package localclock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
)

func TestScaleValidationAndOwnership(t *testing.T) {
	ctx := t.Context()
	c := New()

	scale := c.Scale()
	if scale.Name != clock.ScaleName {
		t.Fatalf("scale name = %q, want %q", scale.Name, clock.ScaleName)
	}
	if len(scale.CanonicalLeapHistory) < 13 || string(scale.CanonicalLeapHistory[:5]) != "GSTL1" {
		t.Fatalf("unexpected canonical GSTL1 header: %q", scale.CanonicalLeapHistory)
	}
	if want := sha256.Sum256(scale.CanonicalLeapHistory); scale.LeapHistoryID != want {
		t.Fatalf("LeapHistoryID = %x, want %x", scale.LeapHistoryID, want)
	}
	if err := clock.ValidateScale(ctx, scale, 1<<20); err != nil {
		t.Fatalf("ValidateScale: %v", err)
	}

	// Mutating the returned bytes must not alter the clock's internal descriptor.
	original := bytes.Clone(scale.CanonicalLeapHistory)
	scale.CanonicalLeapHistory[0] = 'X'
	fresh := c.Scale()
	if !bytes.Equal(fresh.CanonicalLeapHistory, original) {
		t.Fatalf("Scale() aliased internal slice: got %x, want %x", fresh.CanonicalLeapHistory, original)
	}
	if err := clock.ValidateScale(ctx, fresh, 1<<20); err != nil {
		t.Fatalf("ValidateScale after caller mutation: %v", err)
	}

	// Extending the canonical history strictly beyond the protected instant must succeed.
	futureEntry := leapEntry{transitionUnixSecond: 2_000_000_000, delta: 1}
	extended := encodeGSTL1(initialTAIMinusUTC, []leapEntry{futureEntry})
	protected, err := c.FromUTC(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromUTC: %v", err)
	}
	if err := clock.ValidateScaleExtension(ctx, fresh, extended, protected, 1<<20); err != nil {
		t.Fatalf("ValidateScaleExtension: %v", err)
	}
}

func TestReadPropertiesAndDeterministicEpoch(t *testing.T) {
	ctx := t.Context()
	a := New()
	b := New()

	ra, err := a.Read(ctx)
	if err != nil {
		t.Fatalf("Read(a): %v", err)
	}
	rb, err := b.Read(ctx)
	if err != nil {
		t.Fatalf("Read(b): %v", err)
	}

	for _, r := range []clock.Reading{ra, rb} {
		if r.Status != clock.Synced {
			t.Fatalf("status = %v, want Synced", r.Status)
		}
		if r.Earliest < 0 || r.Latest < r.Earliest {
			t.Fatalf("invalid interval: %+v", r)
		}
		if r.Epoch.Assurance == ([16]byte{}) || r.Epoch.Config == ([32]byte{}) || r.Epoch.LeapHistory == ([32]byte{}) {
			t.Fatalf("zero field in epoch: %+v", r.Epoch)
		}
		if r.Epoch.LeapHistory != a.Scale().LeapHistoryID {
			t.Fatalf("epoch leap history = %x, want %x", r.Epoch.LeapHistory, a.Scale().LeapHistoryID)
		}
	}
	if ra.Epoch != rb.Epoch {
		t.Fatalf("default epochs not deterministic: %+v vs %+v", ra.Epoch, rb.Epoch)
	}
	if ra.Latest != ra.Earliest {
		t.Fatalf("default uncertainty width = %d, want 0", ra.Latest-ra.Earliest)
	}

	// Explicit uncertainty produces exact interval width and distinct config epoch.
	width := 2 * time.Millisecond
	widened := New(WithUncertainty(width))
	rw, err := widened.Read(ctx)
	if err != nil {
		t.Fatalf("Read(widened): %v", err)
	}
	if got := time.Duration(rw.Latest - rw.Earliest); got != width {
		t.Fatalf("interval width = %v, want %v", got, width)
	}
	if rw.Epoch == ra.Epoch || rw.Epoch.Config == ra.Epoch.Config {
		t.Fatalf("distinct uncertainty reused config epoch: %+v", rw.Epoch)
	}

	// Negative options clamp safely to zero.
	clamped := New(WithInitialFloor(-100), WithUncertainty(-5*time.Second))
	rc, err := clamped.Read(ctx)
	if err != nil {
		t.Fatalf("Read(clamped): %v", err)
	}
	if rc.Earliest < 0 || rc.Latest != rc.Earliest {
		t.Fatalf("clamped reading = %+v", rc)
	}

	// Canceled context fails closed with the context cause.
	cause := errors.New("read aborted")
	canceledCtx, cancel := context.WithCancelCause(ctx)
	cancel(cause)
	if _, err := a.Read(canceledCtx); !errors.Is(err, cause) {
		t.Fatalf("canceled Read = %v, want %v", err, cause)
	}
}

func TestMonotonicityUnderWallClockStepBackAndForward(t *testing.T) {
	ctx := t.Context()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	var mu sync.Mutex
	current := base
	c := New(withNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}))

	first, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("initial Read: %v", err)
	}
	if want := clock.Instant(base.UnixNano()); first.Earliest != want {
		t.Fatalf("initial Earliest = %d, want %d", first.Earliest, want)
	}

	// Wall clock steps backward by 1 hour: readings must never go backward.
	mu.Lock()
	current = base.Add(-time.Hour)
	mu.Unlock()
	afterStepBack, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read after step back: %v", err)
	}
	if afterStepBack.Earliest < first.Earliest {
		t.Fatalf("clock moved backward: %d < %d", afterStepBack.Earliest, first.Earliest)
	}

	// Wall clock steps forward by 2 hours: readings advance and re-anchor.
	mu.Lock()
	current = base.Add(2 * time.Hour)
	mu.Unlock()
	afterStepForward, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read after step forward: %v", err)
	}
	if want := clock.Instant(base.Add(2 * time.Hour).UnixNano()); afterStepForward.Earliest != want {
		t.Fatalf("forward Earliest = %d, want %d", afterStepForward.Earliest, want)
	}

	// Stepping back again after a forward step still holds the new high-water mark.
	mu.Lock()
	current = base.Add(time.Hour)
	mu.Unlock()
	afterSecondStepBack, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read after second step back: %v", err)
	}
	if afterSecondStepBack.Earliest < afterStepForward.Earliest {
		t.Fatalf("clock moved backward after forward jump: %d < %d", afterSecondStepBack.Earliest, afterStepForward.Earliest)
	}

	// Pre-1970 wall clock still yields nonnegative monotonic instants.
	mu.Lock()
	current = time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)
	mu.Unlock()
	preEpochClock := New(withNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}))
	rPre, err := preEpochClock.Read(ctx)
	if err != nil {
		t.Fatalf("pre-1970 Read: %v", err)
	}
	if rPre.Earliest < 0 || rPre.Latest < rPre.Earliest {
		t.Fatalf("invalid pre-1970 reading: %+v", rPre)
	}

	// Overflow beyond MaxInt64 returns ErrClockRange instead of wrapping negative.
	overflowClock := New(WithInitialFloor(clock.Instant(math.MaxInt64)-10), WithUncertainty(time.Second))
	if _, err := overflowClock.Read(ctx); !errors.Is(err, clock.ErrClockRange) {
		t.Fatalf("overflow Read = %v, want ErrClockRange", err)
	}
}

func TestInitialFloorAdvancesMonotonically(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		futureFloor := clock.Instant(time.Now().Add(time.Hour).UnixNano())
		c := New(WithInitialFloor(futureFloor))

		r1, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("Read 1: %v", err)
		}
		if r1.Earliest < futureFloor {
			t.Fatalf("Earliest = %d, want >= %d", r1.Earliest, futureFloor)
		}

		// Even though wall time is an hour behind futureFloor, OS monotonic
		// progression advances readings smoothly above futureFloor.
		time.Sleep(10 * time.Millisecond)
		r2, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("Read 2: %v", err)
		}
		if r2.Earliest <= r1.Earliest {
			t.Fatalf("clock did not advance above floor: %d <= %d", r2.Earliest, r1.Earliest)
		}
	})
}

func TestMonotonicityUnderConcurrency(t *testing.T) {
	ctx := t.Context()
	c := New(WithUncertainty(500 * time.Microsecond))
	expectedEpoch := c.epoch

	const workers = 16
	const iterations = 2000

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var prevEarliest, prevLatest clock.Instant
			for i := range iterations {
				r, err := c.Read(ctx)
				if err != nil {
					errs <- err
					return
				}
				if r.Status != clock.Synced {
					errs <- errors.New("unexpected non-synced status")
					return
				}
				if r.Epoch != expectedEpoch {
					errs <- errors.New("epoch changed unexpectedly")
					return
				}
				if r.Earliest < 0 || r.Latest < r.Earliest {
					errs <- errors.New("invalid reading bounds")
					return
				}
				if i > 0 && (r.Earliest < prevEarliest || r.Latest < prevLatest) {
					errs <- errors.New("non-monotonic reading observed")
					return
				}
				prevEarliest, prevLatest = r.Earliest, r.Latest
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestWaitUntilAfter(t *testing.T) {
	t.Run("strict_after_and_real_advance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx := t.Context()
			c := New(WithUncertainty(2 * time.Millisecond))
			r, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}

			// Waiting until after Latest must not return until Earliest > Latest.
			if err := c.WaitUntilAfter(ctx, r.Latest, r.Epoch); err != nil {
				t.Fatalf("WaitUntilAfter: %v", err)
			}
			fresh, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Earliest <= r.Latest {
				t.Fatalf("fresh.Earliest = %d, want > %d", fresh.Earliest, r.Latest)
			}
		})
	})

	t.Run("equality_does_not_satisfy_wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx := t.Context()
			base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
			var mu sync.Mutex
			now := base
			c := New(withNow(func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}))

			r, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				done <- c.WaitUntilAfter(ctx, r.Earliest, r.Epoch)
			}()
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("WaitUntilAfter returned early on equality: %v", err)
			default:
			}

			// Advancing by 1ns strictly exceeds target and unblocks the wait.
			mu.Lock()
			now = base.Add(time.Nanosecond)
			mu.Unlock()
			if err := <-done; err != nil {
				t.Fatalf("WaitUntilAfter after advance: %v", err)
			}
		})
	})

	t.Run("epoch_mismatch", func(t *testing.T) {
		ctx := t.Context()
		c := New()
		r, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name   string
			mutate func(*clock.Epoch)
		}{
			{"zero", func(e *clock.Epoch) { *e = clock.Epoch{} }},
			{"assurance", func(e *clock.Epoch) { e.Assurance[0] ^= 1 }},
			{"leap_history", func(e *clock.Epoch) { e.LeapHistory[0] ^= 1 }},
			{"config", func(e *clock.Epoch) { e.Config[0] ^= 1 }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				epoch := r.Epoch
				tc.mutate(&epoch)
				if err := c.WaitUntilAfter(ctx, 0, epoch); !errors.Is(err, clock.ErrClockEpochChanged) {
					t.Fatalf("WaitUntilAfter(%s) = %v, want ErrClockEpochChanged", tc.name, err)
				}
			})
		}
	})

	t.Run("range_errors", func(t *testing.T) {
		ctx := t.Context()
		c := New()
		r, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []clock.Instant{-1, clock.Instant(math.MaxInt64)} {
			if err := c.WaitUntilAfter(ctx, target, r.Epoch); !errors.Is(err, clock.ErrClockRange) {
				t.Fatalf("WaitUntilAfter(%d) = %v, want ErrClockRange", target, err)
			}
		}
	})

	t.Run("context_cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := New()
			r, err := c.Read(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			cause := errors.New("caller canceled fence")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)

			done := make(chan error, 1)
			go func() {
				done <- c.WaitUntilAfter(ctx, r.Earliest+clock.Instant(time.Hour), r.Epoch)
			}()
			synctest.Wait()
			cancel(cause)
			if err := <-done; !errors.Is(err, cause) {
				t.Fatalf("canceled WaitUntilAfter = %v, want %v", err, cause)
			}
		})
	})

	t.Run("context_deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := New()
			r, err := c.Read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
			defer cancel()
			if err := c.WaitUntilAfter(ctx, r.Earliest+clock.Instant(time.Hour), r.Epoch); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline WaitUntilAfter = %v, want context.DeadlineExceeded", err)
			}
		})
	})
}

func TestFromUTCToUTCRoundtripsAndBoundaries(t *testing.T) {
	c := New()

	// Exact roundtrips at boundaries, arbitrary timestamps, and non-UTC zones.
	kst := time.FixedZone("KST", 9*3600)
	samples := []time.Time{
		time.Unix(0, 0).UTC(),
		time.Date(2026, 11, 21, 3, 44, 3, 123456789, time.UTC),
		time.Date(2026, 11, 21, 12, 44, 3, 987654321, kst),
		time.Unix(maxSISecond, maxSINano).UTC(),
	}
	for _, ts := range samples {
		inst, err := c.FromUTC(ts)
		if err != nil {
			t.Fatalf("FromUTC(%v): %v", ts, err)
		}
		proj, err := c.ToUTC(inst)
		if err != nil {
			t.Fatalf("ToUTC(%d): %v", inst, err)
		}
		if proj.LeapSecond {
			t.Fatalf("unexpected LeapSecond for %v", ts)
		}
		if !proj.Time.Equal(ts.UTC()) || proj.Time.Location() != time.UTC {
			t.Fatalf("roundtrip(%v) = %v (%v), want %v UTC", ts, proj.Time, proj.Time.Location(), ts.UTC())
		}
	}

	// Boundary instants 0 and MaxInt64 roundtrip exactly.
	for _, inst := range []clock.Instant{0, clock.Instant(math.MaxInt64)} {
		proj, err := c.ToUTC(inst)
		if err != nil {
			t.Fatalf("ToUTC(%d): %v", inst, err)
		}
		back, err := c.FromUTC(proj.Time)
		if err != nil || back != inst || proj.LeapSecond {
			t.Fatalf("boundary(%d) roundtrip = %d, %+v, %v", inst, back, proj, err)
		}
	}

	// Out-of-range conversions fail with ErrClockRange.
	maxTime := time.Unix(maxSISecond, maxSINano).UTC()
	for _, invalid := range []time.Time{
		time.Unix(-1, 999_999_999).UTC(),
		maxTime.Add(time.Nanosecond),
		time.Date(0, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(-10000, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := c.FromUTC(invalid); !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("FromUTC(%v) = %v, want ErrClockRange", invalid, err)
		}
	}
	if _, err := c.ToUTC(-1); !errors.Is(err, clock.ErrClockRange) {
		t.Fatalf("ToUTC(-1) = %v, want ErrClockRange", err)
	}
}

func TestLeapHistoryProjectionConventions(t *testing.T) {
	t.Run("positive_leap", func(t *testing.T) {
		c := New(withLeapEntries([]leapEntry{{transitionUnixSecond: 1483228800, delta: 1}}))
		if err := clock.ValidateScale(t.Context(), c.Scale(), 1<<20); err != nil {
			t.Fatalf("ValidateScale: %v", err)
		}
		if got := binary.BigEndian.Uint32(c.Scale().CanonicalLeapHistory[9:13]); got != 1 {
			t.Fatalf("entry count = %d, want 1", got)
		}

		before := time.Date(2016, 12, 31, 23, 59, 59, 42, time.UTC)
		after := time.Date(2017, 1, 1, 0, 0, 0, 42, time.UTC)

		first, err := c.FromUTC(before)
		if err != nil || first != 1483228799_000000042 {
			t.Fatalf("before leap = %d, %v", first, err)
		}
		last, err := c.FromUTC(after)
		if err != nil || last != 1483228801_000000042 {
			t.Fatalf("after leap = %d, %v", last, err)
		}
		leapProj, err := c.ToUTC(first + nanosPerSecond)
		if err != nil || !leapProj.LeapSecond || !leapProj.Time.Equal(before) {
			t.Fatalf("leap projection = %+v, %v", leapProj, err)
		}
		ordinaryProj, err := c.ToUTC(last)
		if err != nil || ordinaryProj.LeapSecond || !ordinaryProj.Time.Equal(after) {
			t.Fatalf("ordinary projection = %+v, %v", ordinaryProj, err)
		}

		// Overflow boundary shifts down by 1s after a positive leap.
		maximum, err := c.ToUTC(clock.Instant(math.MaxInt64))
		if err != nil {
			t.Fatalf("ToUTC(MaxInt64): %v", err)
		}
		if back, err := c.FromUTC(maximum.Time); err != nil || back != clock.Instant(math.MaxInt64) {
			t.Fatalf("MaxInt64 roundtrip = %d, %v", back, err)
		}
		if _, err := c.FromUTC(maximum.Time.Add(time.Nanosecond)); !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("overflow after leap = %v, want ErrClockRange", err)
		}
	})

	t.Run("negative_leap", func(t *testing.T) {
		c := New(withLeapEntries([]leapEntry{{transitionUnixSecond: 1483228800, delta: -1}}))
		if err := clock.ValidateScale(t.Context(), c.Scale(), 1<<20); err != nil {
			t.Fatalf("ValidateScale: %v", err)
		}
		removed := time.Date(2016, 12, 31, 23, 59, 59, 0, time.UTC)
		if _, err := c.FromUTC(removed); !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("removed second = %v, want ErrClockRange", err)
		}
		after := removed.Add(time.Second)
		inst, err := c.FromUTC(after)
		if err != nil || inst != 1483228799_000000000 {
			t.Fatalf("post-negative-leap instant = %d, %v", inst, err)
		}
		proj, err := c.ToUTC(inst)
		if err != nil || proj.LeapSecond || !proj.Time.Equal(after) {
			t.Fatalf("post-negative-leap ToUTC = %+v, %v", proj, err)
		}
	})
}
