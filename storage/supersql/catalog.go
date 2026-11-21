package supersql

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	controlstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/notify"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

// Catalog is one opened native database. It owns the backend, workers,
// transaction manager and session services, and borrows the host clock.
type Catalog struct {
	directory   string
	clock       clock.Clock
	keys        *builtin.KeyEncoder
	authority   *txn.FenceAuthority
	backend     *neosepia.Backend
	waitGraph   *locks.WaitGraph
	manager     *txn.Manager
	catalog     *catalog.Store
	statistics  *controlstats.Store
	sequences   *schema.SequenceStore
	identities  *schema.JobStore
	indexJobs   *schema.JobStore
	service     *schema.SequenceService
	settings    *session.Settings
	txns        *session.Transactions
	embedded    *sessionScope
	policies    *schema.IndexPolicyProvider
	lifecycle   *schema.IndexLifecycle
	protections *schema.JobProtections
	retention   *schema.RetentionCoordinator
	scheduler   *schema.IndexJobScheduler
	advisory    *locks.AdvisoryLockManager
	hub         *notify.Hub
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
	sessionSeq    atomic.Uint64

	historyRetention       time.Duration
	parallelism            int
	memoryBudgetBytes      uint64
	transactionMemoryBytes uint64
	transactionSpillBytes  uint64
	maxTransactions        int
	maxOperations          int

	workerCtx        context.Context
	workerStop       context.CancelFunc
	schedulerErr     chan error
	schedulerStarted bool

	maintenance        MaintenanceConfig
	maintenanceErr     chan error
	maintenanceStarted bool
	maintenanceLast    atomic.Pointer[error]

	observer           QueryObserver
	slowQueryThreshold time.Duration
	onSlowQuery        func(context.Context, QueryEvent)
	observing          atomic.Bool
	queriesTotal       atomic.Uint64
	queriesFailed      atomic.Uint64
	slowQueriesTotal   atomic.Uint64
	autoAnalyze        *autoAnalyzeTracker

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
	if options.HistoryRetention < 0 {
		return nil, wrapError(&model.Error{Code: "22023", Message: "invalid history retention duration"})
	}
	if options.SlowQueryThreshold < 0 {
		return nil, wrapError(&model.Error{Code: "22023", Message: "invalid slow query threshold"})
	}
	maintenance, err := normalizedMaintenance(options.Maintenance)
	if err != nil {
		return nil, wrapError(err)
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
	storageLimits := mergeCompactionConfig(options.Compaction)
	backend, err := neosepia.Open(ctx, neosepia.Config{
		Directory: options.Directory, Scale: options.Clock.Scale(), KeyEncoder: keys,
		FenceVerifier: authority, Limits: storageLimits,
		ExtraCheckpointFiles: []string{nativeFenceSecretFile},
		EncryptionKey:        options.EncryptionKey,
		WALArchiveDir:        options.WALArchiveDir,
		WALArchiveKey:        options.WALArchiveKey,
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
		limits:                 model.DescriptorLimits{MaxBytes: 8 << 20, MaxItems: 65536, MaxDepth: 64},
		spill:                  filepath.Join(options.Directory, nativeSpoolDirectory),
		readOnly:               options.HistoricalReadOnly,
		maintenance:            maintenance,
		historyRetention:       options.HistoryRetention,
		parallelism:            options.Parallelism,
		memoryBudgetBytes:      options.MemoryBudgetBytes,
		transactionMemoryBytes: options.TransactionMemoryBytes,
		transactionSpillBytes:  options.TransactionSpillBytes,
		maxTransactions:        options.MaxTransactions,
		maxOperations:          options.MaxOperations,
		observer:               options.Observer,
		slowQueryThreshold:     options.SlowQueryThreshold,
		onSlowQuery:            options.OnSlowQuery,
		autoAnalyze:            newAutoAnalyzeTracker(),
	}
	database.workerCtx, database.workerStop = context.WithCancel(context.Background())
	database.schedulerErr = make(chan error, 1)
	database.maintenanceErr = make(chan error, 1)
	if err := database.compose(ctx); err != nil {
		return nil, wrapError(errors.Join(err, database.closeAll()))
	}
	// Enable user query observation only after compose succeeds so internal
	// bootstrap DDL is never counted or delivered to user observers, without
	// mutating hook fields after background goroutines have started.
	database.queriesTotal.Store(0)
	database.queriesFailed.Store(0)
	database.slowQueriesTotal.Store(0)
	database.observing.Store(true)
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
	// Share one WaitGraph across both transactional locks and advisory locks so
	// cross-registry deadlocks (row/table lock <-> advisory lock) are detected
	// immediately with SQLSTATE 40P01.
	waitGraph, err := locks.NewWaitGraph(locks.DefaultWaitGraphLimits())
	if err != nil {
		return err
	}
	database.waitGraph = waitGraph
	database.manager, err = txn.NewManager(ctx, txn.ManagerConfig{
		Backend: database.backend, Clock: database.clock, Authority: database.authority,
		Recovered: txn.RecoveredState{
			LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS,
			HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID,
		},
		MaxOperations: maxOps, MaxTransactions: maxTxns,
		WaitGraph:          waitGraph,
		HistoricalReadOnly: database.readOnly,
	})
	if err != nil {
		return err
	}
	if database.catalog, err = catalog.New(database.keys, catalog.Limits{
		Descriptor: database.limits, MaxEncodedBytes: 8 << 20, MaxKeyBytes: 4096, ScanBatchRows: 64,
	}); err != nil {
		return err
	}
	if database.statistics, err = controlstats.New(ctx,
		storage.ObjectRef{ObjectID: nativeStatisticsObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: controlstats.EncodingVersion},
		database.keys,
		controlstats.Limits{MaxBytes: 16 << 20, MaxColumns: 1024, MaxValues: 4096, MaxDepth: 32},
		4096,
	); err != nil {
		return err
	}
	if database.sequences, err = schema.NewSequenceStore(ctx, schema.SequenceStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeSequenceObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 4096, MaxCache: 4096,
	}); err != nil {
		return err
	}
	if database.identities, err = schema.NewJobStore(ctx, schema.JobStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeIdentityObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 64 << 10, KeyBytes: 4096, MaxJobs: 256, BatchRows: 64,
	}); err != nil {
		return err
	}
	if database.indexJobs, err = schema.NewJobStore(ctx, schema.JobStoreOptions{
		Target:  storage.ObjectRef{ObjectID: nativeIndexObjectID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: database.keys, MetadataBytes: 1 << 20, RecordBytes: 1 << 16, KeyBytes: 4096, MaxJobs: 256, BatchRows: 64,
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
		Limits: schema.IndexLimits{MetadataBytes: 8 << 20, EntryBytes: 32 << 20, MaxColumns: 1024},
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
		Manager: database.manager, Store: database.sequences, Transactions: options, MaxOperations: 64,
	}); err != nil {
		return err
	}
	if database.lifecycle, err = schema.NewIndexLifecycle(schema.IndexLifecycleOptions{
		Catalog: database.catalog, Jobs: database.indexJobs, Limits: database.limits,
	}); err != nil {
		return err
	}
	if database.protections, err = schema.NewJobProtections(schema.JobProtectionOptions{
		Jobs: database.indexJobs, Snapshots: database.manager, MaxJobs: 256, MemoryBytes: 16 << 20,
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
			Backfill:   schema.BackfillLimits{MaxRows: 1 << 30, MaxKeyBytes: 4 << 20, WorkBytes: 128 << 20},
			Validation: schema.IndexValidationLimits{BatchRows: 64, MaxRows: 1 << 30, MaxBytes: 32 << 20},
			MaxRetries: 8, RetryDelay: 10 * time.Millisecond, MaxRetryDelay: time.Second,
		},
		MaxRunning: 4, MemoryBytes: 32 << 20, PollInterval: 5 * time.Second,
	}); err != nil {
		return err
	}
	advisoryLimits := locks.DefaultAdvisoryLimits()
	advisoryLimits.WaitGraph = waitGraph
	if database.advisory, err = locks.NewAdvisoryLockManagerWithLimits(advisoryLimits); err != nil {
		return err
	}
	database.hub = notify.NewHub(0)
	embeddedSequences, err := session.NewSequenceSession(session.SequenceSessionOptions{
		Allocations: database.service, MaxSequences: 256,
	})
	if err != nil {
		return err
	}
	if database.txns, err = session.NewTransactions(ctx, database.sessionConfig(database.settings, embeddedSequences, 1)); err != nil {
		return errors.Join(err, embeddedSequences.Close(context.Background()))
	}
	database.embedded = &sessionScope{
		database:  database,
		settings:  database.settings,
		sequences: embeddedSequences,
		txns:      database.txns,
		id:        1,
	}
	if !database.readOnly {
		database.schedulerStarted = true
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

func (database *Catalog) checkOpen() error {
	database.mu.Lock()
	defer database.mu.Unlock()
	if database.closed {
		return ErrDatabaseClosed
	}
	return nil
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
	if database.schedulerStarted {
		// Block until the scheduler goroutine exits so scheduler.Close() and
		// protections.Close() never race with an in-flight job scan or backfill.
		runErr := <-database.schedulerErr
		if !errors.Is(runErr, context.Canceled) {
			err = errors.Join(err, runErr)
		}
	}
	if database.scheduler != nil {
		err = errors.Join(err, database.scheduler.Close())
	}
	if database.protections != nil {
		err = errors.Join(err, database.protections.Close())
	}
	if database.embedded != nil {
		err = errors.Join(err, database.embedded.close())
	} else if database.txns != nil {
		err = errors.Join(err, database.txns.Close())
	}
	if database.hub != nil {
		database.hub.Close()
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

// writeFenceSecret persists a fresh secret atomically and durably: write to a
// staging file, fsync, close, rename into place, and sync the parent directory,
// so a crash cannot leave a truncated fence.secret or lose it while
// database.state survives.
func writeFenceSecret(directory string, secret [nativeFenceSecretSize]byte) error {
	path := filepath.Join(directory, nativeFenceSecretFile)
	if _, err := os.Stat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pendingPath := filepath.Join(directory, nativeFenceSecretFile+".pending")
	file, err := os.OpenFile(pendingPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(secret[:]); err != nil {
		return errors.Join(err, file.Close(), os.Remove(pendingPath))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close(), os.Remove(pendingPath))
	}
	if err := file.Close(); err != nil {
		return errors.Join(err, os.Remove(pendingPath))
	}
	if err := os.Rename(pendingPath, path); err != nil {
		return errors.Join(err, os.Remove(pendingPath))
	}
	return syncDirectory(directory)
}
