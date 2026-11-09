package modify_test

// Tests coverage for SQL-defined aggregates through ordinary top-level statements,
// exercising native catalog bootstrap, procedural compiler,
// CREATE FUNCTION / CREATE AGGREGATE DDL, and SELECT execution.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	exwindow "github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// zzConsumerStopped stands in for a caller that stops reading a result set
// after its first batch.
var zzConsumerStopped = errors.New("smoke consumer stopped early")

// zzNullResult reports a NULL where this file requires a concrete integer, so a
// missing value can never be mistaken for zero.
var zzNullResult = errors.New("result column is NULL")

// zzSupportFunctions and zzAggregates are the declarations under test. Every
// transition is a real SQL-language routine compiled by the procedural
// compiler, so each aggregate runs a user-written body rather than a builtin.
var (
	zzSupportFunctions = []string{
		// The ordinary accumulate shape: state plus input.
		`CREATE FUNCTION public.zz_sum_state(bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1 + $2'`,
		// Final function: doubles the accumulated state.
		`CREATE FUNCTION public.zz_sum_final(bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1 * 2'`,
		// Order-sensitive recurrence f(s,x) = 3s + x, so a different input order
		// produces a different result and ORDER BY is observable rather than
		// merely accepted.
		`CREATE FUNCTION public.zz_ordered_state(bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1 * 3 + $2'`,
		// Non-strict counter that ignores its argument, so it sees every row
		// including those carrying a NULL.
		`CREATE FUNCTION public.zz_count_state(bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'`,
		// Strict counter: the engine must skip rows whose argument is NULL.
		`CREATE FUNCTION public.zz_count_strict_state(bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE STRICT AS 'SELECT $1 + 1'`,
		// Ten parameters, so the state plus nine aggregated arguments cross the
		// historical eight-argument boundary in the call plumbing.
		`CREATE FUNCTION public.zz_wide_state(bigint, bigint, bigint, bigint, bigint,
			bigint, bigint, bigint, bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1+$2+$3+$4+$5+$6+$7+$8+$9+$10'`,
		// Divides by zero when the input value is 10. The divisor depends on the
		// row, so this is a genuine execution error rather than something the
		// routine compiler can reject while it plans the body.
		`CREATE FUNCTION public.zz_fail_state(bigint, bigint) RETURNS bigint
		 LANGUAGE sql IMMUTABLE AS 'SELECT $1 + $2 / ($2 - 10)'`,
	}
	zzAggregates = []string{
		`CREATE AGGREGATE public.zz_sum(bigint) (
			sfunc = zz_sum_state, stype = bigint, initcond = '0')`,
		`CREATE AGGREGATE public.zz_scaled(bigint) (
			sfunc = zz_sum_state, stype = bigint, initcond = '0', finalfunc = zz_sum_final)`,
		`CREATE AGGREGATE public.zz_ordered(bigint) (
			sfunc = zz_ordered_state, stype = bigint, initcond = '0')`,
		`CREATE AGGREGATE public.zz_rows(bigint) (
			sfunc = zz_count_state, stype = bigint, initcond = '0')`,
		`CREATE AGGREGATE public.zz_rows_strict(bigint) (
			sfunc = zz_count_strict_state, stype = bigint, initcond = '0')`,
		`CREATE AGGREGATE public.zz_wide(bigint, bigint, bigint, bigint, bigint,
			bigint, bigint, bigint, bigint) (
			sfunc = zz_wide_state, stype = bigint, initcond = '0')`,
		`CREATE AGGREGATE public.zz_fail(bigint) (
			sfunc = zz_fail_state, stype = bigint, initcond = '0')`,
	}
)

// zzParseOne parses exactly one statement.
func zzParseOne(t *testing.T, sql string) *zpgparse.RawStmt {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	if len(parsed.Stmts) != 1 {
		t.Fatalf("parse %q: got %d statements", sql, len(parsed.Stmts))
	}
	return parsed.Stmts[0]
}

// zzSQLState extracts the SQLSTATE of a statement failure.
func zzSQLState(err error) string {
	var state *model.Error
	if errors.As(err, &state) {
		return state.Code
	}
	return ""
}

// zzBinding returns the declaration bind options the native fixture uses, with
// the native catalog relations already wired into the search path.
func (fixture *nativeRoutineFixture) zzBinding() bind.Options {
	planning := fixture.planning("")
	fixture.configurePlanning(&planning)
	binding := planning.Bind
	binding.Limits.SourceBytes = 1 << 20
	return binding
}

// zzDependencies covers the whole catalog plus the fixture table, so a snapshot
// resolves both the declarations this file installs and the rows it reads.
func (fixture *nativeRoutineFixture) zzDependencies(t *testing.T) []txn.ReadDependency {
	t.Helper()
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	result := make([]txn.ReadDependency, 0, len(fixture.catalog.Layouts())+1)
	for _, layout := range fixture.catalog.Layouts() {
		result = append(result, txn.ReadDependency{Target: layout.Target, KeyLayout: fixture.catalog.KeyLayout(), Spans: spans})
	}
	request := fixture.store.request(t, 1)
	return append(result, txn.ReadDependency{Target: request.Target, KeyLayout: fixture.store.options.KeyLayout, Spans: spans})
}

// zzInstallDeclarations publishes every support function and aggregate through
// the ordinary object DDL, exactly as a client would issue them.
func zzInstallDeclarations(t *testing.T, fixture *nativeRoutineFixture) {
	t.Helper()
	// The session clamps every aggregate workspace to work_mem, so the default
	// 4MB would reduce this file's declared 16MB aggregate workspace to a budget
	// that cannot admit the group workspace plus the DISTINCT/ORDER BY sort. The
	// clamp is intended product behaviour; the declared shape is simply larger
	// than the default, so the fixture raises the session limit, exactly as
	// zz_partition_population_smoke_test.go does for one-megabyte index entries.
	requireSettingsSQL(t, fixture.sessionExecutionFixture, `SET work_mem = '64MB'`)
	for _, sql := range zzSupportFunctions {
		fixture.zzCreateFunction(t, sql)
	}
	for _, sql := range zzAggregates {
		fixture.zzDefinition(t, sql)
	}
}

// zzSeedRows loads the dataset. The fourth row carries a NULL so a strict
// transition has something to skip. The affected count is asserted so the whole
// suite cannot pass against an empty table.
func zzSeedRows(t *testing.T, fixture *nativeRoutineFixture) {
	t.Helper()
	result, _, err := fixture.zzRun(t, `INSERT INTO items VALUES (1,10),(2,10),(3,30),(4,NULL)`, nil)
	if err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	if result.RowsAffected != 4 {
		t.Fatalf("seed rows affected = %d, want 4", result.RowsAffected)
	}
}

// zzCreateFunction publishes one CREATE FUNCTION through the real routine DDL.
func (fixture *nativeRoutineFixture) zzCreateFunction(t *testing.T, sql string) {
	t.Helper()
	statement := zzParseOne(t, sql).Stmt.GetCreateFunctionStmt()
	if statement == nil {
		t.Fatalf("not a CREATE FUNCTION: %s", sql)
	}
	options := session.RoutineCommandOptions{
		TableCommandOptions: session.TableCommandOptions{
			Binding: bind.CreateDeclarationOptions{
				Bind:        fixture.zzBinding(),
				Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64},
				Owner:       (model.Principal{}).OID(),
			},
			Dependencies: fixture.zzDependencies(t),
			// StageDeclaration admits the routine descriptor through the schema
			// index policy and the column/index expression compilers, which
			// require explicit metadata, entry, and expression budgets.
			Columns: session.ColumnExpressionOptions{
				Binding: fixture.zzBinding().Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			},
			Indexes: session.IndexExpressionOptions{
				Binding: fixture.zzBinding().Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
			},
			Limits:      schema.IndexLimits{MetadataBytes: 1 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
			PolicyBytes: 8 << 20,
		},
		Compiler:       procedural.Compiler{Limits: fixture.limits},
		MaxDescriptors: 64,
	}
	if _, err := session.ExecuteCreateRoutine(t.Context(), fixture.session, statement, options); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// zzDefinition publishes one cast, operator, or aggregate declaration through
// the shared SQL-defined definition path.
func (fixture *nativeRoutineFixture) zzDefinition(t *testing.T, sql string) {
	t.Helper()
	options := session.ObjectCommandOptions{
		Binding: bind.CreateDeclarationOptions{
			Bind:        fixture.zzBinding(),
			Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64},
			Owner:       (model.Principal{}).OID(),
		},
		Dependencies:   fixture.zzDependencies(t),
		MaxDescriptors: 64,
		PolicyBytes:    8 << 20,
	}
	if _, err := session.ExecuteDefinitionCommand(t.Context(), fixture.session, zzParseOne(t, sql).Stmt, options); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// zzInvocation returns the fully configured top-level invocation. Routine
// execution options come from the fixture, so the capability frame installs the
// aggregate resolver; the aggregate, sort, and window workspaces are supplied
// here because the fixture leaves them to the caller.
func (fixture *nativeRoutineFixture) zzInvocation(t *testing.T) (session.PreparedExecution, *nativeInvocationQueries) {
	t.Helper()
	invocation, queries := fixture.invocation(t)
	invocation.Environment.Aggregate = exaggregate.Options{
		MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20,
		PartitionMemoryBytes: 64 << 10, MaxPartitions: 4,
	}
	invocation.Environment.Sort = externalsort.Options{
		MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2,
	}
	invocation.Environment.Window = exwindow.Options{
		MaxRecordBytes: 4096, PartitionMemoryBytes: 64 << 10,
		MaxPartitionBytes: 1 << 20, MaxPartitionRows: 1024, WorkBytes: 16 << 20,
	}
	return invocation, queries
}

// zzRun prepares and runs one statement on the ordinary top-level path with the
// fully configured invocation, then asserts the statement returned every
// resource. A nil consumer drains the result to EOF, so a statement that fails
// only while producing rows is still executed to completion.
func (fixture *nativeRoutineFixture) zzRun(t *testing.T, sql string, consumer func(context.Context, *session.Rows) error) (session.ExecutionResult, *nativeInvocationQueries, error) {
	t.Helper()
	var view txn.ReadView
	var err error
	if fixture.session.Active() {
		view, err = fixture.session.ReadView(t.Context(), fixture.zzDependencies(t))
	} else {
		view, err = fixture.store.manager.CurrentReadView(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
			t.Error(err)
		}
	}()
	planning := fixture.planning(sql)
	fixture.configurePlanning(&planning)
	access, err := session.NewCatalogAccess(snapshot, view, fixture.keys, session.CatalogAccessOptions{
		MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1,
		IndexLimits:      schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
		IndexExpressions: session.IndexExpressionOptions{Binding: planning.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20},
	})
	if err != nil {
		return session.ExecutionResult{}, nil, err
	}
	planning.Optimize.Access = access
	prepared, err := session.Prepare(t.Context(), zzParseOne(t, sql), snapshot, planning, nil)
	if err != nil {
		return session.ExecutionResult{}, nil, err
	}
	invocation, queries := fixture.zzInvocation(t)
	derived, _, err := prepared.ReadDependencies(t.Context(), fixture.catalog, 4<<20)
	if err != nil {
		return session.ExecutionResult{}, nil, err
	}
	invocation.Dependencies = append(fixture.zzDependencies(t), derived...)
	if consumer == nil {
		consumer = zzDrainRows
	}
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, consumer)
	zzRequireReleased(t, queries)
	return result, queries, err
}

// zzDrainRows reads a result set to EOF without keeping it, so a lazily
// produced result still executes completely.
func zzDrainRows(ctx context.Context, rows *session.Rows) error {
	var batch columnar.Batch
	for {
		err := rows.Next(ctx, &batch)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// zzCollectInt64 reads every result row as a single non-NULL integer. A NULL is
// reported as an error rather than silently compared as zero, so an aggregate
// that wrongly produces NULL cannot satisfy an expected 0.
func zzCollectInt64(ctx context.Context, rows *session.Rows) ([][]int64, error) {
	var collected [][]int64
	var batch columnar.Batch
	for {
		err := rows.Next(ctx, &batch)
		if err == io.EOF {
			return collected, nil
		}
		if err != nil {
			return nil, err
		}
		for lane := range batch.Rows() {
			values := make([]int64, len(batch.Columns))
			for i := range batch.Columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(lane), 4096)
				if err != nil {
					return nil, err
				}
				if value.IsNull {
					return nil, zzNullResult
				}
				values[i] = value.IntVal
			}
			collected = append(collected, values)
		}
	}
}

// zzQuery runs one statement and returns the first column of every non-NULL
// result row.
func (fixture *nativeRoutineFixture) zzQuery(t *testing.T, sql string) ([][]int64, error) {
	t.Helper()
	var collected [][]int64
	_, _, err := fixture.zzRun(t, sql, func(ctx context.Context, rows *session.Rows) error {
		var collectErr error
		collected, collectErr = zzCollectInt64(ctx, rows)
		return collectErr
	})
	return collected, err
}

// zzRequireReleased asserts every query the statement opened returned its work,
// output, spill, files, and readers.
func zzRequireReleased(t *testing.T, queries *nativeInvocationQueries) {
	t.Helper()
	if queries == nil {
		return
	}
	queries.mu.Lock()
	all := append([]*runtime.Query(nil), queries.all...)
	queries.mu.Unlock()
	if len(all) == 0 {
		t.Fatal("statement did not publish a query lifecycle")
	}
	for _, query := range all {
		usage := query.Usage()
		if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
			t.Fatalf("statement query retained resources: %+v", usage)
		}
	}
}

// zzRequireRows runs one statement and asserts its exact result rows.
func zzRequireRows(t *testing.T, fixture *nativeRoutineFixture, sql string, want [][]int64) {
	t.Helper()
	got, err := fixture.zzQuery(t, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", sql, got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d has %d columns, want %d", sql, i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("%s: got %v, want %v", sql, got, want)
			}
		}
	}
}

// zzRequireState asserts a statement failed with one exact SQLSTATE.
func zzRequireState(t *testing.T, err error, want string, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: statement succeeded, want SQLSTATE %s", what, want)
	}
	if got := zzSQLState(err); got != want {
		t.Fatalf("%s: SQLSTATE %s (%v), want %s", what, got, err, want)
	}
}

// TestZZUserAggregateOrdinaryExecution drives declared aggregates across the
// ordinary grouping path: plain accumulation, grouping, FILTER, DISTINCT,
// observable ORDER BY, empty input, strict versus non-strict transitions, final
// function application, and nine aggregated arguments.
func TestZZUserAggregateOrdinaryExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	fixture := newNullableNativeRoutineFixture(t)
	zzInstallDeclarations(t, fixture)
	zzSeedRows(t, fixture)

	for _, scenario := range []struct {
		name string
		sql  string
		want [][]int64
	}{
		{"global sum", `SELECT zz_sum(value) FROM items WHERE id <= 3`, [][]int64{{50}}},
		// Grouping keeps each group's state independent.
		{"grouped sum", `SELECT zz_sum(value) FROM items WHERE id <= 3 GROUP BY id ORDER BY id`, [][]int64{{10}, {10}, {30}}},
		// FILTER drops rows before they reach the transition.
		{"filtered sum", `SELECT zz_sum(value) FILTER (WHERE id > 1) FROM items WHERE id <= 3`, [][]int64{{40}}},
		// DISTINCT removes the duplicate 10 before folding, so this sum is
		// order-independent and can only be 40 when the duplicate was dropped.
		{"distinct sum", `SELECT zz_sum(DISTINCT value) FROM items WHERE id <= 3`, [][]int64{{40}}},
		// The recurrence is order-sensitive, so these two prove ORDER BY reaches
		// the transition instead of being accepted and ignored.
		{"ascending order", `SELECT zz_ordered(value ORDER BY value) FROM items WHERE id <= 3`, [][]int64{{150}}},
		{"descending order", `SELECT zz_ordered(value ORDER BY value DESC) FROM items WHERE id <= 3`, [][]int64{{310}}},
		// An empty input still publishes one global group holding INITCOND. The
		// collector rejects NULL, so 0 must be a real transition result.
		{"empty input", `SELECT zz_sum(value) FROM items WHERE id > 1000`, [][]int64{{0}}},
		// The non-strict transition sees the NULL row; the strict one skips it.
		{"non-strict sees null", `SELECT zz_rows(value) FROM items`, [][]int64{{4}}},
		{"strict skips null", `SELECT zz_rows_strict(value) FROM items`, [][]int64{{3}}},
		// FINALFUNC transforms the finished state.
		{"final function", `SELECT zz_scaled(value) FROM items WHERE id <= 3`, [][]int64{{100}}},
		// State plus nine aggregated arguments crosses the eight-argument
		// boundary in the compiled call plumbing.
		{"nine arguments", `SELECT zz_wide(value,value,value,value,value,value,value,value,value)
			FROM items WHERE id <= 3`, [][]int64{{450}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			zzRequireRows(t, fixture, scenario.sql, scenario.want)
		})
	}
	if fixture.session.Active() {
		t.Fatal("autocommit statements retained a transaction")
	}
}

// TestZZUserAggregateWindowExecution drives a declared aggregate through the
// window path, where every frame is replayed from its start.
func TestZZUserAggregateWindowExecution(t *testing.T) {
	fixture := newNullableNativeRoutineFixture(t)
	zzInstallDeclarations(t, fixture)
	zzSeedRows(t, fixture)

	// A growing default frame yields the running total.
	zzRequireRows(t, fixture,
		`SELECT zz_sum(value) OVER (ORDER BY id) FROM items WHERE id <= 3`,
		[][]int64{{10}, {20}, {50}})
	// A partition per id keeps each partition's state separate.
	zzRequireRows(t, fixture,
		`SELECT zz_sum(value) OVER (PARTITION BY id ORDER BY id) FROM items WHERE id <= 3 ORDER BY id`,
		[][]int64{{10}, {10}, {30}})
}

// TestZZUserAggregateCleanupOnFailureAndEarlyClose proves the declared
// aggregate path releases its resources whether a statement fails inside the
// transition or its consumer stops reading, and that the session survives both.
func TestZZUserAggregateCleanupOnFailureAndEarlyClose(t *testing.T) {
	fixture := newNullableNativeRoutineFixture(t)
	zzInstallDeclarations(t, fixture)
	zzSeedRows(t, fixture)

	// The transition divides by zero once it reaches the input value 10. The
	// exact SQLSTATE is required, so any other failure is a real defect.
	_, _, err := fixture.zzRun(t, `SELECT zz_fail(value) FROM items`, nil)
	zzRequireState(t, err, "22012", "failing transition")
	if fixture.session.Active() {
		t.Fatal("failed statement retained a transaction")
	}
	// zzRun already asserted resource release on that failure path.
	zzRequireRows(t, fixture, `SELECT zz_sum(value) FROM items WHERE id <= 3`, [][]int64{{50}})

	// A consumer that stops after successfully reading its first batch must
	// still release everything.
	_, _, err = fixture.zzRun(t, `SELECT zz_sum(value) OVER (ORDER BY id) FROM items WHERE id <= 3`,
		func(ctx context.Context, rows *session.Rows) error {
			var batch columnar.Batch
			if err := rows.Next(ctx, &batch); err != nil {
				return err
			}
			if batch.Rows() == 0 {
				return errors.New("window statement produced no first batch")
			}
			return zzConsumerStopped
		})
	if !errors.Is(err, zzConsumerStopped) {
		t.Fatalf("early close error = %v, want %v", err, zzConsumerStopped)
	}
	if fixture.session.Active() {
		t.Fatal("early-closed statement retained a transaction")
	}
	// The session is reusable after an early close.
	zzRequireRows(t, fixture, `SELECT zz_sum(value) FROM items WHERE id <= 3`, [][]int64{{50}})
}

// TestZZUserAggregateLifecyclePersistsAndRollsBack proves the declarations are
// real catalog objects: one staged inside an explicit transaction is visible
// there and disappears with the rollback, while committed declarations survive.
func TestZZUserAggregateLifecyclePersistsAndRollsBack(t *testing.T) {
	fixture := newNullableNativeRoutineFixture(t)
	zzInstallDeclarations(t, fixture)
	zzSeedRows(t, fixture)
	zzRequireRows(t, fixture, `SELECT zz_sum(value) FROM items WHERE id <= 3`, [][]int64{{50}})

	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	fixture.zzDefinition(t, `CREATE AGGREGATE public.zz_rolled_back(bigint) (
		sfunc = zz_sum_state, stype = bigint, initcond = '0')`)
	// The declaration executes inside its own transaction, so this proves the
	// staged descriptor is fully usable before it is discarded.
	got, err := fixture.zzQuery(t, `SELECT zz_rolled_back(value) FROM items WHERE id <= 3`)
	if err != nil || len(got) != 1 || len(got[0]) != 1 || got[0][0] != 50 {
		t.Fatalf("declaration in its own transaction = %v, %v", got, err)
	}
	if err := fixture.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	// After the rollback the aggregate is gone, so resolution reports the exact
	// missing-function state rather than any error.
	_, err = fixture.zzQuery(t, `SELECT zz_rolled_back(value) FROM items WHERE id <= 3`)
	zzRequireState(t, err, "42883", "rolled-back aggregate")
	// The committed declarations are unaffected by the rollback.
	zzRequireRows(t, fixture, `SELECT zz_sum(value) FROM items WHERE id <= 3`, [][]int64{{50}})
}
