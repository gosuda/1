package supersql

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	controlstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	exdistinctset "github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	exrecursive "github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	exrowstore "github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsetop "github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	exwindow "github.com/gosuda/supersql-alpha/internal/query/exec/window"
	qexplain "github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
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

// CompactionConfig carries the NeoSepia backend resource envelope, including
// compaction run and preparation bounds. The zero value selects the backend
// defaults.
type CompactionConfig = neosepia.Limits

// OpenOptions configures Open. Clock is required and borrowed: the host owns
// provider startup and shutdown and must close the database before closing the
// provider, because a live database holds fences certified by that provider.
type OpenOptions struct {
	// Directory is the database directory. Open creates it when absent.
	Directory string
	// Compaction replaces the NeoSepia default limits when nonzero.
	Compaction CompactionConfig
	// Clock is the certified time source borrowed by the database.
	Clock clock.Clock
	// Maintenance bounds the background reclamation loop Open starts. The zero
	// value selects the defaults; Maintenance.Disabled stops the loop.
	Maintenance MaintenanceConfig
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
}

// Column describes one result column of an executed statement.
type Column struct {
	Name     string
	Type     model.Type
	Layout   model.TypeLayout
	Nullable bool
}

// Outcome reports one executed statement. Rows carries at most the requested
// row limit. It is the provisional in-tree execution surface: the public driver
// surface (typed parameters and streaming rows) lands with OpenConnector.
type Outcome struct {
	Tag          string
	RowsAffected uint64
	Committed    bool
	Columns      []Column
	Rows         [][]model.Value

	// builtins are the resolve options the producing statement planned with;
	// value rendering resolves declared-typed columns through them.
	builtins builtin.ResolveOptions
}

// Catalog is one opened native database. It owns the backend, workers,
// transaction manager and session services, and borrows the host clock.
type Catalog struct {
	directory   string
	clock       clock.Clock
	keys        *builtin.KeyEncoder
	authority   *txn.FenceAuthority
	backend     *neosepia.Backend
	manager     *txn.Manager
	catalog     *catalog.Store
	statistics  *controlstats.Store
	sequences   *schema.SequenceStore
	identities  *schema.JobStore
	indexJobs   *schema.JobStore
	service     *schema.SequenceService
	facade      *session.SequenceSession
	settings    *session.Settings
	txns        *session.Transactions
	embedded    *sessionScope
	policies    *schema.IndexPolicyProvider
	lifecycle   *schema.IndexLifecycle
	protections *schema.JobProtections
	retention   *schema.RetentionCoordinator
	scheduler   *schema.IndexJobScheduler
	advisory    *locks.AdvisoryLockManager
	native      *session.NativeCatalog
	// declaredTypes shares declared-type metadata between sessions that read the
	// same committed catalog content, which the policy provider's key encoder
	// does on every write statement.
	declaredTypes *session.DeclaredTypeCache
	source        *catalogrel.Source
	pool          *runtime.Pool
	budget        *runtime.Budget
	spill         string
	limits        model.DescriptorLimits
	readOnly      bool
	rls           *rlsManager
	sessionSeq    atomic.Uint64

	parallelism            int
	memoryBudgetBytes      uint64
	transactionMemoryBytes uint64
	transactionSpillBytes  uint64
	maxTransactions        int
	maxOperations          int

	workerCtx    context.Context
	workerStop   context.CancelFunc
	schedulerErr chan error

	maintenance        MaintenanceConfig
	maintenanceErr     chan error
	maintenanceStarted bool
	maintenanceLast    atomic.Pointer[error]

	mu       sync.Mutex
	closed   bool
	closeErr error
}

// Open composes the native session stack over Directory and returns the owning
// database: backend, transaction manager, control-plane stores and session
// services. A fresh directory is bootstrapped in one atomic catalog statement; a
// bootstrapped database is reopened from its durable manifest. The host-supplied
// clock is borrowed, never replaced. Open never reads wall time, keeps no
// package-level registry, and never constructs a clock of its own.
func Open(ctx context.Context, options OpenOptions) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Directory == "" {
		return nil, ErrDirectoryRequired
	}
	if clockUnavailable(options.Clock) {
		return nil, ErrClockRequired
	}
	maintenance, err := normalizedMaintenance(options.Maintenance)
	if err != nil {
		return nil, err
	}
	secret, fresh, err := readFenceSecret(options.Directory)
	if err != nil {
		return nil, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, err
	}
	keys, err := builtin.NewKeyEncoder(defaultCollation)
	if err != nil {
		return nil, err
	}
	storageLimits := neosepia.DefaultLimits()
	if options.Compaction != (CompactionConfig{}) {
		storageLimits = neosepia.Limits(options.Compaction)
	}
	backend, err := neosepia.Open(ctx, neosepia.Config{
		Directory: options.Directory, Scale: options.Clock.Scale(), KeyEncoder: keys,
		FenceVerifier: authority, Limits: storageLimits,
		ExtraCheckpointFiles: []string{nativeFenceSecretFile},
	})
	if err != nil {
		return nil, openFormatError(options.Directory, err)
	}
	if fresh {
		if err := writeFenceSecret(options.Directory, secret); err != nil {
			return nil, errors.Join(err, backend.Close())
		}
	}
	database := &Catalog{
		directory: options.Directory, clock: options.Clock, keys: keys, authority: authority, backend: backend,
		limits:                 model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64},
		spill:                  filepath.Join(options.Directory, nativeSpoolDirectory),
		readOnly:               options.HistoricalReadOnly,
		maintenance:            maintenance,
		parallelism:            options.Parallelism,
		memoryBudgetBytes:      options.MemoryBudgetBytes,
		transactionMemoryBytes: options.TransactionMemoryBytes,
		transactionSpillBytes:  options.TransactionSpillBytes,
		maxTransactions:        options.MaxTransactions,
		maxOperations:          options.MaxOperations,
	}
	database.workerCtx, database.workerStop = context.WithCancel(context.Background())
	database.schedulerErr = make(chan error, 1)
	database.maintenanceErr = make(chan error, 1)
	if err := database.compose(ctx); err != nil {
		return nil, errors.Join(err, database.closeAll())
	}
	return database, nil
}

// openFormatError maps the storage layer's format verdict. A directory without a
// native database.state cannot hold a native database, so an incompatible layout
// there is a legacy database rather than a damaged native one.
func openFormatError(directory string, err error) error {
	if !errors.Is(err, neosepia.ErrIncompatibleFormat) {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(directory, "database.state")); !errors.Is(statErr, os.ErrNotExist) {
		return err
	}
	return &UpgradeRequired{Directory: directory, Cause: err}
}

// compose builds every collaborator in dependency order. On failure the caller
// releases whatever was already constructed.
func (database *Catalog) compose(ctx context.Context) error {
	if err := os.MkdirAll(database.spill, 0o700); err != nil {
		return err
	}
	rls, err := newRLSManager(database.directory, database.spill)
	if err != nil {
		return err
	}
	database.rls = rls
	state, err := database.backend.State(ctx)
	if err != nil {
		return err
	}
	maxOps := database.maxOperations
	if maxOps <= 0 {
		maxOps = nativeMaxOperations
	}
	maxTxns := database.maxTransactions
	if maxTxns <= 0 {
		maxTxns = nativeMaxTransactions
	}
	database.manager, err = txn.NewManager(ctx, txn.ManagerConfig{
		Backend: database.backend, Clock: database.clock, Authority: database.authority,
		Recovered: txn.RecoveredState{
			LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS,
			HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID,
		},
		MaxOperations: maxOps, MaxTransactions: maxTxns,
		HistoricalReadOnly: database.readOnly,
	})
	if err != nil {
		return err
	}
	if database.catalog, err = catalog.New(database.keys, catalog.Limits{
		Descriptor: database.limits, MaxEncodedBytes: 2 << 20, MaxKeyBytes: 4096, ScanBatchRows: 16,
	}); err != nil {
		return err
	}
	if database.statistics, err = controlstats.New(ctx,
		storage.ObjectRef{ObjectID: nativeStatisticsObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: controlstats.EncodingVersion},
		database.keys,
		controlstats.Limits{MaxBytes: 1 << 20, MaxColumns: 32, MaxValues: 1024, MaxDepth: 32},
		4096,
	); err != nil {
		return err
	}
	if database.sequences, err = schema.NewSequenceStore(ctx, schema.SequenceStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeSequenceObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 4, MaxCache: 8,
	}); err != nil {
		return err
	}
	if database.identities, err = schema.NewJobStore(ctx, schema.JobStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeIdentityObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 64 << 10, KeyBytes: 4096, MaxJobs: 64, BatchRows: 8,
	}); err != nil {
		return err
	}
	if database.indexJobs, err = schema.NewJobStore(ctx, schema.JobStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeIndexObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 1 << 16, KeyBytes: 4096, MaxJobs: 16, BatchRows: 16,
	}); err != nil {
		return err
	}
	if database.settings, err = session.NewSettings(ctx, database.settingsOptions()); err != nil {
		return err
	}
	if err := database.prepareWorkers(); err != nil {
		return err
	}
	options := database.transactionOptions()
	builtins := database.settings.BuiltinOptions(defaultCollation, defaultCollation)
	database.declaredTypes = session.NewDeclaredTypeCache()
	keyEncoders, err := session.NewDeclaredTypeKeyEncoderFactory(database.keys, *database.typeRuntimeOptions(builtins), database.declaredTypes)
	if err != nil {
		return err
	}
	if database.policies, err = schema.NewIndexPolicyProvider(ctx, schema.IndexPolicyOptions{
		Catalog: database.catalog, Views: database.manager, Jobs: database.indexJobs,
		Encoders: keyEncoders,
		Expressions: session.IndexExpressionOptions{
			Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
		}.ForSnapshot,
		Columns: session.ColumnExpressionOptions{
			Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			Builtin: builtins,
		}.ForSnapshot,
		Generations: session.GenerationExpressionOptions{
			Columns: session.ColumnExpressionOptions{
				Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
				Builtin: builtins,
			},
			Indexes: session.IndexExpressionOptions{
				Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			},
		}.ForSnapshot,
		Limits: schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16},
	}); err != nil {
		return err
	}
	options = database.resolvedTransactionOptions()
	// A committed transaction always advances the commit timestamp, so a zero
	// high-water mark means no bootstrap has ever published on this database.
	if err := database.bootstrapStores(ctx, options, state.CommitTS == 0); err != nil {
		return err
	}
	if database.service, err = schema.NewSequenceService(ctx, schema.SequenceServiceOptions{
		Manager: database.manager, Store: database.sequences, Transactions: options, MaxOperations: 8,
	}); err != nil {
		return err
	}
	if database.facade, err = session.NewSequenceSession(session.SequenceSessionOptions{Allocations: database.service, MaxSequences: 8}); err != nil {
		return err
	}
	if database.lifecycle, err = schema.NewIndexLifecycle(schema.IndexLifecycleOptions{
		Catalog: database.catalog, Jobs: database.indexJobs, Limits: database.limits,
	}); err != nil {
		return err
	}
	if database.protections, err = schema.NewJobProtections(schema.JobProtectionOptions{
		Jobs: database.indexJobs, Snapshots: database.manager, MaxJobs: 16, MemoryBytes: 8 << 20,
	}); err != nil {
		return err
	}
	// Job protections restore before the scheduler and before any GC decision:
	// an unrestored protection set must never let history beneath a running
	// job's pin be collected.
	var journal storage.SnapshotReader
	if database.readOnly && state.ClosedTS == 0 {
		// Nothing was ever committed, so no job protections exist to restore.
		journal = nil
	} else if database.readOnly {
		journal, err = database.manager.HistoricalSnapshot(ctx, state.ClosedTS)
	} else {
		journal, err = database.manager.CurrentReadView(ctx)
	}
	if err != nil {
		return err
	}
	if journal != nil {
		err = errors.Join(database.protections.Restore(ctx, journal), journal.Close())
		if err != nil {
			return err
		}
	}
	// The coordinator borrows protections and the job store and starts no work
	// of its own; the maintenance loop below is its only caller.
	if database.retention, err = schema.NewRetentionCoordinator(schema.RetentionOptions{
		Clock: database.clock, Backend: database.backend, Protections: database.protections,
		Jobs: database.indexJobs, Manager: database.manager, Transaction: database.transactionOptions(),
		MaxRetiredTargets: database.maintenance.MaxRetiredTargets,
	}); err != nil {
		return err
	}
	if database.scheduler, err = schema.NewIndexJobScheduler(ctx, schema.IndexJobSchedulerOptions{
		Runner: schema.IndexJobRunnerOptions{
			Manager: database.manager, Policies: database.policies, Jobs: database.indexJobs,
			Lifecycle: database.lifecycle, Protections: database.protections, Transaction: options,
			Scan:       schema.BackfillScanLimits{BatchRows: 64, MaxKeyBytes: 4096, WorkBytes: 8 << 20},
			Backfill:   schema.BackfillLimits{MaxRows: 1 << 20, MaxKeyBytes: 4 << 20, WorkBytes: 32 << 20},
			Validation: schema.IndexValidationLimits{BatchRows: 64, MaxRows: 1 << 30, MaxBytes: 32 << 20},
			MaxRetries: 8, RetryDelay: 10 * time.Millisecond, MaxRetryDelay: time.Second,
		},
		MaxRunning: 4, MemoryBytes: 32 << 20, PollInterval: 10 * time.Millisecond,
	}); err != nil {
		return err
	}
	if database.advisory, err = locks.NewAdvisoryLockManagerWithLimits(locks.DefaultAdvisoryLimits()); err != nil {
		return err
	}
	if database.txns, err = session.NewTransactions(ctx, database.sessionConfig(database.settings, 1)); err != nil {
		return err
	}
	database.embedded = &sessionScope{
		database:      database,
		settings:      database.settings,
		txns:          database.txns,
		id:            1,
		preparedStmts: make(map[string]*zpgparse.PrepareStmt),
	}
	if !database.readOnly {
		go func() {
			// The scheduler's execution error is collected by Close before the
			// backend is released; a background scheduler without a collector
			// would drop job failures silently.
			database.schedulerErr <- database.scheduler.Run(database.workerCtx)
		}()
	}
	if err := database.openCatalog(ctx); err != nil {
		return err
	}
	if err := database.loadRLSMetadata(ctx); err != nil {
		return err
	}
	if database.readOnly {
		return nil
	}
	// The retention coordinator refuses to run without the durable policy, so the
	// policy is installed before the loop starts and one code path serves both a
	// fresh directory and a pre-existing one that predates the policy.
	if err := database.ensureHistoryPolicy(ctx); err != nil {
		return err
	}
	if !database.maintenance.Disabled {
		database.maintenanceStarted = true
		go func() {
			// Close joins this loop through maintenanceErr before it releases
			// protections, the manager and the backend.
			database.maintenanceErr <- database.maintenanceLoop(database.workerCtx)
		}()
	}
	return nil
}

// ensureHistoryPolicy installs the default durable retention policy when the
// database has none. It commits once, so a database without a policy gains one
// extra commit on its first Open under this version.
func (database *Catalog) ensureHistoryPolicy(ctx context.Context) error {
	transaction, err := database.manager.Begin(ctx, database.resolvedTransactionOptions())
	if err != nil {
		return err
	}
	err = transaction.Statement(ctx, func(statement context.Context) error {
		if _, err := database.indexJobs.HistoryPolicy(statement, transaction); err == nil {
			return nil
		} else if !errors.Is(err, storage.ErrRowNotFound) {
			return err
		}
		return database.indexJobs.CreateHistoryPolicy(statement, transaction, 0)
	})
	if err == nil {
		_, err = transaction.Commit(ctx)
	}
	return errors.Join(err, transaction.Close(context.Background()))
}

// openCatalog loads the durable native catalog, bootstrapping a fresh database,
// and guarantees the default `public` schema exists for the default search path.
func (database *Catalog) openCatalog(ctx context.Context) error {
	binding := bind.Options{
		SearchPath: []string{"pg_catalog"}, Collation: defaultCollation, DefaultCollation: defaultCollation,
		Limits: database.irLimits(), Builtin: database.settings.BuiltinOptions(defaultCollation, defaultCollation),
	}
	config := session.NativeCatalogConfig{
		Catalog: database.catalog, Identities: database.identities, Binding: binding,
		Descriptors: database.limits, ManifestBytes: 8 << 20,
	}
	bootstrapped, err := database.catalogBootstrapped(ctx, config.ManifestBytes)
	if err != nil {
		return err
	}
	var native *session.NativeCatalog
	if bootstrapped {
		// Loading validates the durable record inside its own transaction block,
		// so a partially published catalog can never be completed silently. A
		// historical-only open pins the recovered closed timestamp, which needs
		// no fresh certification.
		start := session.TransactionStart{}
		if database.readOnly {
			state, stateErr := database.backend.State(ctx)
			if stateErr != nil {
				return stateErr
			}
			start.Historical = state.ClosedTS
		}
		if err := database.txns.Begin(ctx, start); err != nil {
			return err
		}
		if native, err = session.LoadNativeCatalog(ctx, database.txns, config); err != nil {
			return errors.Join(err, database.txns.Rollback(ctx, false))
		}
		if _, err := database.txns.Commit(ctx, false); err != nil {
			return err
		}
	} else if database.readOnly {
		return &model.Error{Code: "55000", Message: "historical read-only open requires a bootstrapped database"}
	} else if native, err = session.BootstrapNativeCatalog(ctx, database.txns, config); err != nil {
		return err
	}
	database.native = native
	principal := model.Principal{}
	if database.source, err = catalogrel.NewSource(catalogrel.SourceOptions{
		Snapshot:   catalogrel.CatalogStoreFactory{Store: database.catalog},
		Statistics: catalogrel.StatisticsStoreFactory{Store: database.statistics},
		Sequences:  database.sequences,
		Features:   newStackSQLMetadata{}, Standards: newStackSQLMetadata{},
		Database: catalogrel.Database{
			OID: 16384, Name: "supersql", Owner: principal.OID(), Encoding: 6,
			Collation: "C", CharacterType: "C", LocaleProvider: 'c', CollationOID: defaultCollation,
			AllowConnections: true, ConnectionLimit: -1,
			RoleOID: principal.OID(), RoleName: principal.Name(), RoleInherit: true,
			RoleCanLogin: true, RoleConnectionLimit: -1,
		},
		Registry: native.Registry,
		MaxRows:  1024, MaxBytes: 4 << 20, MaxDescriptors: 1024,
		MaxDescriptorBytes: database.limits.MaxBytes, MaxDescriptorItems: database.limits.MaxItems,
		MaxDescriptorDepth: database.limits.MaxDepth,
	}); err != nil {
		return err
	}
	if database.readOnly {
		return nil
	}
	_, err = database.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS public", 0)
	return err
}

// catalogBootstrapped reports whether the durable native catalog manifest has
// already been published. Ordinary opens read through a certified current view;
// a historical-only open reads the recovered durable closed timestamp instead,
// so an absent manifest is distinguishable from a corrupt one without fresh
// certification.
func (database *Catalog) catalogBootstrapped(ctx context.Context, manifestBytes uint64) (bool, error) {
	if database.readOnly {
		state, err := database.backend.State(ctx)
		if err != nil {
			return false, err
		}
		if state.ClosedTS == 0 {
			return false, nil
		}
		reader, err := database.manager.HistoricalSnapshot(ctx, state.ClosedTS)
		if err != nil {
			return false, err
		}
		_, err = database.identities.CatalogManifest(ctx, reader, manifestBytes)
		closeErr := reader.Close()
		if err == nil {
			return true, closeErr
		}
		if errors.Is(err, storage.ErrRowNotFound) {
			return false, closeErr
		}
		return false, errors.Join(err, closeErr)
	}
	view, err := database.manager.CurrentReadView(ctx)
	if err != nil {
		return false, err
	}
	_, err = database.identities.CatalogManifest(ctx, view, manifestBytes)
	closeErr := view.Close()
	if err == nil {
		return true, closeErr
	}
	if errors.Is(err, storage.ErrRowNotFound) {
		return false, closeErr
	}
	return false, errors.Join(err, closeErr)
}

// bootstrapStores reserves the durable allocator and identity records once. The
// caller reuses the same transaction options for every later statement.
func (database *Catalog) bootstrapStores(ctx context.Context, options txn.TransactionOptions, fresh bool) error {
	if !fresh {
		return nil
	}
	bootstrap, err := database.manager.Begin(ctx, options)
	if err != nil {
		return err
	}
	err = bootstrap.Statement(ctx, func(statement context.Context) error {
		if err := database.sequences.InitializeAllocator(statement, bootstrap); err != nil {
			return err
		}
		return database.identities.InitializeIdentities(statement, bootstrap, schema.IdentityHighWaterMarks{
			ObjectID: 100, OID: 20000, JobID: 100,
		})
	})
	if err == nil {
		_, err = bootstrap.Commit(ctx)
	}
	return errors.Join(err, bootstrap.Close(context.Background()))
}

// prepareWorkers creates the shared worker pool and memory budget. Each
// statement still owns its own query account.
func (database *Catalog) prepareWorkers() error {
	parallelism := database.parallelism
	if parallelism <= 0 {
		parallelism = max(1, goruntime.GOMAXPROCS(0))
	}
	queueCapacity := max(2, parallelism*2)
	maxGroups := max(16, parallelism*4)
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: parallelism, QueueCapacity: queueCapacity, MaxGroups: maxGroups})
	if err != nil {
		return err
	}
	database.pool = pool
	budgetBytes := database.memoryBudgetBytes
	if budgetBytes == 0 {
		budgetBytes = 256 << 20
	}
	if database.budget, err = runtime.NewBudget(int64(budgetBytes), 4096); err != nil {
		return err
	}
	return nil
}

// transactionOptions is the single session transaction configuration. Resolver
// is filled in by compose once the index policy provider exists.
func (database *Catalog) transactionOptions() txn.TransactionOptions {
	memBytes := database.transactionMemoryBytes
	if memBytes == 0 {
		memBytes = 8 << 20
	}
	spillBytes := database.transactionSpillBytes
	if spillBytes == 0 {
		spillBytes = 32 << 20
	}
	return txn.TransactionOptions{
		Mode: txn.ReadWrite, KeyEncoder: database.keys,
		MemoryBytes: memBytes, SpillBytes: spillBytes, SpillDirectory: database.spill, MaxViews: 16,
	}
}

// resolvedTransactionOptions adds the sequence/identity/policy resolver chain.
// Call only after sequences, identities and policies exist; sessionConfig and
// the compose-time options both need the resolved form.
func (database *Catalog) resolvedTransactionOptions() txn.TransactionOptions {
	options := database.transactionOptions()
	options.Resolver = database.statistics.WithBaseResolver(database.sequences.WithBaseResolver(database.identities.WithBaseResolver(database.policies)))
	return options
}

// sessionScope is one SQL session: its own settings and transaction
// orchestration. The embedded Exec path uses scope(), while connector
// connections each own a distinct scope so per-session state (settings,
// prepared statements, cursors, advisory locks, temporary relations) never
// crosses connections.
type sessionScope struct {
	database      *Catalog
	settings      *session.Settings
	txns          *session.Transactions
	id            uint64
	rlsTouched    atomic.Bool
	preparedMu    sync.Mutex
	preparedStmts map[string]*zpgparse.PrepareStmt
}

func (scope *sessionScope) savePreparedStmt(prep *zpgparse.PrepareStmt) {
	if scope == nil || prep == nil || prep.Name == "" {
		return
	}
	scope.preparedMu.Lock()
	if scope.preparedStmts == nil {
		scope.preparedStmts = make(map[string]*zpgparse.PrepareStmt)
	}
	scope.preparedStmts[strings.ToLower(prep.Name)] = proto.Clone(prep).(*zpgparse.PrepareStmt)
	scope.preparedMu.Unlock()
}

func (scope *sessionScope) getPreparedStmt(name string) *zpgparse.PrepareStmt {
	if scope == nil || name == "" {
		return nil
	}
	scope.preparedMu.Lock()
	defer scope.preparedMu.Unlock()
	if scope.preparedStmts == nil {
		return nil
	}
	prep := scope.preparedStmts[strings.ToLower(name)]
	if prep == nil {
		return nil
	}
	return proto.Clone(prep).(*zpgparse.PrepareStmt)
}

func (scope *sessionScope) deletePreparedStmt(name string, all bool) {
	if scope == nil {
		return
	}
	scope.preparedMu.Lock()
	defer scope.preparedMu.Unlock()
	if all || name == "" {
		clear(scope.preparedStmts)
		return
	}
	delete(scope.preparedStmts, strings.ToLower(name))
}

func (database *Catalog) settingsOptions() session.SettingsOptions {
	return session.SettingsOptions{
		Resolver: newStackSettings{},
		Limits: session.SettingLimits{
			MaxValueBytes: 4096, MaxSavepoints: 8,
			WorkMemoryBytes: 128 << 20, MaintenanceMemoryBytes: 128 << 20, TemporaryFileBytes: 1 << 30,
		},
	}
}

// embeddedScope is the session that owns Exec and internal bookkeeping
// transactions. Its advisory session ID is reserved as 1.
func (database *Catalog) embeddedScope() *sessionScope {
	if database.embedded != nil {
		return database.embedded
	}
	return &sessionScope{database: database, settings: database.settings, txns: database.txns, id: 1}
}

// sessionConfig is one session's transaction orchestration configuration:
// per-session settings and advisory identity over the shared manager.
func (database *Catalog) sessionConfig(settings *session.Settings, id uint64) session.TransactionConfig {
	return session.TransactionConfig{
		Manager: database.manager, Settings: settings, Options: database.resolvedTransactionOptions(), TemporalClock: database.clock,
		TypeRuntime: database.typeRuntimeOptions(settings.BuiltinOptions(defaultCollation, defaultCollation)),
		SequenceDDL: &session.SequenceDDLConfig{
			Catalog: database.catalog, Identities: database.identities, Service: database.service, Session: database.facade,
			MaxChanges: 16, MemoryBytes: 64 << 10, CleanupTimeout: nativeCleanupTimeout,
		},
		NamedObjects: session.SessionNamedObjectOptions{
			NewQuery: database.newStatementQuery, MaxPrepared: 64, MaxCursors: 16,
			MetadataBytes: 8 << 20, PlanningCostUnitsPerSecond: 1e6,
		},
		Advisory: session.AdvisoryLockConfig{Manager: database.advisory, SessionID: id},
	}
}

// newSession builds one isolated session for a driver connection. Close
// releases every session-scoped object.
func (database *Catalog) newSession(ctx context.Context) (_ *sessionScope, err error) {
	settings, err := session.NewSettings(ctx, database.settingsOptions())
	if err != nil {
		return nil, err
	}
	id := database.sessionSeq.Add(1) + 1
	txns, err := session.NewTransactions(ctx, database.sessionConfig(settings, id))
	if err != nil {
		return nil, err
	}
	return &sessionScope{
		database:      database,
		settings:      settings,
		txns:          txns,
		id:            id,
		preparedStmts: make(map[string]*zpgparse.PrepareStmt),
	}, nil
}

func (scope *sessionScope) close() error {
	if scope == nil || scope.txns == nil {
		return nil
	}
	return scope.txns.Close()
}

func (database *Catalog) irLimits() ir.Limits {
	return ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: 1 << 20}
}

// Exec runs every statement in sql through the session stack and reports the
// last statement's outcome. Result rows are materialized up to maxRows; maxRows
// <= 0 accepts every produced row. Statements bind and plan against a certified
// current snapshot and then run under the dependencies their own plan declares.
// AS OF SYSTEM TIME sidecars route the statement to the fixed-snapshot
// historical path; transaction-level temporal clauses route to the session's
// transaction commands.
func (database *Catalog) Exec(ctx context.Context, sql string, maxRows int) (Outcome, error) {
	if database == nil {
		return Outcome{}, ErrDatabaseClosed
	}
	return database.execIn(ctx, database.embeddedScope(), sql, maxRows)
}

// execIn runs every statement in sql through scope and reports the last
// statement's outcome.
func (database *Catalog) execIn(ctx context.Context, scope *sessionScope, sql string, maxRows int) (Outcome, error) {
	if err := database.checkOpen(); err != nil {
		return Outcome{}, err
	}
	parsed, err := scope.txns.Parse(ctx, sql, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return Outcome{}, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return Outcome{}, ErrMissingStatement
	}
	var outcome Outcome
	for index := range parsed.ParseResult.Stmts {
		if outcome, err = database.execStatement(ctx, scope, parsed, index, nil, maxRows, nil, nil); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
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

// CopyFrom executes one COPY ... FROM STDIN statement, reading records from
// source until EOF. The caller keeps ownership of source; the engine never
// closes it. Server-side COPY FROM 'file' and FROM PROGRAM are excluded
// features and fail with SQLSTATE 0A000.
func (database *Catalog) CopyFrom(ctx context.Context, statement string, source io.Reader, options CopyOptions) (Outcome, error) {
	streams := &qcopy.Streams{Reader: source, MaxRecordBytes: options.MaxRecordBytes, Report: copyReport(options.Report)}
	return database.copyStream(ctx, statement, streams)
}

// CopyTo executes one COPY ... TO STDOUT statement, writing records to sink.
// The caller keeps ownership of sink; the engine never closes it. Server-side
// COPY TO 'file' and TO PROGRAM are excluded features and fail with SQLSTATE
// 0A000.
func (database *Catalog) CopyTo(ctx context.Context, statement string, sink io.Writer, options CopyOptions) (Outcome, error) {
	streams := &qcopy.Streams{Writer: sink, MaxRecordBytes: options.MaxRecordBytes, Report: copyReport(options.Report)}
	return database.copyStream(ctx, statement, streams)
}

func copyReport(report func(context.Context, CopyDiagnostic)) func(context.Context, qcopy.Diagnostic) {
	if report == nil {
		return nil
	}
	return func(ctx context.Context, diagnostic qcopy.Diagnostic) {
		report(ctx, CopyDiagnostic{Row: diagnostic.Row, Column: diagnostic.Column, Err: diagnostic.Err})
	}
}

// copyStream runs a single-statement COPY through the embedded session with
// the caller's stream attached.
func (database *Catalog) copyStream(ctx context.Context, statement string, streams *qcopy.Streams) (Outcome, error) {
	if database == nil {
		return Outcome{}, ErrDatabaseClosed
	}
	return database.copyIn(ctx, database.embeddedScope(), statement, streams)
}

// copyIn runs a single-statement COPY through scope. The statement's direction
// decides which stream field validation accepts; a non-COPY or wrongly directed
// statement fails with a typed error instead of reaching execution.
func (database *Catalog) copyIn(ctx context.Context, scope *sessionScope, statement string, streams *qcopy.Streams) (Outcome, error) {
	if err := database.checkOpen(); err != nil {
		return Outcome{}, err
	}
	parsed, err := scope.txns.Parse(ctx, statement, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return Outcome{}, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return Outcome{}, &model.Error{Code: "22023", Message: "COPY accepts exactly one statement"}
	}
	return database.execStatement(ctx, scope, parsed, 0, nil, 0, nil, streams)
}

func (database *Catalog) checkOpen() error {
	database.mu.Lock()
	defer database.mu.Unlock()
	if database.closed {
		return ErrDatabaseClosed
	}
	return nil
}

// rowSource is the pull-side row contract both session portals and historical
// portals satisfy; the driver streams batches through it.
type rowSource interface {
	Next(context.Context, *columnar.Batch) error
}

// rowStream receives one statement's result columns, the resolve options that
// statement planned with, and its row source. The options carry the
// declared-type resolver pinned to the statement's snapshot, which result
// rendering needs for enum and domain columns after execution releases its pin.
type rowStream func(ctx context.Context, columns []bind.ResultColumn, builtins builtin.ResolveOptions, source rowSource) error

// execStatement resolves one parsed statement's extension sidecars and then
// routes it: transaction-level temporal clauses to the transaction commands,
// relation-level temporal clauses to the fixed-snapshot historical path, and
// everything else to RunStatement with the full command configuration. A nil
// stream materializes rows into the Outcome; a non-nil stream receives the
// result columns and callback-scoped row source instead.
func (database *Catalog) execStatement(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (_ Outcome, err error) {
	statement := parsed.ParseResult.Stmts[index]
	builtins := scope.builtins()
	systemTime := scope.systemTimeOptions(builtins)
	selection, err := session.ResolveSystemTime(ctx, parsed, index, nil, systemTime)
	if err != nil {
		return Outcome{}, err
	}
	if selection.Present && selection.Transaction {
		if len(parameters) != 0 {
			return Outcome{}, &model.Error{Code: "42P02", Message: "transaction commands do not accept parameters"}
		}
		result, err := session.ExecuteSystemTimeTransaction(ctx, scope.txns, parsed, index, nil, systemTime)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Tag: result.Tag, Committed: result.Commit != nil}, nil
	}
	if selection.Present {
		return database.execHistorical(ctx, scope, parsed, index, builtins, parameters, maxRows, stream)
	}
	if database.readOnly {
		// A historical-only database serves retained fixed snapshots only;
		// implicit latest reads and every write are rejected before planning.
		return Outcome{}, &model.Error{Code: "0A000", Message: "historical read-only database requires AS OF SYSTEM TIME"}
	}
	if rlsOutcome, handled, rlsErr := database.tryExecRLSCommand(ctx, scope, statement, stream, streams); handled {
		return rlsOutcome, rlsErr
	}
	preDDLTables := database.capturePreDDLRLSTables(ctx, scope, statement)
	var rlsCheck *rlsCheckPlan
	statement, rlsCheck, err = database.rewriteStatementForRLS(ctx, scope, statement)
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := database.runStatementCore(ctx, scope, parsed, index, statement, rlsCheck, preDDLTables, parameters, maxRows, stream, streams)
	if statement != nil && statement.Stmt != nil && statement.Stmt.GetTransactionStmt() != nil && !scope.txns.Active() && scope.rlsTouched.Swap(false) {
		database.rls.pendingTx.Store(false)
		_ = database.syncCommittedRLSState(ctx)
	}
	return outcome, err
}

func (database *Catalog) runRawStatement(ctx context.Context, scope *sessionScope, statement *zpgparse.RawStmt, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (Outcome, error) {
	return database.runStatementCore(ctx, scope, nil, 0, statement, nil, nil, parameters, maxRows, stream, streams)
}

func (database *Catalog) runRawStatementWithRLSCheck(ctx context.Context, scope *sessionScope, statement *zpgparse.RawStmt, rlsCheck *rlsCheckPlan, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (Outcome, error) {
	return database.runStatementCore(ctx, scope, nil, 0, statement, rlsCheck, nil, parameters, maxRows, stream, streams)
}

func (database *Catalog) runStatementCore(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, statement *zpgparse.RawStmt, rlsCheck *rlsCheckPlan, preDDLTables []string, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (_ Outcome, err error) {
	builtins := scope.builtins()
	query, err := runtime.NewQuery(ctx, database.pool, database.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes: 128 << 20, OutputBytes: 16 << 20, SpillBytes: 1 << 30,
		SpillMode: runtime.SpillEnabled, Directory: database.spill, MaxFiles: 8, MaxReaders: 8,
	})
	if err != nil {
		return Outcome{}, err
	}
	defer func() { err = errors.Join(err, query.Close()) }()
	environment := database.executionEnvironment(query, builtins)
	environment.Parameters = parameters
	execution := session.PreparedExecution{
		Catalog:       database.catalog,
		Environment:   environment,
		ObjectDDL:     ptrTo(scope.objectCommandOptions(builtins)),
		Statistics:    ptrTo(scope.statisticsCommandOptions(builtins)),
		TypeDDL:       ptrTo(scope.typeCommandOptions(builtins)),
		RoutineDDL:    ptrTo(scope.routineCommandOptions(builtins)),
		TextSearchDDL: ptrTo(scope.textSearchCommandOptions(builtins)),
		TypeRuntime:   scope.typeRuntimeOptions(builtins),
		Explain:       &session.ExplainExecutionOptions{Limits: qexplain.Limits{MaxOutputBytes: 8 << 20, MaxNodeBytes: 1 << 20, MaxValueDepth: 16}},
		Routines:      scope.routineExecutionOptions(builtins),
	}
	schemaExecution, release, err := database.schemaCommandExecution(ctx, scope, parsed, index, builtins)
	if err != nil {
		return Outcome{}, err
	}
	if release != nil {
		defer func() { err = errors.Join(err, release()) }()
	}
	execution.Schema = schemaExecution
	execution.Copy = streams
	outcome := Outcome{Rows: make([][]model.Value, 0)}
	var consume func(context.Context, *session.Prepared, *session.Rows) error
	if streams == nil {
		consume = func(statementCtx context.Context, prepared *session.Prepared, rows *session.Rows) error {
			var columns []bind.ResultColumn
			statementBuiltins := builtins
			if prepared != nil {
				columns = prepared.Columns()
				statementBuiltins = prepared.Builtin()
			}
			if rlsCheck != nil {
				return database.consumeWithRLSCheck(statementCtx, columns, statementBuiltins, rows, rlsCheck, stream, &outcome, maxRows)
			}
			if stream != nil {
				return stream(statementCtx, columns, statementBuiltins, rows)
			}
			outcome.builtins = statementBuiltins
			for _, column := range columns {
				outcome.Columns = append(outcome.Columns, Column{Name: column.Name, Type: column.Layout.Type, Layout: column.Layout, Nullable: column.Nullable})
			}
			return database.consumeRows(statementCtx, rows, &outcome, maxRows)
		}
	}
	result, err := scope.txns.RunStatement(ctx, statement, session.StatementExecution{
		Execution:  execution,
		Parameters: declaredParameterLayouts(parameters),
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
			return scope.prepareOptions(ctx, snapshot, reader, builtins)
		},
	}, consume)
	if err == nil && len(preDDLTables) > 0 {
		err = database.afterDDLStatementForRLS(ctx, scope, statement, preDDLTables)
	}
	outcome.Tag, outcome.RowsAffected, outcome.Committed = result.Tag, result.RowsAffected, result.Commit.Committed
	return outcome, err
}

// declaredParameterLayouts collects the parameter layouts callers assert so
// binding treats those positions as typed parameters instead of inferring their
// types from context. Undeclared positions stay zero and keep context
// inference. Parameter numbers beyond PostgreSQL's 65535 limit are left
// undeclared; the bound-layout reconcile rejects them.
func declaredParameterLayouts(parameters []expr.ParameterValue) []model.TypeLayout {
	const maxParameterNumber = 65535
	maximum := uint32(0)
	for _, parameter := range parameters {
		if parameter.Number == 0 || parameter.Number > maxParameterNumber || parameter.Layout.Type.ID == 0 {
			continue
		}
		maximum = max(maximum, parameter.Number)
	}
	if maximum == 0 {
		return nil
	}
	declared := make([]model.TypeLayout, maximum)
	for _, parameter := range parameters {
		if parameter.Number == 0 || parameter.Number > maxParameterNumber || parameter.Layout.Type.ID == 0 {
			continue
		}
		declared[parameter.Number-1] = parameter.Layout
	}
	return declared
}

// execHistorical runs one relation-level AS OF SYSTEM TIME statement over a
// retained fixed snapshot without a live transaction or fresh certification.
func (database *Catalog) execHistorical(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, builtins builtin.ResolveOptions, parameters []expr.ParameterValue, maxRows int, stream rowStream) (_ Outcome, err error) {
	if parsed != nil && parsed.ParseResult != nil && index >= 0 && index < len(parsed.ParseResult.Stmts) {
		rewritten, _, rlsErr := database.rewriteStatementForRLS(ctx, scope, parsed.ParseResult.Stmts[index])
		if rlsErr != nil {
			return Outcome{}, rlsErr
		}
		if rewritten != parsed.ParseResult.Stmts[index] {
			clonedStmts := make([]*zpgparse.RawStmt, len(parsed.ParseResult.Stmts))
			copy(clonedStmts, parsed.ParseResult.Stmts)
			clonedStmts[index] = rewritten
			clonedParsed := *parsed
			clonedParsed.ParseResult = &zpgparse.ParseResult{Version: parsed.ParseResult.Version, Stmts: clonedStmts}
			parsed = &clonedParsed
		}
	}
	historical, err := session.PrepareHistorical(ctx, parsed, index, parameters, session.HistoricalOptions{
		Snapshots:  database.manager,
		Catalog:    database.catalog,
		SystemTime: scope.systemTimeOptions(builtins),
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
			return scope.prepareOptions(ctx, snapshot, reader, builtins)
		},
		TypeRuntime: scope.typeRuntimeOptions(builtins),
	})
	if err != nil {
		return Outcome{}, err
	}
	defer func() { err = errors.Join(err, historical.Close()) }()
	query, err := runtime.NewQuery(ctx, database.pool, database.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes: 128 << 20, OutputBytes: 16 << 20, SpillBytes: 1 << 30,
		SpillMode: runtime.SpillEnabled, Directory: database.spill, MaxFiles: 8, MaxReaders: 8,
	})
	if err != nil {
		return Outcome{}, err
	}
	defer func() { err = errors.Join(err, query.Close()) }()
	portal, err := historical.Open(ctx, database.executionEnvironment(query, builtins))
	if err != nil {
		return Outcome{}, err
	}
	defer func() { err = errors.Join(err, portal.Close()) }()
	outcome := Outcome{Rows: make([][]model.Value, 0)}
	var columns []bind.ResultColumn
	statementBuiltins := builtins
	if prepared := historical.Prepared(); prepared != nil {
		columns = prepared.Columns()
		statementBuiltins = prepared.Builtin()
	}
	if stream != nil {
		return outcome, stream(ctx, columns, statementBuiltins, portal)
	}
	outcome.builtins = statementBuiltins
	for _, column := range columns {
		outcome.Columns = append(outcome.Columns, Column{Name: column.Name, Type: column.Layout.Type, Layout: column.Layout, Nullable: column.Nullable})
	}
	var batch columnar.Batch
	for {
		if maxRows > 0 && len(outcome.Rows) >= maxRows {
			break
		}
		nextErr := portal.Next(ctx, &batch)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return outcome, nextErr
		}
		for lane := range batch.Rows() {
			if maxRows > 0 && len(outcome.Rows) >= maxRows {
				break
			}
			row := make([]model.Value, len(batch.Columns))
			for columnIndex := range batch.Columns {
				value, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[columnIndex], batch.Lane(lane), 1<<20)
				if copyErr != nil {
					return outcome, copyErr
				}
				row[columnIndex] = value
			}
			outcome.Rows = append(outcome.Rows, row)
		}
	}
	outcome.Tag, outcome.RowsAffected, outcome.Committed = "SELECT", uint64(len(outcome.Rows)), true
	return outcome, nil
}

// ptrTo returns the address of a locally constructed options value; the session
// distinguishes "family not configured" by nil pointer, not by zero fields.
func ptrTo[T any](value T) *T { return &value }

func (database *Catalog) consumeRows(ctx context.Context, rows *session.Rows, outcome *Outcome, maxRows int) error {
	var batch columnar.Batch
	for {
		if maxRows > 0 && len(outcome.Rows) >= maxRows {
			return nil
		}
		err := rows.Next(ctx, &batch)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for lane := range batch.Rows() {
			if maxRows > 0 && len(outcome.Rows) >= maxRows {
				return nil
			}
			row := make([]model.Value, len(batch.Columns))
			for index := range batch.Columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(lane), 1<<20)
				if err != nil {
					return err
				}
				row[index] = value
			}
			outcome.Rows = append(outcome.Rows, row)
		}
	}
}

// prepareOptions supplies the immutable planning collaborators for one
// statement: the catalog access planner is bound to the snapshot and reader the
// caller already pinned.
func (scope *sessionScope) prepareOptions(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, builtins builtin.ResolveOptions) (session.PrepareOptions, error) {
	database := scope.database
	limits := database.irLimits()
	options := session.PrepareOptions{
		Bind:      scope.bindOptions(builtins),
		Normalize: normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtins},
		Optimize: opt.Options{
			IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20},
			MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20,
		},
		Fragment: fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20},
	}
	// The statement's declared-type runtime is pinned before this factory runs,
	// so the key encoder shares it. A caller that supplies no runtime, such as a
	// direct Open path, builds one here: through the session when there is one,
	// so the definition scan and the routine discovery are shared with the
	// session's other statements reading the same catalog content.
	types := builtins.Types
	if types == nil {
		build := session.NewDeclaredTypeRuntime
		if scope.txns != nil {
			build = scope.txns.DeclaredTypeRuntime
		}
		var err error
		types, err = build(ctx, snapshot, *database.typeRuntimeOptions(builtins))
		if err != nil {
			return options, err
		}
	}
	resolvers := builtins
	resolvers.Types = types
	keys := database.keys.WithResolveOptions(resolvers)
	access, err := session.NewCatalogAccess(snapshot, reader, keys, session.CatalogAccessOptions{
		MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1,
		IndexLimits:      schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
		IndexExpressions: session.IndexExpressionOptions{Binding: limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20},
		Builtin:          builtins,
	})
	if err != nil {
		// A missing planner is reported by the optimizer as an unsupported scan,
		// so the statement still fails with a SQL diagnostic rather than silently
		// planning without access paths.
		return options, nil
	}
	options.Optimize.Access = access
	return options, nil
}

// objectCommandOptions wires CREATE/ALTER/DROP/TRUNCATE/INDEX declarations,
// including their column and index expression compilers.
func (scope *sessionScope) objectCommandOptions(builtins builtin.ResolveOptions) session.ObjectCommandOptions {
	database := scope.database
	limits := database.irLimits()
	metadata := uint64(4 << 20)
	// Admission reserves two MetadataBytes copies plus one Evaluation.MaxBytes
	// per declared expression against PolicyBytes, so Evaluation.MaxBytes bounds
	// one compiled column/index expression, not a whole statement program.
	evaluation := expr.DefaultLimits()
	evaluation.MaxBytes = 128 << 10
	return session.ObjectCommandOptions{
		Binding: bind.CreateDeclarationOptions{
			Bind:        scope.bindOptions(builtins),
			Descriptors: database.limits,
			Physical:    model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		},
		Columns: session.ColumnExpressionOptions{
			Binding: limits, Evaluation: evaluation, InputBytes: 1 << 20, ResultBytes: 1 << 20, Builtin: builtins,
		},
		Indexes: session.IndexExpressionOptions{
			Binding: limits, Evaluation: evaluation, InputBytes: 1 << 20, ResultBytes: 1 << 20,
		},
		Limits:         schema.IndexLimits{MetadataBytes: metadata, EntryBytes: 1 << 20, MaxColumns: 64},
		PolicyBytes:    8 * metadata,
		MaxDescriptors: 64,
		Constraints:    database.policies,
		RewriteQuery: func(ctx context.Context, raw *zpgparse.RawStmt) (*zpgparse.RawStmt, error) {
			rewritten, _, err := database.rewriteStatementForRLS(ctx, scope, raw)
			return rewritten, err
		},
	}
}

// builtins returns the resolve options for one statement: the native catalog
// registry serves information_schema and pg_catalog relations once the durable
// catalog has loaded.
func (scope *sessionScope) builtins() builtin.ResolveOptions {
	options := scope.settings.BuiltinOptions(defaultCollation, defaultCollation)
	if scope.database.native != nil {
		options.InformationSchemaFunctions = scope.database.native.Registry
		options.InformationSchemaTypes = scope.database.native.Registry
	}
	return options
}

// bindOptions returns the shared binding configuration for one statement.
func (scope *sessionScope) bindOptions(builtins builtin.ResolveOptions) bind.Options {
	options := bind.Options{
		SearchPath: scope.settings.SearchPath(), CurrentRole: scope.activeRole(), Collation: defaultCollation,
		DefaultCollation: defaultCollation, Limits: scope.database.irLimits(), Builtin: builtins,
	}
	if scope.database.native != nil {
		options.CatalogRelations = scope.database.native.Registry
	}
	return options
}

// declarationOptions returns the create/alter declaration binding for command
// families that declare catalog objects.
func (scope *sessionScope) declarationOptions(builtins builtin.ResolveOptions) bind.CreateDeclarationOptions {
	return bind.CreateDeclarationOptions{
		Bind:        scope.bindOptions(builtins),
		Descriptors: scope.database.limits,
		Physical:    model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
	}
}

// systemTimeOptions resolves AS OF SYSTEM TIME sidecars. The converter is the
// borrowed certified clock; expression binding shares the statement's builtin
// inventory so only immutable temporal expressions resolve.
func (scope *sessionScope) systemTimeOptions(builtins builtin.ResolveOptions) session.SystemTimeOptions {
	return session.SystemTimeOptions{
		Bind:        scope.bindOptions(builtins),
		Expressions: expr.Options{Limits: expr.DefaultLimits(), Builtin: builtins},
		Converter:   scope.database.clock,
		MaxClauses:  64, MaxBytes: 4 << 20,
	}
}

// typeRuntimeOptions describes how declared type runtimes compile against one
// pinned catalog snapshot.
func (scope *sessionScope) typeRuntimeOptions(builtins builtin.ResolveOptions) *session.DeclaredTypeRuntimeOptions {
	return scope.database.typeRuntimeOptions(builtins)
}

func (database *Catalog) typeRuntimeOptions(builtins builtin.ResolveOptions) *session.DeclaredTypeRuntimeOptions {
	if database.native != nil {
		builtins.InformationSchemaFunctions = database.native.Registry
		builtins.InformationSchemaTypes = database.native.Registry
	}
	return &session.DeclaredTypeRuntimeOptions{
		Descriptors: database.limits,
		Expressions: session.ColumnExpressionOptions{
			Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			Builtin: builtins,
		},
		Routines: database.proceduralLimits(),
	}
}

func (database *Catalog) proceduralLimits() procedural.Limits {
	limits := procedural.DefaultLimits()
	limits.MaxProgramBytes = 1 << 20
	limits.MaxStatements = 1024
	limits.MaxDatums = 1024
	limits.MaxSteps = 1 << 20
	limits.MaxRows = 1 << 20
	limits.MaxResultBytes = 16 << 20
	limits.MaxDeferredEvents = 1024
	limits.MaxTransitionRows = 1 << 20
	return limits
}

// statisticsCommandOptions wires ANALYZE and statistics DDL. Analyze is a
// bounds template; the session fills target, snapshot and layout per command
// and clamps the budgets by maintenance_work_mem.
func (scope *sessionScope) statisticsCommandOptions(builtins builtin.ResolveOptions) session.StatisticsCommandOptions {
	database := scope.database
	return session.StatisticsCommandOptions{
		Catalog: database.catalog, Store: database.statistics, KeyEncoder: database.keys,
		TypeRuntime: scope.typeRuntimeOptions(builtins),
		Binding:     scope.declarationOptions(builtins),
		Expressions: session.IndexExpressionOptions{
			Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
		},
		Analyze: controlstats.AnalyzeOptions{
			SampleRows: 30000, BatchRows: 1024, MostCommon: 100, HistogramBins: 100,
			MaxScanRows: 1 << 30, MaxBatches: 1 << 20,
			MaxLiveBytes: 64 << 20, MaxReservoirBytes: 16 << 20, MaxKeyBytes: 4096,
			Limits: controlstats.Limits{MaxBytes: 1 << 20, MaxColumns: 32, MaxValues: 1024, MaxDepth: 32},
		},
	}
}

// typeCommandOptions wires CREATE/ALTER/DROP TYPE, DOMAIN and their cascades.
func (scope *sessionScope) typeCommandOptions(builtins builtin.ResolveOptions) session.TypeCommandOptions {
	database := scope.database
	return session.TypeCommandOptions{
		Binding: scope.declarationOptions(builtins), MaxDescriptors: 64,
		Cascade: session.TypeCascadeOptions{
			Policies: database.policies,
			Scan:     schema.BackfillScanLimits{BatchRows: 64, MaxKeyBytes: 4096, WorkBytes: 8 << 20},
		},
		Validation: session.TypeDomainValidationOptions{
			Runtime: *scope.typeRuntimeOptions(builtins), BatchRows: 64, ValueBytes: 1 << 20,
		},
	}
}

// routineCommandOptions wires CREATE FUNCTION/PROCEDURE/TRIGGER and routine
// lifecycle commands. The procedural compiler is shared; every invocation binds
// against its own pinned snapshot.
func (scope *sessionScope) routineCommandOptions(builtins builtin.ResolveOptions) session.RoutineCommandOptions {
	return session.RoutineCommandOptions{
		TableCommandOptions: scope.tableCommandOptions(builtins),
		Compiler:            procedural.Compiler{Limits: scope.database.proceduralLimits()},
		MaxDescriptors:      64,
	}
}

// tableCommandOptions is the shared declaration base for routine and text
// search command families.
func (scope *sessionScope) tableCommandOptions(builtins builtin.ResolveOptions) session.TableCommandOptions {
	object := scope.objectCommandOptions(builtins)
	return session.TableCommandOptions{
		Binding: object.Binding, Dependencies: object.Dependencies, Columns: object.Columns,
		Indexes: object.Indexes, Limits: object.Limits, PolicyBytes: object.PolicyBytes,
		Constraints: object.Constraints,
	}
}

// textSearchCommandOptions wires text search configuration, dictionary, parser,
// template and mapping DDL.
func (scope *sessionScope) textSearchCommandOptions(builtins builtin.ResolveOptions) session.TableCommandOptions {
	return scope.tableCommandOptions(builtins)
}

// routineExecutionOptions wires SQL and PL/pgSQL routine invocation. Planning
// rebinds routine bodies against their own pinned snapshot; NewQuery owns each
// nested statement's bounded work account.
func (scope *sessionScope) routineExecutionOptions(builtins builtin.ResolveOptions) *session.RoutineExecutionOptions {
	database := scope.database
	if database.native == nil {
		return nil
	}
	return &session.RoutineExecutionOptions{
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, _ *zpgparse.RawStmt, _ []model.TypeLayout) (session.PrepareOptions, error) {
			return scope.prepareOptions(ctx, snapshot, reader, builtins)
		},
		NewQuery: database.newStatementQuery,
		Limits:   database.proceduralLimits(),
		Capabilities: session.RoutineTransactionCapabilities{
			Sequence: database.facade, CatalogRelations: database.native.Registry,
			BaseFunctions: unresolvedFunctions{}, BaseBuiltin: builtins, BaseKeys: database.keys, PathBytes: 4096,
		},
		RewriteStatement: func(ctx context.Context, raw *zpgparse.RawStmt) (*zpgparse.RawStmt, func(context.Context, *columnar.Batch) error, bool, error) {
			rewritten, checkPlan, err := database.rewriteStatementForRLS(ctx, scope, raw)
			if err != nil {
				return nil, nil, false, err
			}
			if checkPlan == nil {
				return rewritten, nil, false, nil
			}
			tableName := checkPlan.tableName
			hasUserReturning := checkPlan.hasUserReturning
			return rewritten, func(batchCtx context.Context, batch *columnar.Batch) error {
				return verifyRLSCheckBatch(batchCtx, batch, tableName)
			}, hasUserReturning, nil
		},
	}
}

// schemaCommandExecution configures one online or concurrent schema statement.
// It reserves the durable job and object identities in their own committed
// transaction — the schema command itself runs in a separate one — and borrows
// a current catalog snapshot for binding. The returned cleanup releases that
// snapshot after the statement finishes.
func (database *Catalog) schemaCommandExecution(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, builtins builtin.ResolveOptions) (execution *session.SchemaCommandExecution, cleanup func() error, err error) {
	if parsed == nil || parsed.ParseResult == nil || index < 0 || index >= len(parsed.ParseResult.Stmts) {
		return nil, nil, nil
	}
	statement := parsed.ParseResult.Stmts[index]
	if statement == nil || statement.Stmt == nil {
		return nil, nil, nil
	}
	node := statement.Stmt
	var online *zpgparse.OnlineDDLClause
	for clause := range parsed.OnlineDDLClauses {
		candidate := parsed.OnlineDDLClauses[clause]
		if candidate.StatementIndex == index && candidate.Location >= 0 {
			online = &candidate
			break
		}
	}
	alter := node.GetAlterTableStmt()
	drop := node.GetDropStmt()
	createIndex := node.GetIndexStmt()
	reindex := node.GetReindexStmt()
	alterOnline := online != nil && alter != nil
	dropConcurrent := drop != nil && drop.Concurrent && drop.RemoveType == zpgparse.ObjectType_OBJECT_INDEX
	createConcurrent := createIndex != nil && createIndex.Concurrent
	reindexConcurrent := bind.ReindexConcurrent(reindex)
	if !alterOnline && !dropConcurrent && !createConcurrent && !reindexConcurrent {
		return nil, nil, nil
	}
	allocation, err := database.allocateSchemaIdentity(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	view, err := database.manager.CurrentReadView(ctx)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := database.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, nil, errors.Join(err, view.Close())
	}
	release := func() error { return errors.Join(snapshot.Close(), view.Close()) }
	execution = &session.SchemaCommandExecution{
		Snapshot: snapshot, Online: online, StatementIndex: index,
		AlterDrop: session.OnlineSchemaOptions{
			Binding: scope.declarationOptions(builtins), Catalog: database.catalog, JobID: allocation.JobID,
			Lifecycle: database.lifecycle, Policies: database.policies, Scheduler: database.scheduler,
		},
	}
	if createConcurrent {
		expressions, compileErr := session.NewIndexExpressionCompiler(snapshot, session.IndexExpressionOptions{
			Binding: database.irLimits(), Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
		})
		if compileErr != nil {
			return nil, nil, errors.Join(compileErr, release())
		}
		execution.ConcurrentIndex = session.ConcurrentIndexOptions{
			Binding: bind.CreateIndexOptions{
				Bind: scope.bindOptions(builtins), Descriptors: database.limits,
				ObjectID: allocation.ObjectID, OID: allocation.OID,
				Physical:    model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
				Expressions: expressions,
			},
			JobID: allocation.JobID, Lifecycle: database.lifecycle, Policies: database.policies, Scheduler: database.scheduler,
		}
	}
	return execution, release, nil
}

// allocateSchemaIdentity reserves one ObjectID/OID/JobID triple in its own
// committed transaction. The reservation is durable even when the schema
// command itself later fails; unused committed reservations are never recycled.
func (database *Catalog) allocateSchemaIdentity(ctx context.Context, scope *sessionScope) (schema.IdentityAllocation, error) {
	var allocation schema.IdentityAllocation
	if err := scope.txns.Begin(ctx, session.TransactionStart{}); err != nil {
		return allocation, err
	}
	err := scope.txns.Statement(ctx, func(ctx context.Context, transaction *txn.Transaction) error {
		var allocateErr error
		allocation, allocateErr = database.identities.AllocateIdentity(ctx, transaction)
		return allocateErr
	})
	if err != nil {
		return allocation, errors.Join(err, scope.txns.Rollback(ctx, false))
	}
	commit, err := scope.txns.Commit(ctx, false)
	if err != nil {
		return allocation, err
	}
	if !commit.Committed {
		return allocation, &model.Error{Code: "55000", Message: "schema identity reservation did not commit"}
	}
	return allocation, nil
}

// newStatementQuery creates a fresh bounded work account for a named-object or
// nested routine statement. The caller owns and closes the returned query.
func (database *Catalog) newStatementQuery(ctx context.Context) (*runtime.Query, error) {
	return runtime.NewQuery(ctx, database.pool, database.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes: 128 << 20, OutputBytes: 16 << 20, SpillBytes: 1 << 30,
		SpillMode: runtime.SpillEnabled, Directory: database.spill, MaxFiles: 8, MaxReaders: 8,
	})
}

// unresolvedFunctions is the immutable fallback function resolver. Every
// builtin resolves through the builtin inventory and every routine through the
// procedural resolver; reaching this resolver means the identity names neither.
type unresolvedFunctions struct{}

func (unresolvedFunctions) Resolve(expr.Function) (expr.ScalarCall, error) {
	return nil, &model.Error{Code: "42883", Message: "function does not exist"}
}

// executionEnvironment assembles one invocation environment. Every budget is
// explicit; nothing here is inherited from a package-level default.
func (database *Catalog) executionEnvironment(query *runtime.Query, builtins builtin.ResolveOptions) exec.Environment {
	return exec.Environment{
		Query: query, Keys: database.keys, Builtin: builtins, Catalog: database.source,
		MutationTargets: newStackMutationTargets{descriptors: database.currentDescriptors(), policies: database.policies},
		Limits: exec.Limits{
			Expressions: expr.DefaultLimits(), BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 8 << 20,
		},
		// The shared spool owns one layout header and one append value header per
		// column, so its metadata budget admits the widest relation the catalog
		// accepts (IndexLimits.MaxColumns = 64) with room for reader state.
		Spool:     exrowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 1024, MaxRecordBytes: 4096, MetadataBytes: 16 << 10, DecodeBytes: 4096, MaxReaders: 8},
		Sort:      exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2},
		Join:      exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2},
		Aggregate: exaggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4},
		Set:       exsetop.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, PartitionMemoryBytes: 64 << 10, MaxSpillBytes: 16 << 20, WorkBytes: 16 << 20, MaxPasses: 16},
		Window:    exwindow.Options{MaxRecordBytes: 4096, PartitionMemoryBytes: 64 << 10, MaxPartitionBytes: 16 << 20, MaxPartitionRows: 10000, WorkBytes: 16 << 20},
		Apply:     exapply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10},
		Modify: exmodify.Options{
			// MaxRecordBytes sizes each row's completion workspace as well as the
			// record itself; a wide relation's normalization needs the same bound
			// every other operator uses.
			BatchRows: 2, BatchBytes: 1 << 20, MaxRecordBytes: 4096, MaxSpoolBytes: 8 << 20,
			MaxRows: 1000, WorkBytes: 64 << 20, SortRunBytes: 1 << 20, SortMaxRuns: 8, SortMergeFanIn: 2,
		},
		Recursive: exrecursive.Options{
			MaxIterations: 10000,
			Spool:         exrowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 8},
			Seen:          exdistinctset.Options{MemoryBytes: 4096, WorkBytes: 256 << 10, MaxKeyBytes: 4096, MaxKeys: 10000, MaxLevels: 8},
		},
	}
}

func (database *Catalog) currentDescriptors() *catalog.CurrentDescriptors {
	descriptors, err := catalog.NewCurrentDescriptors(database.catalog, database.manager)
	if err != nil {
		return nil
	}
	return descriptors
}

// Directory reports the database directory this instance was opened on.
func (database *Catalog) Directory() string { return database.directory }

// Close releases the session, allocator, transaction manager, backend and
// workers. It is safe to call repeatedly and reports the first error while still
// releasing every owned collaborator.
func (database *Catalog) Close() error {
	if database == nil {
		return nil
	}
	database.mu.Lock()
	defer database.mu.Unlock()
	if database.closed {
		return database.closeErr
	}
	database.closed = true
	database.closeErr = database.closeAll()
	return database.closeErr
}

// closeAll releases every constructed collaborator in ownership order, joining
// each failure so a single release error cannot strand the rest. Job workers
// stop before the session, and snapshot protections release before the
// transaction manager and backend they pin.
func (database *Catalog) closeAll() error {
	background := context.Background()
	var err error
	if database.workerStop != nil {
		database.workerStop()
	}
	if database.maintenanceStarted {
		// The blocking receive is what guarantees no Advance or Collect is in
		// flight when protections, the manager and the backend close.
		runErr := <-database.maintenanceErr
		if !errors.Is(runErr, context.Canceled) {
			err = errors.Join(err, runErr)
		}
		// A fatal exit already delivered its error; only an earlier transient
		// failure the loop recorded and continued past is still outstanding.
		if last := database.maintenanceLast.Load(); last != nil && !errors.Is(runErr, *last) {
			err = errors.Join(err, *last)
		}
	}
	if database.scheduler != nil {
		if database.schedulerErr != nil {
			select {
			case runErr := <-database.schedulerErr:
				if !errors.Is(runErr, context.Canceled) {
					err = errors.Join(err, runErr)
				}
			default:
			}
		}
		err = errors.Join(err, database.scheduler.Close())
	}
	if database.protections != nil {
		err = errors.Join(err, database.protections.Close())
	}
	if database.txns != nil {
		err = errors.Join(err, database.txns.Close())
	}
	if database.facade != nil {
		err = errors.Join(err, database.facade.Close(background))
	}
	if database.service != nil {
		err = errors.Join(err, database.service.Close(background))
	}
	if database.manager != nil {
		err = errors.Join(err, database.manager.Close())
	}
	if database.backend != nil {
		err = errors.Join(err, database.backend.Close())
	}
	if database.pool != nil {
		err = errors.Join(err, database.pool.Close())
	}
	return err
}

// clockUnavailable reports a nil or typed-nil Clock, which would otherwise
// satisfy the interface while panicking on first use.
func clockUnavailable(value clock.Clock) bool {
	if value == nil {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}

// readFenceSecretFile reads and validates the fence secret file in directory.
// It returns the raw read error (including os.ErrNotExist), reports corrupt when
// the file exists but fails its size or all-zero check, and returns the secret
// otherwise.
func readFenceSecretFile(directory string) ([nativeFenceSecretSize]byte, bool, error) {
	var secret [nativeFenceSecretSize]byte
	data, err := os.ReadFile(filepath.Join(directory, nativeFenceSecretFile))
	if err != nil {
		return secret, false, err
	}
	if len(data) != nativeFenceSecretSize {
		return secret, true, nil
	}
	copy(secret[:], data)
	if secret == ([nativeFenceSecretSize]byte{}) {
		return secret, true, nil
	}
	return secret, false, nil
}

// readFenceSecret returns the persisted closed-read fence secret. A directory
// without one is a database this composition has never bootstrapped, so a fresh
// secret is generated and persisted after the backend has published its state.
func readFenceSecret(directory string) ([nativeFenceSecretSize]byte, bool, error) {
	secret, corrupt, err := readFenceSecretFile(directory)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := rand.Read(secret[:]); err != nil {
			return secret, false, err
		}
		return secret, true, nil
	}
	if err != nil {
		return secret, false, err
	}
	if corrupt {
		return secret, false, fmt.Errorf("supersql: %s is corrupt", nativeFenceSecretFile)
	}
	return secret, false, nil
}

// writeFenceSecret persists a fresh secret exactly once, durably: the file
// sync covers the bytes and the directory sync covers the entry, so a crash
// cannot lose the secret while database.state survives — reopening would
// otherwise generate a new secret that fails the persisted read fence. It runs
// after the backend has published database.state, so the storage inspector —
// which lists a directory only until that record exists — never sees an
// unknown entry.
func writeFenceSecret(directory string, secret [nativeFenceSecretSize]byte) error {
	path := filepath.Join(directory, nativeFenceSecretFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(secret[:]); err != nil {
		return errors.Join(err, file.Close(), os.Remove(path))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close(), os.Remove(path))
	}
	if err := file.Close(); err != nil {
		return err
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	return errors.Join(syncErr, handle.Close())
}
