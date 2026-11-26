package modify_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	partitionSmokeQueryWorkBytes   = 64 << 20
	partitionSmokeQueryOutputBytes = 8 << 20
	partitionSmokeQuerySpillBytes  = 8 << 20
	partitionSmokeMaxRows          = 64
)

type partitionSmokeTopology struct {
	children uint32
	parents  int
	bound    bool
	fallback model.ObjectID
	oid      model.OID
}

func partitionSmokeInvocation(t *testing.T, fixture *sessionExecutionFixture, mutations execution.MutationTargets) (session.PreparedExecution, *runtime.Query) {
	t.Helper()
	query, err := runtime.NewQuery(t.Context(), fixture.pool, fixture.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes: partitionSmokeQueryWorkBytes,

		OutputBytes:     partitionSmokeQueryOutputBytes,
		MaxReservations: 1000,
		SpillMode:       runtime.SpillEnabled,
		SpillBytes:      partitionSmokeQuerySpillBytes,
		Directory:       t.TempDir(),
		MaxFiles:        4,
		MaxReaders:      4,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits := expr.DefaultLimits()
	limits.MaxRows = partitionSmokeMaxRows
	modify := fixture.store.options
	modify.Transaction = nil
	modify.Query = query
	modify.BatchRows = 8
	modify.BatchBytes = 64 << 10
	modify.MaxSpoolBytes = 2 << 20
	modify.MaxRows = partitionSmokeMaxRows
	modify.WorkBytes = 16 << 20
	modify.SortRunBytes = 64 << 10
	modify.SortMaxRuns = 4
	modify.SortMergeFanIn = 2
	environment := execution.Environment{
		Query:           query,
		Keys:            modify.Encoder,
		Modify:          modify,
		MutationTargets: mutations,
		Builtin:         fixture.settings.BuiltinOptions(950, 950),
		Limits: execution.Limits{
			Expressions:    limits,
			BatchRows:      8,
			BatchBytes:     64 << 10,
			ParameterBytes: 64 << 10,
			MetadataBytes:  1 << 20,
		},
		Apply: apply.Options{MaxRowBytes: 4096, WorkBytes: 2 << 20},
		Aggregate: aggregate.Options{
			MaxRecordBytes:       4096,
			GroupBytes:           512 << 10,
			WorkBytes:            2 << 20,
			PartitionMemoryBytes: 64 << 10,
			MaxPartitions:        4,
		},
		Sort:  externalsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 2 << 20, MaxRuns: 4, MergeFanIn: 2},
		Spool: rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 2 << 20, MaxRows: partitionSmokeMaxRows, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4},
		Join:  exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 2 << 20, PartitionBytes: 2 << 20, MaxPartitions: 2, MaxDepth: 2},
	}
	return session.PreparedExecution{Catalog: fixture.catalog, Environment: environment}, query
}

func partitionSmokeObjectOptions(fixture *sessionExecutionFixture, dependencies []txn.ReadDependency, sql string) session.ObjectCommandOptions {
	binding := fixture.planning(sql).Bind
	binding.Limits.SourceBytes = 1 << 20
	evaluation := expr.DefaultLimits()
	evaluation.MaxBytes = 128 << 10
	// Admission derives its retained-policy charge from the descriptor and
	// expression limits (two MetadataBytes copies plus one Evaluation.MaxBytes per
	// expression), so PolicyBytes must dominate that derivation. Keep the 8x
	// MetadataBytes ratio the other declaration fixtures use.
	metadataBytes := uint64(4 << 20)
	return session.ObjectCommandOptions{
		Binding: bind.CreateDeclarationOptions{
			Bind:        binding,
			Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64},
			Physical:    model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		},
		Dependencies:   dependencies,
		Columns:        session.ColumnExpressionOptions{Binding: binding.Limits, Evaluation: evaluation, InputBytes: 1 << 20, ResultBytes: 1 << 20, Builtin: fixture.settings.BuiltinOptions(950, 950)},
		Indexes:        session.IndexExpressionOptions{Binding: binding.Limits, Evaluation: evaluation, InputBytes: 1 << 20, ResultBytes: 1 << 20},
		Limits:         schema.IndexLimits{MetadataBytes: metadataBytes, EntryBytes: 1 << 20, MaxColumns: 64},
		PolicyBytes:    8 * metadataBytes,
		MaxDescriptors: 64,
	}
}

func partitionSmokeDDL(t *testing.T, fixture *sessionExecutionFixture, dependencies []txn.ReadDependency, sql string, population bool) (session.ExecutionResult, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	object := partitionSmokeObjectOptions(fixture, dependencies, sql)
	invocation := session.PreparedExecution{Catalog: fixture.catalog, ObjectDDL: &object}
	var query *runtime.Query
	var planning func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader) (session.PrepareOptions, error)
	if population {
		invocation, query = partitionSmokeInvocation(t, fixture, nil)
		invocation.ObjectDDL = &object
		// Deliberately leave invocation.Dependencies empty: partition population
		// must discover and protect catalog, descriptor, and physical data reads.
		planning = func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader) (session.PrepareOptions, error) {
			return fixture.planning(sql), nil
		}
	}
	result, runErr := fixture.session.RunStatement(t.Context(), parsed.Stmts[0], session.StatementExecution{Execution: invocation, Planning: planning}, nil)
	if query != nil {
		runErr = errors.Join(runErr, query.Close())
	}
	return result, runErr
}

func partitionSmokeSQL(t *testing.T, fixture *sessionExecutionFixture, facade *session.SequenceSession, dependencies []txn.ReadDependency, sql string, mutations execution.MutationTargets) ([][]int64, error) {
	t.Helper()
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(errors.Join(err, view.Close()))
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
	options := fixture.planning(sql)
	access, err := session.NewCatalogAccess(snapshot, view, fixture.store.options.Encoder, session.CatalogAccessOptions{
		MaxPaths:        8,
		MaxBytes:        4 << 20,
		CapabilityEpoch: 1,
		Builtin:         fixture.settings.BuiltinOptions(950, 950),
		IndexLimits:     schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
		IndexExpressions: session.IndexExpressionOptions{
			Binding:     options.Bind.Limits,
			Evaluation:  expr.DefaultLimits(),
			InputBytes:  1 << 20,
			ResultBytes: 1 << 20,
		},
	})
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
	invocation, query := partitionSmokeInvocation(t, fixture, mutations)
	invocation.Environment.Sequences = sequences
	invocation.Environment.Relations = relations
	derived, _, err := prepared.ReadDependencies(t.Context(), fixture.catalog, 4<<20)
	if err != nil {
		return nil, errors.Join(err, query.Close())
	}
	invocation.Dependencies = append(append([]txn.ReadDependency(nil), dependencies...), derived...)
	var rows [][]int64
	_, runErr := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, input *session.Rows) error {
		var err error
		rows, err = collectWritableRows(ctx, input)
		return err
	})
	return rows, errors.Join(runErr, query.Close())
}

func requirePartitionSmokeRows(t *testing.T, fixture *sessionExecutionFixture, facade *session.SequenceSession, dependencies []txn.ReadDependency, sql string, mutations execution.MutationTargets, want [][]int64) {
	t.Helper()
	got, err := partitionSmokeSQL(t, fixture, facade, dependencies, sql, mutations)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, %v; want %v", sql, got, err, want)
	}
}

func partitionSmokeTableTopology(t *testing.T, fixture *sessionExecutionFixture, name string) partitionSmokeTopology {
	t.Helper()
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(errors.Join(err, view.Close()))
	}
	resolved, err := snapshot.Resolve(t.Context(), model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: name})
	if err != nil {
		t.Fatal(errors.Join(err, snapshot.Close(), view.Close()))
	}
	table, ok := resolved.Descriptor.Table()
	if !ok {
		t.Fatal(errors.Join(model.ErrInvalidDescriptor, snapshot.Close(), view.Close()))
	}
	state := partitionSmokeTopology{children: table.InheritanceChildren, parents: len(table.Parents), bound: table.PartitionBound != nil, fallback: table.PartitionDefault, oid: resolved.Descriptor.OID()}
	if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPartitionPopulationSmoke(t *testing.T) {
	fixture, _, _, facade, dependencies := sequenceDDLFixture(t)
	// This fixture provisions one-megabyte index entries, so relational expansion
	// reserves a workspace ceiling larger than the default 4MB work_mem. Raise the
	// session limit so the invocation is not rejected for a ceiling it never uses.
	requireSettingsSQL(t, fixture, `SET work_mem = '64MB'`)
	create := func(sql string) session.TableCommandResult {
		t.Helper()
		result, err := createIdentityTable(t, fixture, dependencies, sql)
		if err != nil {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && diagnostic.Detail != "" {
				t.Fatalf("%s: %v\ndetail: %s\nposition: %d", sql, err, diagnostic.Detail, diagnostic.Position)
			}
			t.Fatalf("%s: %v", sql, err)
		}
		return result
	}

	create(`CREATE TABLE smoke_events(id bigint CONSTRAINT smoke_events_pkey PRIMARY KEY, value bigint) PARTITION BY RANGE(id)`)
	create(`CREATE TABLE smoke_events_low PARTITION OF smoke_events FOR VALUES FROM (0) TO (10)`)
	fallback := create(`CREATE TABLE smoke_events_default PARTITION OF smoke_events DEFAULT`)
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `INSERT INTO smoke_events VALUES (5,50),(15,150),(25,250) RETURNING id`, fallback.Mutations, [][]int64{{5}, {15}, {25}})

	candidate := create(`CREATE TABLE smoke_events_candidate(id bigint PRIMARY KEY, value bigint)`)
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `INSERT INTO smoke_events_candidate VALUES (18,180) RETURNING id`, candidate.Mutations, [][]int64{{18}})

	_, err := partitionSmokeDDL(t, fixture, dependencies, `ALTER TABLE smoke_events ATTACH PARTITION smoke_events_candidate FOR VALUES FROM (10) TO (20)`, true)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "23514" {
		t.Fatalf("conflicting default partition attach = %v, want SQLSTATE 23514", err)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events"); got.children != 2 || got.fallback == 0 {
		t.Fatalf("failed attach changed root topology: %+v", got)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events_candidate"); got.parents != 0 || got.bound {
		t.Fatalf("failed attach changed candidate topology: %+v", got)
	}
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id FROM smoke_events ORDER BY id`, nil, [][]int64{{5}, {15}, {25}})
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id FROM smoke_events_candidate ORDER BY id`, nil, [][]int64{{18}})

	requirePartitionSmokeRows(t, fixture, facade, dependencies, `DELETE FROM smoke_events_default WHERE id = 15 RETURNING id`, fallback.Mutations, [][]int64{{15}})
	if _, err := partitionSmokeDDL(t, fixture, dependencies, `ALTER TABLE smoke_events ATTACH PARTITION smoke_events_candidate FOR VALUES FROM (10) TO (20)`, true); err != nil {
		t.Fatalf("attach candidate: %v", err)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events"); got.children != 3 || got.fallback == 0 {
		t.Fatalf("successful attach root topology: %+v", got)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events_candidate"); got.parents != 1 || !got.bound {
		t.Fatalf("successful attach candidate topology: %+v", got)
	}
	lowOID := partitionSmokeTableTopology(t, fixture, "smoke_events_low").oid
	candidateOID := partitionSmokeTableTopology(t, fixture, "smoke_events_candidate").oid
	defaultOID := partitionSmokeTableTopology(t, fixture, "smoke_events_default").oid
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id, tableoid FROM smoke_events ORDER BY id`, nil, [][]int64{{5, int64(lowOID)}, {18, int64(candidateOID)}, {25, int64(defaultOID)}})

	if _, err := partitionSmokeDDL(t, fixture, dependencies, `CREATE TABLE smoke_events_mid PARTITION OF smoke_events FOR VALUES FROM (30) TO (40)`, true); err != nil {
		t.Fatalf("create partition with existing default: %v", err)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events"); got.children != 4 || got.fallback == 0 {
		t.Fatalf("partition create root topology: %+v", got)
	}

	if _, err := partitionSmokeDDL(t, fixture, dependencies, `ALTER TABLE smoke_events DETACH PARTITION smoke_events_default`, false); err != nil {
		t.Fatalf("detach default: %v", err)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events"); got.children != 3 || got.fallback != 0 {
		t.Fatalf("default detach root topology: %+v", got)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events_default"); got.parents != 0 || got.bound {
		t.Fatalf("detached default topology: %+v", got)
	}
	high := create(`CREATE TABLE smoke_events_high PARTITION OF smoke_events FOR VALUES FROM (20) TO (30)`)
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `INSERT INTO smoke_events VALUES (12,120),(25,251) RETURNING id`, high.Mutations, [][]int64{{12}, {25}})
	highOID := partitionSmokeTableTopology(t, fixture, "smoke_events_high").oid
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id, tableoid FROM smoke_events ORDER BY id`, nil, [][]int64{{5, int64(lowOID)}, {12, int64(candidateOID)}, {18, int64(candidateOID)}, {25, int64(highOID)}})

	if _, err := partitionSmokeDDL(t, fixture, dependencies, `ALTER TABLE smoke_events DETACH PARTITION smoke_events_candidate`, false); err != nil {
		t.Fatalf("detach candidate: %v", err)
	}
	extra := create(`CREATE TABLE smoke_events_extra PARTITION OF smoke_events FOR VALUES FROM (40) TO (50)`)
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `INSERT INTO smoke_events VALUES (45,450) RETURNING id`, extra.Mutations, [][]int64{{45}})
	extraOID := partitionSmokeTableTopology(t, fixture, "smoke_events_extra").oid
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id, tableoid FROM smoke_events ORDER BY id`, nil, [][]int64{{5, int64(lowOID)}, {25, int64(highOID)}, {45, int64(extraOID)}})
	requirePartitionSmokeRows(t, fixture, facade, dependencies, `SELECT id, tableoid FROM smoke_events_candidate ORDER BY id`, nil, [][]int64{{12, int64(candidateOID)}, {18, int64(candidateOID)}})
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events_candidate"); got.parents != 0 || got.bound {
		t.Fatalf("detached candidate topology: %+v", got)
	}
	if got := partitionSmokeTableTopology(t, fixture, "smoke_events"); got.children != 4 || got.fallback != 0 {
		t.Fatalf("final root topology: %+v", got)
	}
}

func TestPartitionPopulationAfterInlineCommitSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	fixture := newNativeRoutineFixture(t)
	var dependencies []txn.ReadDependency
	for _, layout := range fixture.catalog.Layouts() {
		dependencies = append(dependencies, txn.ReadDependency{
			Target:    layout.Target,
			KeyLayout: []storage.KeyColumn{{ColumnID: layout.Columns[0].ID, Type: layout.Columns[0].Type}},
			Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		})
	}
	root, err := createIdentityTable(t, fixture.sessionExecutionFixture, dependencies, `CREATE TABLE smoke_inline_root(id bigint, value bigint) PARTITION BY RANGE(id)`)
	if err != nil {
		t.Fatalf("create inline root: %v", err)
	}
	fallback, err := createIdentityTable(t, fixture.sessionExecutionFixture, dependencies, `CREATE TABLE smoke_inline_default PARTITION OF smoke_inline_root DEFAULT`)
	if err != nil {
		t.Fatalf("create inline default: %v", err)
	}
	dependencies = append(dependencies,
		fixture.catalog.DescriptorDependency(root.Descriptor.ID()),
		fixture.catalog.DescriptorDependency(fallback.Descriptor.ID()),
	)

	partitionSQL := `CREATE TABLE smoke_inline_child PARTITION OF smoke_inline_root FOR VALUES FROM (0) TO (10)`
	doSQL := `DO $$
BEGIN
  INSERT INTO items VALUES (77, 770);
  COMMIT;
  CREATE TABLE smoke_inline_child PARTITION OF smoke_inline_root FOR VALUES FROM (0) TO (10);
END
$$`
	prepared := fixture.prepareSQL(t, doSQL)
	invocation, queries := fixture.invocation(t)
	object := partitionSmokeObjectOptions(fixture.sessionExecutionFixture, dependencies, partitionSQL)
	nativePlanning := fixture.planning(partitionSQL)
	fixture.configurePlanning(&nativePlanning)
	object.Binding.Bind = nativePlanning.Bind
	object.Columns.Binding = nativePlanning.Bind.Limits
	object.Columns.Builtin = nativePlanning.Bind.Builtin
	object.Indexes.Binding = nativePlanning.Bind.Limits
	invocation.ObjectDDL = &object
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(context.Context, *session.Rows) error { return nil })
	if err != nil || !result.Commit.Committed {
		assertNativeInvocationReleased(t, fixture, queries)
		t.Fatalf("inline partition create after COMMIT = %+v, %v", result, err)
	}
	assertNativeInvocationReleased(t, fixture, queries)
	if value, err := fixture.committedValue(t, 77); err != nil || value != 770 {
		t.Fatalf("inline committed prefix = %d, %v", value, err)
	}
	if got := partitionSmokeTableTopology(t, fixture.sessionExecutionFixture, "smoke_inline_root"); got.children != 2 || got.fallback == 0 {
		t.Fatalf("inline partition root topology: %+v", got)
	}
	if got := partitionSmokeTableTopology(t, fixture.sessionExecutionFixture, "smoke_inline_child"); got.parents != 1 || !got.bound {
		t.Fatalf("inline partition child topology: %+v", got)
	}
}
