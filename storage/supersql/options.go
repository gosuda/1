package supersql

import (
	"cmp"
	"context"
	"errors"
	"math"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// defaultCollation is the native build's one collation (C, OID 950). Key
// encoding, comparison and the default catalog destination all use it until the
// catalog can publish per-column collations.
const defaultCollation model.CollationID = 950

// Object identifiers owned by this composition. They sit below the identity
// high-water floor reserved during bootstrap, so user declarations can never
// collide with a composition-owned collection.
const (
	nativeSequenceObjectID   = model.ObjectID(90)
	nativeIdentityObjectID   = model.ObjectID(91)
	nativeStatisticsObjectID = model.ObjectID(92)
	nativeIndexObjectID      = model.ObjectID(4)
)

const (
	nativeMaxOperations   = 64
	nativeMaxTransactions = 64
	nativeSpoolDirectory  = "tmp"
	nativeFenceSecretFile = "fence.secret"
	nativeFenceSecretSize = 32
	// nativeCleanupTimeout bounds the sequence publication cleanup statement that
	// runs when a DDL statement aborts.
	nativeCleanupTimeout = 1 * time.Second
)

var (
	// ErrClockRequired reports a missing or typed-nil Clock. The native stack has
	// no local-estimate or offline-write fallback, so a database cannot be opened
	// without a certified clock.
	ErrClockRequired = errors.New("supersql: Open requires an explicit clock.Clock")
	// ErrDirectoryRequired reports an empty OpenOptions.Directory.
	ErrDirectoryRequired = errors.New("supersql: Open requires a database directory")
	// ErrDatabaseClosed reports use of a database after Close.
	ErrDatabaseClosed = errors.New("supersql: database is closed")
	// ErrMissingStatement reports SQL text that parsed to no statement.
	ErrMissingStatement = errors.New("supersql: no statement to execute")
)

// UpgradeRequired reports a directory that holds a legacy database. Open never
// writes to such a directory: the format is detected before any constructor runs
// and the host must run the offline copy-upgrade instead.
type UpgradeRequired struct {
	Directory string
	Cause     error
}

// Error returns the upgrade-required message.
func (e *UpgradeRequired) Error() string {
	return "supersql: " + e.Directory + " holds a legacy database that requires an offline upgrade"
}

// Unwrap keeps the storage layer's own format verdict reachable.
func (e *UpgradeRequired) Unwrap() error { return e.Cause }

// CompactionConfig carries the storage backend resource envelope, including
// resident handles, WAL, compaction run preparation, read budgets, and plan
// budgets. Any zero field inherits its DefaultCompactionConfig value when
// passed to Open or RestoreCheckpoint, so callers may override individual
// fields without specifying the entire struct.
type CompactionConfig struct {
	ResidentBytes     uint64
	MaxFiles          uint64
	MaxPins           uint64
	MetadataBytes     uint64
	RecoveryFiles     uint64
	IdentityBytes     uint64
	WALBytes          uint64
	WALAllocation     uint64
	MutationBytes     uint64
	WriteBytes        uint64
	SortBytes         uint64
	BlockBytes        uint64
	DecodedBlockBytes uint64
	ReadBlockBytes    uint64
	BlockRows         int
	RunBytes          uint64
	RunIndexBytes     uint64
	RunBlocks         uint32
	Builders          int
	Runs              uint64
	PreparationBytes  uint64
	PublicationBytes  uint64
	ReadSourceBytes   uint64
	ReadMergeBytes    uint64
	KeyBytes          uint64
	ScanBytes         uint64
	RowBytes          uint64
	PredicateBytes    uint64
	OverlayBytes      uint64
	OverlayBlockBytes uint64
	PlanBytes         uint64
	BatchRows         int
	PredicateNodes    int
	Plans             int
}

// DefaultCompactionConfig returns the default storage backend resource envelope.
func DefaultCompactionConfig() CompactionConfig {
	def := neosepia.DefaultLimits()
	return CompactionConfig{
		ResidentBytes:     def.ResidentBytes,
		MaxFiles:          def.MaxFiles,
		MaxPins:           def.MaxPins,
		MetadataBytes:     def.MetadataBytes,
		RecoveryFiles:     def.RecoveryFiles,
		IdentityBytes:     def.IdentityBytes,
		WALBytes:          def.WALBytes,
		WALAllocation:     def.WALAllocation,
		MutationBytes:     def.MutationBytes,
		WriteBytes:        def.WriteBytes,
		SortBytes:         def.SortBytes,
		BlockBytes:        def.BlockBytes,
		DecodedBlockBytes: def.DecodedBlockBytes,
		ReadBlockBytes:    def.ReadBlockBytes,
		BlockRows:         def.BlockRows,
		RunBytes:          def.RunBytes,
		RunIndexBytes:     def.RunIndexBytes,
		RunBlocks:         def.RunBlocks,
		Builders:          def.Builders,
		Runs:              def.Runs,
		PreparationBytes:  def.PreparationBytes,
		PublicationBytes:  def.PublicationBytes,
		ReadSourceBytes:   def.ReadSourceBytes,
		ReadMergeBytes:    def.ReadMergeBytes,
		KeyBytes:          def.KeyBytes,
		ScanBytes:         def.ScanBytes,
		RowBytes:          def.RowBytes,
		PredicateBytes:    def.PredicateBytes,
		OverlayBytes:      def.OverlayBytes,
		OverlayBlockBytes: def.OverlayBlockBytes,
		PlanBytes:         def.PlanBytes,
		BatchRows:         def.BatchRows,
		PredicateNodes:    def.PredicateNodes,
		Plans:             def.Plans,
	}
}

// mergeCompactionConfig overlays any nonzero fields from cfg onto the storage
// backend defaults. Negative integer fields are preserved so backend validation
// still rejects invalid overrides.
func mergeCompactionConfig(cfg CompactionConfig) neosepia.Limits {
	def := neosepia.DefaultLimits()
	merged := neosepia.Limits{
		ResidentBytes:     cmp.Or(cfg.ResidentBytes, def.ResidentBytes),
		MaxFiles:          cmp.Or(cfg.MaxFiles, def.MaxFiles),
		MaxPins:           cmp.Or(cfg.MaxPins, def.MaxPins),
		MetadataBytes:     cmp.Or(cfg.MetadataBytes, def.MetadataBytes),
		RecoveryFiles:     cmp.Or(cfg.RecoveryFiles, def.RecoveryFiles),
		IdentityBytes:     cmp.Or(cfg.IdentityBytes, def.IdentityBytes),
		WALBytes:          cmp.Or(cfg.WALBytes, def.WALBytes),
		WALAllocation:     cmp.Or(cfg.WALAllocation, def.WALAllocation),
		MutationBytes:     cmp.Or(cfg.MutationBytes, def.MutationBytes),
		WriteBytes:        cmp.Or(cfg.WriteBytes, def.WriteBytes),
		SortBytes:         cmp.Or(cfg.SortBytes, def.SortBytes),
		BlockBytes:        cmp.Or(cfg.BlockBytes, def.BlockBytes),
		DecodedBlockBytes: cmp.Or(cfg.DecodedBlockBytes, def.DecodedBlockBytes),
		ReadBlockBytes:    cmp.Or(cfg.ReadBlockBytes, def.ReadBlockBytes),
		BlockRows:         cmp.Or(cfg.BlockRows, def.BlockRows),
		RunBytes:          cmp.Or(cfg.RunBytes, def.RunBytes),
		RunIndexBytes:     cmp.Or(cfg.RunIndexBytes, def.RunIndexBytes),
		RunBlocks:         cmp.Or(cfg.RunBlocks, def.RunBlocks),
		Builders:          cmp.Or(cfg.Builders, def.Builders),
		Runs:              cmp.Or(cfg.Runs, def.Runs),
		PreparationBytes:  cmp.Or(cfg.PreparationBytes, def.PreparationBytes),
		PublicationBytes:  cmp.Or(cfg.PublicationBytes, def.PublicationBytes),
		ReadSourceBytes:   cmp.Or(cfg.ReadSourceBytes, def.ReadSourceBytes),
		ReadMergeBytes:    cmp.Or(cfg.ReadMergeBytes, def.ReadMergeBytes),
		KeyBytes:          cmp.Or(cfg.KeyBytes, def.KeyBytes),
		ScanBytes:         cmp.Or(cfg.ScanBytes, def.ScanBytes),
		RowBytes:          cmp.Or(cfg.RowBytes, def.RowBytes),
		PredicateBytes:    cmp.Or(cfg.PredicateBytes, def.PredicateBytes),
		OverlayBytes:      cmp.Or(cfg.OverlayBytes, def.OverlayBytes),
		OverlayBlockBytes: cmp.Or(cfg.OverlayBlockBytes, def.OverlayBlockBytes),
		PlanBytes:         cmp.Or(cfg.PlanBytes, def.PlanBytes),
		BatchRows:         cmp.Or(cfg.BatchRows, def.BatchRows),
		PredicateNodes:    cmp.Or(cfg.PredicateNodes, def.PredicateNodes),
		Plans:             cmp.Or(cfg.Plans, def.Plans),
	}
	if cfg.ReadBlockBytes == 0 && merged.BlockBytes <= math.MaxUint64-merged.DecodedBlockBytes {
		if needed := merged.BlockBytes + merged.DecodedBlockBytes; merged.ReadBlockBytes < needed {
			merged.ReadBlockBytes = needed
		}
	}
	return merged
}

// OpenOptions configures Open. Clock is required and borrowed: the host owns
// provider startup and shutdown and must close the database before closing the
// provider, because a live database holds fences certified by that provider.
type OpenOptions struct {
	// Directory is the database directory. Open creates it when absent.
	Directory string
	// Compaction overrides individual storage backend resource limits when
	// nonzero; zero fields inherit DefaultCompactionConfig().
	Compaction CompactionConfig
	// Clock is the certified time source borrowed by the database.
	Clock clock.Clock
	// Maintenance bounds the background reclamation loop Open starts. The zero
	// value selects the defaults; Maintenance.Disabled stops the loop.
	Maintenance MaintenanceConfig
	// HistoryRetention configures the durable MVCC history retention window.
	// Zero selects the default (24h); negative values are rejected by Open.
	HistoryRetention time.Duration
	// HistoricalReadOnly opens the database without requiring fresh certified
	// readiness: no writes and no latest reads are accepted, only retained
	// historical snapshots are served. The clock may be unsynchronized.
	HistoricalReadOnly bool
	// Parallelism sets the execution worker pool concurrency. Zero or negative
	// selects runtime.GOMAXPROCS(0).
	Parallelism int
	// MemoryBudgetBytes overrides the shared query memory budget (default 256 MiB).
	MemoryBudgetBytes uint64
	// TransactionMemoryBytes overrides the per-transaction in-memory staging limit (default 8 MiB).
	TransactionMemoryBytes uint64
	// TransactionSpillBytes overrides the per-transaction spill limit (default 32 MiB).
	TransactionSpillBytes uint64
	// MaxTransactions overrides the maximum concurrent active transactions (default 64).
	MaxTransactions int
	// MaxOperations overrides the maximum concurrent snapshot/read operations (default 64).
	MaxOperations int
	// Observer receives one QueryEvent after each completed SQL statement.
	// Nil disables per-query observer notifications.
	Observer QueryObserver
	// SlowQueryThreshold marks statements whose duration meets or exceeds this
	// value as slow queries. Zero disables slow-query tracking; negative values
	// are rejected by Open.
	SlowQueryThreshold time.Duration
	// OnSlowQuery is invoked when a statement's duration meets or exceeds
	// SlowQueryThreshold. Nil disables the callback while still incrementing
	// DatabaseMetrics.SlowQueriesTotal when SlowQueryThreshold > 0.
	OnSlowQuery func(context.Context, QueryEvent)
	// EncryptionKey enables optional pure-Go AES-256-GCM at-rest encryption for
	// newly written SST runs, WAL files, and encrypted backups when set to a
	// 32-byte key. Nil disables at-rest encryption.
	EncryptionKey []byte
	// WALArchiveDir enables continuous WAL segment archiving into the given
	// directory for Point-in-Time Recovery (PITR). Empty disables WAL archiving.
	WALArchiveDir string
	// WALArchiveKey optionally encrypts archived WAL segments with a 32-byte
	// AES-256-GCM key when WALArchiveDir is set.
	WALArchiveKey []byte
}

// CopyResult reports the completed command tag and row count of one CopyFrom or
// CopyTo operation.
type CopyResult struct {
	Tag          string
	RowsAffected uint64
}

// CopyOptions bounds one caller-supplied COPY stream. MaxRecordBytes caps a
// single encoded record; zero rejects the call rather than defaulting to an
// unbounded read. Report receives per-record diagnostics during COPY FROM.
type CopyOptions struct {
	MaxRecordBytes uint64
	Report         func(context.Context, CopyDiagnostic)
}

// CopyDiagnostic identifies one rejected record in a COPY FROM stream.
type CopyDiagnostic struct {
	Row    uint64
	Column string
	Err    error
}
