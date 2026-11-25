package modify_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	controlstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type nativeCatalogMetadata struct{}

func (nativeCatalogMetadata) CatalogFeatures(ctx context.Context) ([]catalogrel.SQLFeature, error) {
	return nil, ctx.Err()
}

func (nativeCatalogMetadata) CatalogSQLParts(ctx context.Context) ([]catalogrel.SQLPart, error) {
	return nil, ctx.Err()
}

func (nativeCatalogMetadata) CatalogSQLSizing(ctx context.Context) ([]catalogrel.SQLSizing, error) {
	return nil, ctx.Err()
}

type nativeRoutineBaseResolver struct{}

func (nativeRoutineBaseResolver) Resolve(expr.Function) (expr.ScalarCall, error) {
	return nil, &model.Error{Code: "42883", Message: "routine is not present in the native fixture"}
}

type nativeInvocationQueries struct {
	mu       sync.Mutex
	original *runtime.Query
	all      []*runtime.Query
}

type nativeRoutineFixture struct {
	*sessionExecutionFixture
	native           *session.NativeCatalog
	sequences        *session.SequenceSession
	source           *catalogrel.Source
	keys             *builtin.KeyEncoder
	descriptorLimits model.DescriptorLimits
	limits           procedural.Limits
}

func newNativeRoutineFixture(t *testing.T) *nativeRoutineFixture {
	t.Helper()
	return newNativeRoutineFixtureFrom(t, newSessionExecutionFixture(t))
}

// newNullableNativeRoutineFixture is newNativeRoutineFixture over the NULL-able
// value column, for routine/aggregate semantics that observe a real NULL row.
func newNullableNativeRoutineFixture(t *testing.T) *nativeRoutineFixture {
	t.Helper()
	return newNativeRoutineFixtureFrom(t, newNullableSessionExecutionFixture(t))
}

func newNativeRoutineFixtureFrom(t *testing.T, base *sessionExecutionFixture) *nativeRoutineFixture {
	t.Helper()
	if err := base.session.Close(); err != nil {
		t.Fatal(err)
	}
	keys, ok := base.store.options.Encoder.(*builtin.KeyEncoder)
	if !ok {
		t.Fatal("session fixture does not use the native key encoder")
	}
	sequenceStore, err := schema.NewSequenceStore(t.Context(), schema.SequenceStoreOptions{
		Target:  storage.ObjectRef{ObjectID: 90, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: keys, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 4, MaxCache: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := schema.NewJobStore(t.Context(), schema.JobStoreOptions{
		Target:  storage.ObjectRef{ObjectID: 91, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder: keys, MetadataBytes: 1 << 20, RecordBytes: 64 << 10, KeyBytes: 4096, MaxJobs: 64, BatchRows: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	transactionOptions := base.transactionOptions
	transactionOptions.Resolver = sequenceStore.WithBaseResolver(identities.WithBaseResolver(transactionOptions.Resolver))
	bootstrap, err := base.store.manager.Begin(t.Context(), transactionOptions)
	if err != nil {
		t.Fatal(err)
	}
	err = bootstrap.Statement(t.Context(), func(ctx context.Context) error {
		if err := sequenceStore.InitializeAllocator(ctx, bootstrap); err != nil {
			return err
		}
		return identities.InitializeIdentities(ctx, bootstrap, schema.IdentityHighWaterMarks{ObjectID: 100, OID: 20000, JobID: 100})
	})
	if err == nil {
		_, err = bootstrap.Commit(t.Context())
	}
	if err = errors.Join(err, bootstrap.Close(context.Background())); err != nil {
		t.Fatal(err)
	}
	sequenceService, err := schema.NewSequenceService(t.Context(), schema.SequenceServiceOptions{
		Manager: base.store.manager, Store: sequenceStore, Transactions: transactionOptions, MaxOperations: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sequenceService.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	sequences, err := session.NewSequenceSession(session.SequenceSessionOptions{Allocations: sequenceService, MaxSequences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sequences.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	base.transactionOptions = transactionOptions
	base.session, err = session.NewTransactions(t.Context(), session.TransactionConfig{
		Manager: base.store.manager, Settings: base.settings, Options: transactionOptions,
		SequenceDDL: &session.SequenceDDLConfig{
			Catalog: base.catalog, Identities: identities, Service: sequenceService, Session: sequences,
			MaxChanges: 16, MemoryBytes: 64 << 10, CleanupTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := base.session.Close(); err != nil {
			t.Error(err)
		}
	})
	descriptors := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	binding := base.planning("").Bind
	binding.Limits.SourceBytes = 1 << 20
	binding.Builtin = base.settings.BuiltinOptions(950, 950)
	native, err := session.BootstrapNativeCatalog(t.Context(), base.session, session.NativeCatalogConfig{
		Catalog: base.catalog, Identities: identities, Binding: binding, Descriptors: descriptors, ManifestBytes: 8 << 20,
	})
	if err != nil {
		var diagnostic *model.Error
		if errors.As(err, &diagnostic) && diagnostic.Detail != "" {
			t.Fatalf("%v\ndetail: %s\nposition: %d", err, diagnostic.Detail, diagnostic.Position)
		}
		t.Fatal(err)
	}
	base.dependencies = append(base.dependencies,
		base.catalog.DescriptorDependency(native.Identity.PgCatalogObjectID),
		base.catalog.DescriptorDependency(native.Identity.InformationSchema.ObjectID),
	)
	statistics, err := controlstats.New(
		t.Context(),
		storage.ObjectRef{ObjectID: 92, Generation: 1, DescriptorVersion: 1, EncodingVersion: controlstats.EncodingVersion},
		keys,
		controlstats.Limits{MaxBytes: 1 << 20, MaxColumns: 32, MaxValues: 1024, MaxDepth: 32},
		4096,
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := model.Principal{}
	source, err := catalogrel.NewSource(catalogrel.SourceOptions{
		Snapshot:   catalogrel.CatalogStoreFactory{Store: base.catalog},
		Statistics: catalogrel.StatisticsStoreFactory{Store: statistics},
		Sequences:  sequenceStore,
		Features:   nativeCatalogMetadata{},
		Standards:  nativeCatalogMetadata{},
		Database: catalogrel.Database{
			OID: 16384, Name: "fixture", Owner: principal.OID(), Encoding: 6,
			Collation: "C", CharacterType: "C", LocaleProvider: 'c', CollationOID: 950,
			AllowConnections: true, ConnectionLimit: -1,
			RoleOID: principal.OID(), RoleName: principal.Name(), RoleInherit: true,
			RoleCanLogin: true, RoleConnectionLimit: -1,
		},
		Registry: native.Registry,
		MaxRows:  1024, MaxBytes: 4 << 20, MaxDescriptors: 1024,
		MaxDescriptorBytes: descriptors.MaxBytes, MaxDescriptorItems: descriptors.MaxItems, MaxDescriptorDepth: descriptors.MaxDepth,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits := procedural.DefaultLimits()
	limits.MaxProgramBytes = 1 << 20
	limits.MaxStatements = 64
	limits.MaxDatums = 64
	limits.MaxSteps = 10_000
	limits.MaxRows = 64
	limits.MaxResultBytes = 1 << 20
	limits.MaxDeferredEvents = 64
	limits.MaxTransitionRows = 64
	return &nativeRoutineFixture{
		sessionExecutionFixture: base,
		native:                  native,
		sequences:               sequences,
		source:                  source,
		keys:                    keys,
		descriptorLimits:        descriptors,
		limits:                  limits,
	}
}

func (fixture *nativeRoutineFixture) configurePlanning(options *session.PrepareOptions) {
	builtins := fixture.settings.BuiltinOptions(950, 950)
	builtins.InformationSchemaFunctions = fixture.native.Registry
	builtins.InformationSchemaTypes = fixture.native.Registry
	options.Bind.Builtin = builtins
	options.Bind.CatalogRelations = fixture.native.Registry
	options.Bind.Limits.SourceBytes = 1 << 20
	options.Normalize.Builtin = builtins
}

func (fixture *nativeRoutineFixture) prepareSQL(t *testing.T, sql string) *session.Prepared {
	t.Helper()
	return fixture.prepare(t, sql, nil, fixture.configurePlanning)
}

func (fixture *nativeRoutineFixture) invocation(t *testing.T) (session.PreparedExecution, *nativeInvocationQueries) {
	t.Helper()
	directory := t.TempDir()
	queries := &nativeInvocationQueries{}
	newQuery := func(ctx context.Context) (*runtime.Query, error) {
		query, err := runtime.NewQuery(ctx, fixture.pool, fixture.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
			WorkBytes: 8 << 20, OutputBytes: 2 << 20, SpillBytes: 8 << 20,
			Directory: directory, MaxFiles: 2, MaxReaders: 2,
		})
		if err != nil {
			return nil, err
		}
		queries.mu.Lock()
		queries.all = append(queries.all, query)
		queries.mu.Unlock()
		t.Cleanup(func() {
			if err := query.Close(); err != nil {
				t.Error(err)
			}
		})
		return query, nil
	}
	query, err := newQuery(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	queries.mu.Lock()
	queries.original = query
	queries.mu.Unlock()
	builtins := fixture.settings.BuiltinOptions(950, 950)
	builtins.InformationSchemaFunctions = fixture.native.Registry
	builtins.InformationSchemaTypes = fixture.native.Registry
	resolver := nativeRoutineBaseResolver{}
	planning := fixture.planning("")
	fixture.configurePlanning(&planning)
	planning.Bind.Limits.SourceBytes = 1 << 20
	modify := fixture.store.options
	modify.Transaction = nil
	modify.WorkBytes = 2 << 20
	modify.MaxSpoolBytes = 4 << 20
	modify.SortRunBytes = 1 << 20
	environment := execution.Environment{
		Query: query, Catalog: fixture.source, Keys: fixture.keys, Modify: modify, Builtin: builtins,
		Limits: execution.Limits{Expressions: expr.DefaultLimits(), BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 2 << 20},
	}
	environment.MutationTargets = execution.MutationTarget{Descriptor: modify.Descriptor, Expansion: modify.Expansion}
	typeRuntime := &session.DeclaredTypeRuntimeOptions{
		Descriptors: fixture.descriptorLimits,
		Expressions: session.ColumnExpressionOptions{
			Binding: planning.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			Builtin: builtins, Functions: resolver,
		},
		Routines: fixture.limits,
	}
	execution := session.PreparedExecution{
		Catalog:      fixture.catalog,
		Dependencies: append([]txn.ReadDependency(nil), fixture.dependencies...),
		Environment:  environment,
		TypeRuntime:  typeRuntime,
		RoutineDDL:   &session.RoutineCommandOptions{Compiler: procedural.Compiler{Limits: fixture.limits}},
		Routines: &session.RoutineExecutionOptions{
			Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, raw *zpgparse.RawStmt, parameters []model.TypeLayout) (session.PrepareOptions, error) {
				options := fixture.planning("")
				fixture.configurePlanning(&options)
				request := fixture.store.request(t, 1)
				options.Optimize.Access = mutationAccess{reader: reader, target: request.Target, layout: request.Layout, keys: fixture.store.options.KeyLayout}
				return options, ctx.Err()
			},
			NewQuery: newQuery,
			Limits:   fixture.limits,
			Capabilities: session.RoutineTransactionCapabilities{
				Sequence: fixture.sequences, CatalogRelations: fixture.native.Registry,
				BaseFunctions: resolver, BaseBuiltin: builtins, BaseKeys: fixture.keys, PathBytes: 4096,
			},
		},
	}
	return execution, queries
}

func (fixture *nativeRoutineFixture) installProcedure(t *testing.T, sql string) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	statement := parsed.Stmts[0].Stmt.GetCreateFunctionStmt()
	if statement == nil {
		t.Fatal("procedure source did not parse as CREATE PROCEDURE")
	}
	planning := fixture.planning(sql)
	fixture.configurePlanning(&planning)
	metadata := model.RoutineMetadata{
		Owner: 1, LanguageOID: 13563, Parallel: "unsafe", Cost: 100,
		Result:   model.TypeLayout{Type: model.Type{ID: 2278, Storage: model.TypeAny, Modifier: -1}},
		Language: "plpgsql", Volatility: "volatile", Procedure: true,
	}
	metadata, err = (procedural.Compiler{Limits: fixture.limits}).Compile(
		t.Context(), statement, metadata, mutationCatalog{fixture.sessionExecutionFixture.descriptors}, planning.Bind, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 3, OID: 10003, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectRoutine, Name: "commit_prefix"},
		Version: 1, State: model.StatePublic, Routine: &metadata,
	}, fixture.descriptorLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	err = fixture.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		dependencies, err := fixture.catalog.CreateDependencies(ctx, descriptor)
		if err != nil {
			return err
		}
		view, err := transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		change, err := fixture.catalog.Create(ctx, snapshot, descriptor)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := fixture.session.Commit(t.Context(), false)
	if err != nil || !committed.Committed {
		t.Fatalf("publish procedure = %+v, %v", committed, err)
	}
	fixture.sessionExecutionFixture.descriptors = append(fixture.sessionExecutionFixture.descriptors, descriptor)
	fixture.dependencies = append(fixture.dependencies, fixture.catalog.DescriptorDependency(descriptor.ID()))
}

func assertNativeInvocationReleased(t *testing.T, fixture *nativeRoutineFixture, queries *nativeInvocationQueries) {
	t.Helper()
	if fixture.session.Active() {
		t.Fatal("automatic routine invocation retained a transaction")
	}
	queries.mu.Lock()
	all := append([]*runtime.Query(nil), queries.all...)
	original := queries.original
	queries.mu.Unlock()
	if original == nil || len(all) == 0 {
		t.Fatal("routine invocation did not publish its query lifecycle")
	}
	for _, query := range all {
		usage := query.Usage()
		if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
			t.Fatalf("routine query retained resources: %+v", usage)
		}
		if query == original {
			if err := query.Context().Err(); err != nil {
				t.Fatalf("session closed caller-owned query: %v", err)
			}
		} else if err := query.Context().Err(); err == nil {
			t.Fatal("session retained a nested routine query")
		}
	}
}

func TestNativePreparedCallCommitsPrefixAndRollsBackReplacementTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	fixture := newNativeRoutineFixture(t)
	fixture.installProcedure(t, `CREATE PROCEDURE public.commit_prefix()
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO items VALUES (1, 10);
  COMMIT;
  INSERT INTO items VALUES (1, 99);
END
$$`)
	prepared := fixture.prepareSQL(t, `CALL public.commit_prefix()`)
	invocation, queries := fixture.invocation(t)
	_, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(context.Context, *session.Rows) error { return nil })
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "23505" {
		t.Fatalf("write after procedure COMMIT = %v, want SQLSTATE 23505", err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("committed procedure prefix = %d, %v", value, err)
	}
	assertNativeInvocationReleased(t, fixture, queries)

	followup := fixture.prepareSQL(t, `INSERT INTO items VALUES (2, 20)`)
	followupInvocation, followupQueries := fixture.invocation(t)
	result, err := fixture.session.RunPrepared(t.Context(), followup, followupInvocation, func(context.Context, *session.Rows) error { return nil })
	if err != nil || !result.Commit.Committed {
		t.Fatalf("session reuse after procedure failure = %+v, %v", result, err)
	}
	if value, err := fixture.committedValue(t, 2); err != nil || value != 20 {
		t.Fatalf("post-procedure statement = %d, %v", value, err)
	}
	assertNativeInvocationReleased(t, fixture, followupQueries)
}

func TestNativeInlineRoutineCommitsPrefixAndRollsBackReplacementTransaction(t *testing.T) {
	fixture := newNativeRoutineFixture(t)
	prepared := fixture.prepareSQL(t, `DO $$
BEGIN
  INSERT INTO items VALUES (1, 10);
  COMMIT;
  INSERT INTO items VALUES (1, 99);
END
$$`)
	invocation, queries := fixture.invocation(t)
	_, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(context.Context, *session.Rows) error { return nil })
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "23505" {
		t.Fatalf("write after DO COMMIT = %v, want SQLSTATE 23505", err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("committed DO prefix = %d, %v", value, err)
	}
	assertNativeInvocationReleased(t, fixture, queries)
}
