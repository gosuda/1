package fixture

import "time"

// CompactionConfig is the fixture backend envelope alias.
type CompactionConfig = Limits

// OpenOptions configures Open in the fixture.
type OpenOptions struct {
	// Directory is the database directory. Open creates it when absent.
	Directory string
	// Compaction replaces the backend default limits when nonzero.
	Compaction CompactionConfig
	// Clock is the certified time source borrowed by the database.
	Clock any
	// HistoricalReadOnly serves retained historical snapshots only.
	HistoricalReadOnly bool
}

// CopyOptions bounds one caller-supplied COPY stream.
type CopyOptions struct {
	MaxRecordBytes uint64
	Report         func(any, any)
}

// MaintenanceConfig bounds the background maintenance loop started by Open.
type MaintenanceConfig struct {
	// Disabled stops Open from starting the loop.
	Disabled bool
	// Interval is the probe period; zero selects the default.
	Interval time.Duration
	// WALThreshold is the commit-segment count at which maintenance runs.
	WALThreshold uint64
}

// Maintenance constants declared alongside the type.
const (
	DefaultMaintenanceInterval = 30 * time.Second
	MinMaintenanceInterval     = 100 * time.Millisecond
	DefaultWALThreshold        = 64
)

// Composition-owned directory and fence names.
const (
	nativeSpoolDirectory  = "tmp"
	nativeFenceSecretFile = "fence.secret"
	nativeFenceSecretSize = 32
)
