package clock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

// substituteClock is a second Clock implementation honoring the same SI
// nanosecond scale and leap-history contract. It models a provider whose raw
// source is a counter register rather than a wall clock, and whose continuity
// is unknown until qualified: reads fail closed instead of fabricating a
// certified interval.
type substituteClock struct {
	mu        sync.Mutex
	now       Instant
	scale     ScaleDescriptor
	epoch     Epoch
	qualified bool
}

func newSubstituteClock(entries ...scaleEntry) *substituteClock {
	data := make([]byte, 13+len(entries)*16)
	copy(data, "GSTL1")
	binary.BigEndian.PutUint32(data[5:9], 10)
	binary.BigEndian.PutUint32(data[9:13], uint32(len(entries)))
	for i, entry := range entries {
		binary.BigEndian.PutUint64(data[13+i*16:], uint64(entry.second))
		data[21+i*16] = byte(entry.delta)
	}
	identifier := sha256.Sum256(data)
	return &substituteClock{
		now:   1_000_000,
		scale: ScaleDescriptor{Name: ScaleName, LeapHistoryID: identifier, CanonicalLeapHistory: data},
		epoch: Epoch{Assurance: [16]byte{2}, LeapHistory: identifier, Config: [32]byte{2}},
	}
}

func (c *substituteClock) Read(ctx context.Context) (Reading, error) {
	if err := ctx.Err(); err != nil {
		return Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Unknown suspend/continuity behavior must fail qualification, not yield an
	// uncertified estimate.
	if !c.qualified {
		return Reading{}, ErrClockUnavailable
	}
	return Reading{Earliest: c.now, Latest: c.now + 500, Epoch: c.epoch, Status: Synced}, nil
}

func (c *substituteClock) WaitUntilAfter(ctx context.Context, target Instant, epoch Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return ErrClockEpochChanged
	}
	if target < 0 || target == math.MaxInt64 {
		return ErrClockRange
	}
	if !c.qualified {
		return ErrClockUnavailable
	}
	c.now = max(c.now, target+1)
	return nil
}

func (c *substituteClock) Scale() ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

// FromUTC maps POSIX seconds to SI instants by accumulating the recorded leap
// deltas, matching the canonical mapping the certified provider implements.
func (c *substituteClock) FromUTC(value time.Time) (Instant, error) {
	utc := value.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return 0, ErrClockRange
	}
	second := utc.Unix()
	var cumulative int64
	data := c.scale.CanonicalLeapHistory
	for offset := 13; offset+16 <= len(data); offset += 16 {
		transition := int64(binary.BigEndian.Uint64(data[offset:]))
		delta := int64(int8(data[offset+8]))
		// A removed label has no SI image; the inserted second is only
		// reachable through the flagged ToUTC projection, never FromUTC.
		if delta == -1 && second == transition-1 {
			return 0, ErrClockRange
		}
		if second >= transition {
			cumulative += delta
		}
	}
	si := second + cumulative
	if si < 0 || si > math.MaxInt64/1_000_000_000 || si == math.MaxInt64/1_000_000_000 && int64(utc.Nanosecond()) > math.MaxInt64%1_000_000_000 {
		return 0, ErrClockRange
	}
	return Instant(si*1_000_000_000 + int64(utc.Nanosecond())), nil
}

func (c *substituteClock) ToUTC(value Instant) (UTCProjection, error) {
	if value < 0 {
		return UTCProjection{}, ErrClockRange
	}
	si := int64(value)
	second, nanos := si/1_000_000_000, si%1_000_000_000
	var cumulative int64
	data := c.scale.CanonicalLeapHistory
	for offset := 13; offset+16 <= len(data); offset += 16 {
		transition := int64(binary.BigEndian.Uint64(data[offset:]))
		delta := int64(int8(data[offset+8]))
		transitionSI := transition + cumulative
		switch {
		case delta == 1 && second == transitionSI:
			// The inserted second presents as the repeated prior POSIX label.
			return UTCProjection{Time: time.Unix(transition-1, nanos).UTC(), LeapSecond: true}, nil
		case delta == 1 && second > transitionSI, delta == -1 && second >= transitionSI:
			cumulative += delta
		}
	}
	unix := second - cumulative
	if unix > math.MaxInt64/1_000_000_000 {
		return UTCProjection{}, ErrClockRange
	}
	return UTCProjection{Time: time.Unix(unix, nanos).UTC()}, nil
}

// TestClockScaleReplacement drives the Clock contract through two independent
// implementations and proves consumers observe identical ordering, fencing and
// conversion semantics, while incompatible scales or unqualified raw sources
// fail closed.
func TestClockScaleReplacement(t *testing.T) {
	ctx := context.Background()
	leap := scaleEntry{second: 1483228800, delta: 1}
	first := newSubstituteClock(leap)
	second := newSubstituteClock(leap)

	for _, clockImpl := range []*substituteClock{first, second} {
		// An unqualified substitute must not certify reads or fences.
		if _, err := clockImpl.Read(ctx); !errors.Is(err, ErrClockUnavailable) {
			t.Fatalf("unqualified read = %v, want ErrClockUnavailable", err)
		}
		if err := clockImpl.WaitUntilAfter(ctx, 0, clockImpl.epoch); !errors.Is(err, ErrClockUnavailable) {
			t.Fatalf("unqualified fence = %v, want ErrClockUnavailable", err)
		}
		clockImpl.qualified = true
	}

	// The commit sequencer depends only on the interface: identical targets
	// produce identical strictly-increasing fences on either implementation.
	var boundaries []Instant
	for _, clockImpl := range []*substituteClock{first, second} {
		reading, err := clockImpl.Read(ctx)
		if err != nil {
			t.Fatalf("qualified read: %v", err)
		}
		if reading.Earliest <= 0 || reading.Latest < reading.Earliest {
			t.Fatalf("invalid interval %+v", reading)
		}
		if err := clockImpl.WaitUntilAfter(ctx, reading.Latest, reading.Epoch); err != nil {
			t.Fatalf("fence: %v", err)
		}
		after, err := clockImpl.Read(ctx)
		if err != nil || after.Earliest <= reading.Latest {
			t.Fatalf("fence did not advance: %+v after %+v", after, reading)
		}
		if err := clockImpl.WaitUntilAfter(ctx, after.Latest, Epoch{}); !errors.Is(err, ErrClockEpochChanged) {
			t.Fatalf("foreign epoch fence = %v, want ErrClockEpochChanged", err)
		}
		boundaries = append(boundaries, after.Earliest)
	}
	if boundaries[0] != boundaries[1] {
		t.Fatalf("substitute clocks diverged: %d vs %d", boundaries[0], boundaries[1])
	}

	// Scale descriptors are interchangeable: each implementation's published
	// canonical history validates and extends identically.
	if err := ValidateScale(ctx, first.Scale(), 1<<20); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if err := ValidateScale(ctx, ScaleDescriptor{Name: "tai_ns_1958", LeapHistoryID: first.Scale().LeapHistoryID, CanonicalLeapHistory: first.Scale().CanonicalLeapHistory}, 1<<20); !errors.Is(err, ErrClockScale) {
		t.Fatalf("foreign timescale = %v, want ErrClockScale", err)
	}
	mutated := first.Scale()
	mutated.CanonicalLeapHistory[21] = 0xff // rewrite the leap delta
	if err := ValidateScale(ctx, mutated, 1<<20); !errors.Is(err, ErrClockScale) {
		t.Fatalf("rewritten leap mapping = %v, want ErrClockScale", err)
	}

	// Positive leap-second SI ordering: the inserted second is a distinct
	// instant strictly between the adjacent POSIX labels on both impls.
	before := time.Date(2016, 12, 31, 23, 59, 59, 0, time.UTC)
	after := time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, clockImpl := range []*substituteClock{first, second} {
		prior, err := clockImpl.FromUTC(before)
		if err != nil {
			t.Fatalf("pre-leap: %v", err)
		}
		next, err := clockImpl.FromUTC(after)
		if err != nil {
			t.Fatalf("post-leap: %v", err)
		}
		if next-prior != 2_000_000_000 {
			t.Fatalf("leap gap = %d ns, want one inserted second", next-prior)
		}
		// The flagged POSIX presentation: the leap instant projects back to the
		// repeated 23:59:59 label with LeapSecond set.
		projection, err := clockImpl.ToUTC(prior + 1_000_000_000)
		if err != nil || !projection.LeapSecond || !projection.Time.Equal(before) {
			t.Fatalf("leap projection = %+v, %v", projection, err)
		}
		ordinary, err := clockImpl.ToUTC(next)
		if err != nil || ordinary.LeapSecond || !ordinary.Time.Equal(after) {
			t.Fatalf("ordinary projection = %+v, %v", ordinary, err)
		}
	}

	// Overflow boundaries reject on both directions.
	if _, err := first.FromUTC(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrClockRange) {
		t.Fatalf("year-10000 conversion = %v, want ErrClockRange", err)
	}
	if _, err := first.ToUTC(-1); !errors.Is(err, ErrClockRange) {
		t.Fatalf("negative instant = %v, want ErrClockRange", err)
	}
	if err := first.WaitUntilAfter(ctx, math.MaxInt64, first.epoch); !errors.Is(err, ErrClockRange) {
		t.Fatalf("overflow fence = %v, want ErrClockRange", err)
	}

	// Future leap-table extension is legal only strictly beyond the protected
	// instant; rewriting history at or below it is rejected before readiness.
	extended := scaleFixture(scaleEntry{second: 1483228800, delta: 1}, scaleEntry{second: 2000000000, delta: -1})
	protected, err := first.FromUTC(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateScaleExtension(ctx, first.Scale(), extended, protected, 1<<20); err != nil {
		t.Fatalf("future extension: %v", err)
	}
	lowProtected, err := first.FromUTC(time.Date(2033, 5, 18, 3, 33, 21, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateScaleExtension(ctx, first.Scale(), extended, lowProtected, 1<<20); !errors.Is(err, ErrClockScale) {
		t.Fatalf("extension inside protected history = %v, want ErrClockScale", err)
	}

	// Old Unix-to-SI copy-upgrade conversion: legacy commit timestamps recorded
	// as Unix nanoseconds map through FromUTC identically on both substitutes.
	legacyCommitTS := time.Date(2020, 6, 1, 12, 0, 0, 123456789, time.UTC)
	a, err := first.FromUTC(legacyCommitTS)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.FromUTC(legacyCommitTS)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("copy-upgrade conversion diverged: %d vs %d", a, b)
	}
}
