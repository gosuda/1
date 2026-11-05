// Package clock defines certified continuous SI time independently of a provider.
package clock

import (
	"context"
	"errors"
	"time"
)

// Instant counts SI nanoseconds since 1970-01-01T00:00:00 UTC, including leap seconds.
type Instant int64

type Epoch struct {
	Assurance   [16]byte
	LeapHistory [32]byte
	Config      [32]byte
}

type Status uint8

const (
	Synced Status = iota + 1
	Holdover
)

type Reading struct {
	Earliest, Latest Instant
	Epoch            Epoch
	Status           Status
}

const ScaleName = "si_ns_1970"

type ScaleDescriptor struct {
	Name                 string
	LeapHistoryID        [32]byte
	CanonicalLeapHistory []byte
}

type UTCProjection struct {
	Time       time.Time
	LeapSecond bool
}

var (
	ErrClockUnavailable  = errors.New("certified clock unavailable")
	ErrClockEpochChanged = errors.New("certified clock epoch changed")
	ErrClockRange        = errors.New("certified clock value out of range")
)

// Clock is borrowed by the database; its host owns provider startup and shutdown.
// Reads and waits must be concurrency-safe and honor cancellation without
// per-request network polling. Failed assurance must never return an estimate.
type Clock interface {
	// Read returns a fresh nonnegative closed interval in Synced or Holdover
	// status, validated against the provider's width, raw age and continuity bounds.
	Read(context.Context) (Reading, error)
	// WaitUntilAfter requires a fresh valid sample in exactly epoch proving
	// Earliest > target. Equality, elapsed timers and stale watermarks do not suffice.
	WaitUntilAfter(context.Context, Instant, Epoch) error
	// Scale returns owned canonical GSTL1 bytes; mutating them cannot alter the provider.
	Scale() ScaleDescriptor
	FromUTC(time.Time) (Instant, error)
	ToUTC(Instant) (UTCProjection, error)
}
