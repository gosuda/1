package modify_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func sequenceDDLFixture(t *testing.T) (*sessionExecutionFixture, *schema.SequenceStore, *schema.SequenceService, *session.SequenceSession, []txn.ReadDependency) {
	return sequenceDDLFixtureWithBackend(t, nil)
}

func sequenceDDLFixtureWithBackend(t *testing.T, wrap func(storage.Backend) storage.Backend) (*sessionExecutionFixture, *schema.SequenceStore, *schema.SequenceService, *session.SequenceSession, []txn.ReadDependency) {
	t.Helper()
	f := newSessionExecutionFixture(t)
	if wrap != nil {
		if err := errors.Join(f.session.Close(), f.store.manager.Close()); err != nil {
			t.Fatal(err)
		}
		state, err := f.store.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		f.store.manager, err = txn.NewManager(t.Context(), txn.ManagerConfig{Backend: wrap(f.store.backend), Clock: f.store.clock, Authority: f.store.authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := schema.NewSequenceStore(t.Context(), schema.SequenceStoreOptions{Target: storage.ObjectRef{ObjectID: 90, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.store.options.Encoder, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 4, MaxCache: 8})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := schema.NewJobStore(t.Context(), schema.JobStoreOptions{Target: storage.ObjectRef{ObjectID: 91, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.store.options.Encoder, MetadataBytes: 1 << 20, RecordBytes: 64 << 10, KeyBytes: 4096, MaxJobs: 8, BatchRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapOptions := f.transactionOptions
	bootstrapOptions.Resolver = store.WithBaseResolver(jobs.WithBaseResolver(f.transactionOptions.Resolver))
	bootstrap, err := f.store.manager.Begin(t.Context(), bootstrapOptions)
	if err != nil {
		t.Fatal(err)
	}
	err = bootstrap.Statement(t.Context(), func(ctx context.Context) error {
		if err := store.InitializeAllocator(ctx, bootstrap); err != nil {
			return err
		}
		return jobs.InitializeIdentities(ctx, bootstrap, schema.IdentityHighWaterMarks{ObjectID: 100, OID: 10100, JobID: 100})
	})
	if err == nil {
		_, err = bootstrap.Commit(t.Context())
	}
	if err = errors.Join(err, bootstrap.Close(context.Background())); err != nil {
		t.Fatal(err)
	}
	service, err := schema.NewSequenceService(t.Context(), schema.SequenceServiceOptions{Manager: f.store.manager, Store: store, Transactions: f.transactionOptions, MaxOperations: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	facade, err := session.NewSequenceSession(session.SequenceSessionOptions{Allocations: service, MaxSequences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := facade.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := f.session.Close(); err != nil {
		t.Fatal(err)
	}
	bindingLimits := f.planning("SELECT 7").Bind.Limits
	bindingLimits.SourceBytes = 1 << 20
	columns := session.ColumnExpressionOptions{Binding: bindingLimits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20, Builtin: f.settings.BuiltinOptions(950, 950)}
	indexes := session.IndexExpressionOptions{Binding: bindingLimits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20}
	policies, err := schema.NewIndexPolicyProvider(t.Context(), schema.IndexPolicyOptions{Catalog: f.catalog, Views: f.store.manager, Jobs: jobs, Encoders: func(context.Context, *catalog.CatalogSnapshot) (schema.KeyCodec, error) {
		codec, ok := f.store.options.Encoder.(schema.KeyCodec)
		if !ok {
			return nil, storage.ErrInvalidLayout
		}
		return codec, nil
	}, Columns: columns.ForSnapshot, Expressions: indexes.ForSnapshot, Limits: schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64}})
	if err != nil {
		t.Fatal(err)
	}
	f.transactionOptions.Resolver = policies
	f.session, err = session.NewTransactions(t.Context(), session.TransactionConfig{Manager: f.store.manager, Settings: f.settings, Options: f.transactionOptions, TemporalClock: f.store.clock, SequenceDDL: &session.SequenceDDLConfig{Catalog: f.catalog, Identities: jobs, Service: service, Session: facade, MaxChanges: 16, MemoryBytes: 64 << 10, CleanupTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	// The replacement owner must close before its borrowed sequence service.
	t.Cleanup(func() {
		if err := f.session.Close(); err != nil {
			t.Error(err)
		}
	})
	var dependencies []txn.ReadDependency
	for _, layout := range f.catalog.Layouts() {
		dependencies = append(dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: []storage.KeyColumn{{ColumnID: layout.Columns[0].ID, Type: layout.Columns[0].Type}}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
	}
	return f, store, service, facade, dependencies
}
func executeSequenceDDL(t *testing.T, f *sessionExecutionFixture, dependencies []txn.ReadDependency, sql string) (session.SequenceCommandResult, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := session.SequenceCommandOptions{Binding: bind.CreateDeclarationOptions{Bind: f.planning(sql).Bind, Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}, Owner: (model.Principal{}).OID(), Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}}, Dependencies: dependencies}
	if create := parsed.Stmts[0].Stmt.GetCreateSeqStmt(); create != nil {
		return session.ExecuteCreateSequence(t.Context(), f.session, create, options)
	}
	return session.ExecuteAlterSequence(t.Context(), f.session, parsed.Stmts[0].Stmt.GetAlterSeqStmt(), options)
}
func runSequenceDDLQuery(t *testing.T, f *sessionExecutionFixture, facade *session.SequenceSession, dependencies []txn.ReadDependency, sql string) ([][]int64, error) {
	return runDeclarationQuery(t, f, facade, dependencies, sql, nil)
}

func runDeclarationQuery(t *testing.T, f *sessionExecutionFixture, facade *session.SequenceSession, dependencies []txn.ReadDependency, sql string, mutations execution.MutationTargets) ([][]int64, error) {
	t.Helper()
	var view txn.ReadView
	var err error
	if f.session.Active() {
		view, err = f.session.ReadView(t.Context(), dependencies)
	} else {
		view, err = f.store.manager.CurrentReadView(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
			t.Error(err)
		}
	}()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := f.planning(sql)
	access, err := session.NewCatalogAccess(snapshot, view, f.store.options.Encoder, session.CatalogAccessOptions{MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1, IndexLimits: schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64}, IndexExpressions: session.IndexExpressionOptions{Binding: options.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20}})
	if err != nil {
		return nil, err
	}
	options.Optimize.Access = access
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], snapshot, options, nil)
	if err != nil {
		return nil, err
	}
	sequences, err := session.NewSequenceRuntime(snapshot, facade)
	if err != nil {
		return nil, err
	}
	relations, err := session.NewRelationRuntime(snapshot, nil, []string{"public"}, 4096)
	if err != nil {
		return nil, err
	}
	invocation := mergeInvocation(t, f)
	invocation.Environment.Sort = externalsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2}
	invocation.Environment.Aggregate = aggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4}
	derived, _, err := prepared.ReadDependencies(t.Context(), f.catalog, 4<<20)
	if err != nil {
		return nil, err
	}
	invocation.Dependencies = append(append([]txn.ReadDependency(nil), dependencies...), derived...)
	if mutations != nil {
		invocation.Environment.MutationTargets = mutations
	}
	invocation.Environment.Sequences = sequences
	invocation.Environment.Relations = relations
	var got [][]int64
	_, err = f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	return got, err
}
func requireSequenceDDLRows(t *testing.T, f *sessionExecutionFixture, facade *session.SequenceSession, dependencies []txn.ReadDependency, sql string, want [][]int64) {
	t.Helper()
	got, err := runSequenceDDLQuery(t, f, facade, dependencies, sql)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: %v,%v; want %v", sql, got, err, want)
	}
}

func TestSequenceRestartRollbackRetainsOldProgression(t *testing.T) {
	f, _, _, facade, dependencies := sequenceDDLFixture(t)
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
	if _, err := f.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Savepoint(t.Context(), "before_restart"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers RESTART WITH 50`); err != nil {
			t.Fatal(err)
		}
		requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{50}})
		if err := f.session.RollbackTo(t.Context(), "before_restart"); err != nil {
			t.Fatal(err)
		}
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT currval('numbers'),nextval('numbers')`, [][]int64{{50, 2}})
	if _, err := f.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers START WITH 90`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{3}})
	if _, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers RESTART`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{90}})
}

func TestRolledBackSequenceCreationDoesNotLeakSessionIdentity(t *testing.T) {
	f, _, _, facade, dependencies := sequenceDDLFixture(t)
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
	if err := f.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	_, err := runSequenceDDLQuery(t, f, facade, dependencies, `SELECT currval('numbers')`)
	requireUniqueSQLState(t, err, "55000")
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
}

func TestSequenceStartupReclaimsOnlyUnpublishedIncarnations(t *testing.T) {
	f, store, service, _, dependencies := sequenceDDLFixture(t)
	created, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`)
	if err != nil {
		t.Fatal(err)
	}
	live, ok := created.Descriptor.Sequence()
	if !ok {
		t.Fatal("missing sequence metadata")
	}
	abandoned, err := service.Initialize(t.Context(), schema.SequenceConfig{Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 1})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	err = schema.ReconcileSequences(t.Context(), service, snapshot, view, 64<<10)
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		t.Fatal(err)
	}
	view, err = f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := store.Get(t.Context(), view, schema.SequenceIncarnation(live.Incarnation)); err != nil {
		t.Fatalf("live sequence reclaimed: %v", err)
	}
	if _, err := store.Get(t.Context(), view, abandoned.Incarnation); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("abandoned incarnation remains: %v", err)
	}
}
