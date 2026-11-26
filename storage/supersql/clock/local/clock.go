// Package localclock provides a standalone single-process clock.Clock backed by
// the host OS wall and monotonic clocks. It requires no external NTP or PTP
// synchronization and is intended for single-process embedded databases.
package localclock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
)

const (
	nanosPerSecond     = 1_000_000_000
	maxSISecond        = math.MaxInt64 / nanosPerSecond
	maxSINano          = math.MaxInt64 % nanosPerSecond
	initialTAIMinusUTC = 10
)

var _ clock.Clock = (*Clock)(nil)

type leapEntry struct {
	transitionUnixSecond int64
	delta                int8
}

type config struct {
	initialFloor clock.Instant
	uncertainty  time.Duration
	now          func() time.Time
	entries      []leapEntry
}

// Option configures a local Clock constructed by New.
type Option func(*config)

// WithInitialFloor sets a minimum starting SI instant for the clock.
// Negative values are clamped to zero.
func WithInitialFloor(floor clock.Instant) Option {
	return func(cfg *config) {
		if floor < 0 {
			floor = 0
		}
		cfg.initialFloor = floor
	}
}

// WithUncertainty configures the interval width (Latest - Earliest) returned by
// Read. Negative values are clamped to zero. The default is zero.
func WithUncertainty(d time.Duration) Option {
	return func(cfg *config) {
		if d < 0 {
			d = 0
		}
		cfg.uncertainty = d
	}
}

func withNow(fn func() time.Time) Option {
	return func(cfg *config) {
		if fn != nil {
			cfg.now = fn
		}
	}
}

func withLeapEntries(entries []leapEntry) Option {
	return func(cfg *config) {
		cfg.entries = append([]leapEntry(nil), entries...)
	}
}

// Clock is a concurrency-safe standalone clock.Clock backed by the host OS wall
// and monotonic clocks with a strict monotonic high-water mark.
type Clock struct {
	scale       clock.ScaleDescriptor
	entries     []leapEntry
	epoch       clock.Epoch
	uncertainty time.Duration
	now         func() time.Time

	mu         sync.Mutex
	anchorTime time.Time
	anchorInst clock.Instant
	highWater  clock.Instant
}

// New constructs a standalone local clock with safe zero-config defaults.
func New(opts ...Option) *Clock {
	cfg := config{
		now: time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	scale := encodeGSTL1(initialTAIMinusUTC, cfg.entries)
	epoch := deriveEpoch(scale.LeapHistoryID, cfg.uncertainty)

	c := &Clock{
		scale:       scale,
		entries:     cfg.entries,
		epoch:       epoch,
		uncertainty: cfg.uncertainty,
		now:         cfg.now,
	}

	now := c.now()
	var anchor clock.Instant
	if inst, err := c.FromUTC(now); err == nil {
		anchor = inst
	}
	if cfg.initialFloor > anchor {
		anchor = cfg.initialFloor
	}
	c.anchorTime = now
	c.anchorInst = anchor
	c.highWater = anchor
	return c
}

func encodeGSTL1(initialTAI uint32, entries []leapEntry) clock.ScaleDescriptor {
	data := make([]byte, 13+len(entries)*16)
	copy(data, "GSTL1")
	binary.BigEndian.PutUint32(data[5:9], initialTAI)
	binary.BigEndian.PutUint32(data[9:13], uint32(len(entries)))
	for i, entry := range entries {
		offset := 13 + i*16
		binary.BigEndian.PutUint64(data[offset:offset+8], uint64(entry.transitionUnixSecond))
		data[offset+8] = byte(entry.delta)
		// Bytes [offset+9 : offset+16] remain zero-initialized (reserved).
	}
	return clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(data),
		CanonicalLeapHistory: data,
	}
}

func deriveEpoch(leapHistoryID [32]byte, uncertainty time.Duration) clock.Epoch {
	configHash := sha256.New()
	configHash.Write([]byte("supersql/clock/local/v1/config\x00"))
	var width [8]byte
	binary.BigEndian.PutUint64(width[:], uint64(uncertainty))
	configHash.Write(width[:])
	var configID [32]byte
	configHash.Sum(configID[:0])

	assuranceHash := sha256.New()
	assuranceHash.Write([]byte("supersql/clock/local/v1/assurance\x00"))
	assuranceHash.Write(leapHistoryID[:])
	assuranceHash.Write(configID[:])
	var full [32]byte
	assuranceHash.Sum(full[:0])

	var assuranceID [16]byte
	copy(assuranceID[:], full[:16])

	return clock.Epoch{
		Assurance:   assuranceID,
		LeapHistory: leapHistoryID,
		Config:      configID,
	}
}

func contextError(ctx context.Context) error {
	return context.Cause(ctx)
}

// Read returns a fresh nonnegative closed interval [Earliest, Latest] in
// clock.Synced status. Readings never move backward even if the OS wall clock
// steps backward.
func (c *Clock) Read(ctx context.Context) (clock.Reading, error) {
	if err := contextError(ctx); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sampleLocked(c.now())
}

func (c *Clock) sampleLocked(now time.Time) (clock.Reading, error) {
	elapsed := now.Sub(c.anchorTime)
	if elapsed < 0 {
		elapsed = 0
	}
	if int64(c.anchorInst) > math.MaxInt64-int64(elapsed) {
		return clock.Reading{}, clock.ErrClockRange
	}
	monoInst := c.anchorInst + clock.Instant(elapsed)

	earliest := monoInst
	wallInst, err := c.FromUTC(now)
	if err == nil {
		if wallInst > earliest {
			earliest = wallInst
			c.anchorTime = now
			c.anchorInst = earliest
		}
	} else if now.UTC().Year() >= 1970 {
		return clock.Reading{}, err
	}

	if earliest < c.highWater {
		earliest = c.highWater
	} else {
		c.highWater = earliest
	}

	if int64(earliest) > math.MaxInt64-int64(c.uncertainty) {
		return clock.Reading{}, clock.ErrClockRange
	}
	latest := earliest + clock.Instant(c.uncertainty)

	return clock.Reading{
		Earliest: earliest,
		Latest:   latest,
		Epoch:    c.epoch,
		Status:   clock.Synced,
	}, nil
}

// WaitUntilAfter blocks until a fresh reading in epoch proves Earliest > target,
// or until ctx is canceled.
func (c *Clock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if target < 0 || target == math.MaxInt64 {
		return clock.ErrClockRange
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		reading, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if reading.Epoch != epoch {
			return clock.ErrClockEpochChanged
		}
		if reading.Earliest > target {
			return nil
		}
		remaining := target - reading.Earliest + 1
		delay := time.Duration(min(int64(remaining), int64(5*time.Millisecond)))
		if delay < 50*time.Microsecond {
			delay = 50 * time.Microsecond
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return contextError(ctx)
		case <-timer.C:
		}
	}
}

// Scale returns owned canonical GSTL1 bytes; mutating them cannot alter the clock.
func (c *Clock) Scale() clock.ScaleDescriptor {
	owned := c.scale
	owned.CanonicalLeapHistory = bytes.Clone(c.scale.CanonicalLeapHistory)
	return owned
}

// FromUTC converts a UTC timestamp to its SI nanosecond instant under this
// clock's canonical GSTL1 leap history.
func (c *Clock) FromUTC(value time.Time) (clock.Instant, error) {
	utc := value.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return 0, clock.ErrClockRange
	}
	second := utc.Unix()
	var cumulative int64
	for _, entry := range c.entries {
		if entry.delta == -1 && second == entry.transitionUnixSecond-1 {
			return 0, clock.ErrClockRange
		}
		if second >= entry.transitionUnixSecond {
			cumulative += int64(entry.delta)
		}
	}
	siSecond := second + cumulative
	nanos := int64(utc.Nanosecond())
	if siSecond < 0 || siSecond > maxSISecond || (siSecond == maxSISecond && nanos > maxSINano) {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(siSecond*nanosPerSecond + nanos), nil
}

// ToUTC projects an SI nanosecond instant onto UTC under this clock's canonical
// GSTL1 leap history. An inserted positive leap second projects onto the
// preceding UTC second with LeapSecond set.
func (c *Clock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	if value < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	si := int64(value)
	siSecond := si / nanosPerSecond
	nanos := si % nanosPerSecond

	var cumulative int64
	for _, entry := range c.entries {
		if entry.delta == 1 {
			leapSI := entry.transitionUnixSecond + cumulative
			if siSecond == leapSI {
				utc := time.Unix(entry.transitionUnixSecond-1, nanos).UTC()
				if utc.Year() < 1 || utc.Year() > 9999 {
					return clock.UTCProjection{}, clock.ErrClockRange
				}
				return clock.UTCProjection{Time: utc, LeapSecond: true}, nil
			}
		}
		transitionSI := entry.transitionUnixSecond + cumulative + int64(entry.delta)
		if siSecond >= transitionSI {
			cumulative += int64(entry.delta)
		}
	}

	unixSecond := siSecond - cumulative
	utc := time.Unix(unixSecond, nanos).UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: utc, LeapSecond: false}, nil
}
