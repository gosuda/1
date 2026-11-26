package supersql

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	controlstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

// sessionScope is one SQL session: its own settings, per-session sequence state
// (currval/lastval), and transaction orchestration. The embedded exec path uses
// embeddedScope(), while connector connections each own a distinct scope so
// per-session state (settings, sequences, prepared statements, cursors,
// advisory locks, temporary relations) never crosses connections.
type sessionScope struct {
	database  *Catalog
	settings  *session.Settings
	sequences *session.SequenceSession
	txns      *session.Transactions
	id        uint64
	ddlSeq    atomic.Uint64
}

func (database *Catalog) settingsOptions() session.SettingsOptions {
	return session.SettingsOptions{
		Resolver: newStackSettings{database: database},
		Limits: session.SettingLimits{
			MaxValueBytes: 4096, MaxSavepoints: 64,
			WorkMemoryBytes: 128 << 20, MaintenanceMemoryBytes: 128 << 20, TemporaryFileBytes: 1 << 30,
		},
	}
}

// embeddedScope is the session that owns internal bookkeeping transactions and
// in-package exec helpers. Its advisory session ID is reserved as 1.
func (database *Catalog) embeddedScope() *sessionScope {
	if database.embedded != nil {
		return database.embedded
	}
	return &sessionScope{database: database, settings: database.settings, txns: database.txns, id: 1}
}

// sessionConfig is one session's transaction orchestration configuration:
// per-session settings, per-session sequence state, and advisory identity over
// the shared manager.
func (database *Catalog) sessionConfig(settings *session.Settings, sequences *session.SequenceSession, id uint64) session.TransactionConfig {
	return session.TransactionConfig{
		Manager: database.manager, Settings: settings, Options: database.resolvedTransactionOptions(), TemporalClock: database.clock,
		TypeRuntime: database.typeRuntimeOptions(settings.BuiltinOptions(defaultCollation, defaultCollation)),
		SequenceDDL: &session.SequenceDDLConfig{
			Catalog: database.catalog, Identities: database.identities, Service: database.service, Session: sequences,
			MaxChanges: 256, MemoryBytes: 1 << 20, CleanupTimeout: nativeCleanupTimeout,
		},
		NamedObjects: session.SessionNamedObjectOptions{
			NewQuery: database.newStatementQuery, MaxPrepared: 256, MaxCursors: 64,
			MetadataBytes: 16 << 20, PlanningCostUnitsPerSecond: 1e6,
		},
		Advisory: session.AdvisoryLockConfig{Manager: database.advisory, SessionID: id},
		Notify:   session.NotifyConfig{Hub: database.hub, SessionID: id},
	}
}

// newSession builds one isolated session for a driver connection. Close
// releases every session-scoped object.
func (database *Catalog) newSession(ctx context.Context) (_ *sessionScope, err error) {
	settings, err := session.NewSettings(ctx, database.settingsOptions())
	if err != nil {
		return nil, err
	}
	sequences, err := session.NewSequenceSession(session.SequenceSessionOptions{
		Allocations: database.service, MaxSequences: 256,
	})
	if err != nil {
		return nil, err
	}
	id := database.sessionSeq.Add(1) + 1
	txns, err := session.NewTransactions(ctx, database.sessionConfig(settings, sequences, id))
	if err != nil {
		return nil, errors.Join(err, sequences.Close(context.Background()))
	}
	return &sessionScope{
		database:  database,
		settings:  settings,
		sequences: sequences,
		txns:      txns,
		id:        id,
	}, nil
}

func (scope *sessionScope) close() error {
	if scope == nil {
		return nil
	}
	var err error
	if scope.txns != nil {
		err = errors.Join(err, scope.txns.Close())
	}
	if scope.sequences != nil {
		err = errors.Join(err, scope.sequences.Close(context.Background()))
	}
	return err
}

func (database *Catalog) irLimits() ir.Limits {
	return ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: 1 << 20}
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
		SearchPath: scope.settings.SearchPath(), CurrentRole: scope.settings.CurrentRole(), SessionUser: scope.settings.SessionUser(),
		Collation: defaultCollation, DefaultCollation: defaultCollation, Limits: scope.database.irLimits(), Builtin: builtins,
		TextSearch: scope.database.customTextSearchRuntime(),
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

// statisticsProviderAdapter exposes committed ANALYZE table statistics from the
// controlplane statistics store to the cost-based optimizer without widening
// transactional read-lock spans or holding snapshots open across optimization.
type statisticsProviderAdapter struct {
	store   *controlstats.Store
	manager *txn.Manager
	ts      model.Timestamp
}

var _ opt.Statistics = (*statisticsProviderAdapter)(nil)

func (a *statisticsProviderAdapter) Lookup(ctx context.Context, target storage.ObjectRef) (*controlstats.Table, bool, error) {
	if a == nil || a.store == nil || a.manager == nil {
		return nil, false, nil
	}
	var reader storage.SnapshotReader
	var snapTS model.Timestamp
	if a.ts > 0 {
		if historical, err := a.manager.HistoricalSnapshot(ctx, a.ts); err == nil {
			reader = historical
			snapTS = a.ts
		} else if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
	}
	if reader == nil {
		view, err := a.manager.ClosedReadView(ctx)
		if errors.Is(err, txn.ErrNoClosedFrontier) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		reader = view
		snapTS = view.Timestamp()
	}
	defer func() { _ = reader.Close() }()
	snapshot, err := a.store.Snapshot(reader, snapTS)
	if err != nil {
		return nil, false, err
	}
	return snapshot.Lookup(ctx, target)
}

// prepareOptions supplies the immutable planning collaborators for one
// statement: the catalog access planner is bound to the snapshot and reader the
// caller already pinned, and committed ANALYZE statistics are wired into the
// optimizer.
func (scope *sessionScope) prepareOptions(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, builtins builtin.ResolveOptions) (session.PrepareOptions, error) {
	database := scope.database
	limits := database.irLimits()
	textSearch := database.customTextSearchRuntime()
	if snapshot != nil {
		if runtimeTS, tsErr := session.NewTextSearchRuntimeWithCustom(snapshot, scope.settings.SearchPath(), 4096, nil, nil, database.textSearchRegistry); tsErr == nil {
			textSearch = runtimeTS
		}
	}
	bindOpts := scope.bindOptions(builtins)
	bindOpts.TextSearch = textSearch
	options := session.PrepareOptions{
		Bind:      bindOpts,
		Normalize: normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtins, TextSearch: textSearch},
		Optimize: opt.Options{
			IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20},
			MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 1600, MaxAccessPaths: 64, WorkingMemory: 16 << 20,
			DefaultCollation: builtins.DefaultCollation,
		},
		Fragment: fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20},
	}
	if database.statistics != nil && database.manager != nil {
		var statsTS model.Timestamp
		if stamped, ok := reader.(interface{ Timestamp() model.Timestamp }); ok {
			statsTS = stamped.Timestamp()
		}
		options.Optimize.Statistics = &statisticsProviderAdapter{
			store:   database.statistics,
			manager: database.manager,
			ts:      statsTS,
		}
		options.Optimize.StatisticsLimits = database.statistics.Limits()
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
		MaxPaths: 64, MaxBytes: 4 << 20, CapabilityEpoch: 1,
		IndexLimits:      schema.IndexLimits{MetadataBytes: 8 << 20, EntryBytes: 32 << 20, MaxColumns: 1024},
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

func (database *Catalog) currentDescriptors() *catalog.CurrentDescriptors {
	descriptors, err := catalog.NewCurrentDescriptors(database.catalog, database.manager)
	if err != nil {
		return nil
	}
	return descriptors
}

// unresolvedFunctions is the immutable fallback function resolver. Every
// builtin resolves through the builtin inventory and every routine through the
// procedural resolver; reaching this resolver means the identity names neither.
type unresolvedFunctions struct{}

func (unresolvedFunctions) Resolve(expr.Function) (expr.ScalarCall, error) {
	return nil, &model.Error{Code: "42883", Message: "function does not exist"}
}
