// Package clock defines certified continuous SI time independently of a provider.
package clock

import (
	"context"
	"errors"
	"time"
)

// Instant counts SI nanoseconds since 1970-01-01T00:00:00 UTC, including leap seconds.
type Instant int64

// Epoch identifies a continuous certified-clock assurance epoch, leap-second
// history, and provider configuration.
type Epoch struct {
	Assurance   [16]byte
	LeapHistory [32]byte
	Config      [32]byte
}

// Status reports the synchronization state of a certified clock reading.
type Status uint8

// Certified clock synchronization states.
const (
	Synced Status = iota + 1
	Holdover
)

// Reading is a certified closed SI interval [Earliest, Latest] with its epoch
// and synchronization status.
type Reading struct {
	Earliest, Latest Instant
	Epoch            Epoch
	Status           Status
}

// ScaleName is the canonical identifier for the continuous SI nanosecond scale.
const ScaleName = "si_ns_1970"

// ScaleDescriptor carries the SI scale name and canonical GSTL1 leap-second
// history bytes with their SHA-256 identity.
type ScaleDescriptor struct {
	Name                 string
	LeapHistoryID        [32]byte
	CanonicalLeapHistory []byte
}

// UTCProjection is the civil UTC projection of an SI Instant, with LeapSecond
// set when the instant falls inside a positive leap second.
type UTCProjection struct {
	Time       time.Time
	LeapSecond bool
}

// Sentinel errors returned by certified Clock implementations.
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
