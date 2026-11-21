package supersql

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
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
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// column describes one result column of an executed statement for in-package callers.
type column struct {
	Name     string
	Type     model.Type
	Layout   model.TypeLayout
	Nullable bool
}

// outcome reports one executed statement for in-package callers. Rows carries
// at most the requested row limit.
type outcome struct {
	Tag          string
	RowsAffected uint64
	Committed    bool
	Columns      []column
	Rows         [][]model.Value

	// builtins are the resolve options the producing statement planned with;
	// value rendering resolves declared-typed columns through them.
	builtins builtin.ResolveOptions
}

// exec runs every statement in sql through the embedded session and reports the
// last statement's outcome. Result rows are materialized up to maxRows; maxRows
// <= 0 accepts every produced row.
func (database *Catalog) exec(ctx context.Context, sql string, maxRows int) (outcome, error) {
	if database == nil {
		return outcome{}, ErrDatabaseClosed
	}
	return database.execIn(ctx, database.embeddedScope(), sql, maxRows)
}

// execIn runs every statement in sql through scope and reports the last
// statement's outcome.
func (database *Catalog) execIn(ctx context.Context, scope *sessionScope, sql string, maxRows int) (outcome, error) {
	if err := database.checkOpen(); err != nil {
		return outcome{}, err
	}
	parsed, err := scope.txns.Parse(ctx, sql, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return outcome{}, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return outcome{}, ErrMissingStatement
	}
	var out outcome
	for index := range parsed.ParseResult.Stmts {
		if out, err = database.execStatement(ctx, scope, parsed, index, nil, maxRows, nil, nil); err != nil {
			return out, err
		}
	}
	return out, nil
}

// CopyFrom executes one COPY ... FROM STDIN statement, reading records from
// source until EOF. The caller keeps ownership of source; the engine never
// closes it. Server-side COPY FROM 'file' and FROM PROGRAM are excluded
// features and fail with SQLSTATE 0A000.
func (database *Catalog) CopyFrom(ctx context.Context, statement string, source io.Reader, options CopyOptions) (CopyResult, error) {
	streams := &qcopy.Streams{Reader: source, MaxRecordBytes: options.MaxRecordBytes, Report: copyReport(options.Report)}
	out, err := database.copyStream(ctx, statement, streams)
	return CopyResult{Tag: out.Tag, RowsAffected: out.RowsAffected}, wrapError(err)
}

// CopyTo executes one COPY ... TO STDOUT statement, writing records to sink.
// The caller keeps ownership of sink; the engine never closes it. Server-side
// COPY TO 'file' and TO PROGRAM are excluded features and fail with SQLSTATE
// 0A000.
func (database *Catalog) CopyTo(ctx context.Context, statement string, sink io.Writer, options CopyOptions) (CopyResult, error) {
	streams := &qcopy.Streams{Writer: sink, MaxRecordBytes: options.MaxRecordBytes, Report: copyReport(options.Report)}
	out, err := database.copyStream(ctx, statement, streams)
	return CopyResult{Tag: out.Tag, RowsAffected: out.RowsAffected}, wrapError(err)
}

func copyReport(report func(context.Context, CopyDiagnostic)) func(context.Context, qcopy.Diagnostic) {
	if report == nil {
		return nil
	}
	return func(ctx context.Context, diagnostic qcopy.Diagnostic) {
		report(ctx, CopyDiagnostic{Row: diagnostic.Row, Column: diagnostic.Column, Err: wrapError(diagnostic.Err)})
	}
}

// copyStream runs a single-statement COPY through the embedded session with
// the caller's stream attached.
func (database *Catalog) copyStream(ctx context.Context, statement string, streams *qcopy.Streams) (outcome, error) {
	if database == nil {
		return outcome{}, ErrDatabaseClosed
	}
	return database.copyIn(ctx, database.embeddedScope(), statement, streams)
}

// copyIn runs a single-statement COPY through scope. The statement's direction
// decides which stream field validation accepts; a non-COPY or wrongly directed
// statement fails with a typed error instead of reaching execution.
func (database *Catalog) copyIn(ctx context.Context, scope *sessionScope, statement string, streams *qcopy.Streams) (outcome, error) {
	if err := database.checkOpen(); err != nil {
		return outcome{}, err
	}
	parsed, err := scope.txns.Parse(ctx, statement, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return outcome{}, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return outcome{}, &model.Error{Code: "22023", Message: "COPY accepts exactly one statement"}
	}
	return database.execStatement(ctx, scope, parsed, 0, nil, 0, nil, streams)
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

// isSessionDDLStatement reports statements that may alter session-local
// temporary objects or catalog definitions, bumping scope.ddlSeq so prepared
// statement plan caches invalidate immediately.
func isSessionDDLStatement(raw *zpgparse.RawStmt) bool {
	if raw == nil || raw.Stmt == nil {
		return false
	}
	node := raw.Stmt
	if node.GetSelectStmt() != nil || node.GetInsertStmt() != nil || node.GetUpdateStmt() != nil || node.GetDeleteStmt() != nil || node.GetMergeStmt() != nil {
		return false
	}
	if node.GetTransactionStmt() != nil || node.GetVariableShowStmt() != nil {
		return false
	}
	return true
}

// execStatement resolves one parsed statement's extension sidecars and then
// routes it: transaction-level temporal clauses to the transaction commands,
// relation-level temporal clauses to the fixed-snapshot historical path, and
// everything else to RunStatement with the full command configuration. A nil
// stream materializes rows into the outcome; a non-nil stream receives the
// result columns and callback-scoped row source instead.
func (database *Catalog) execStatement(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (_ outcome, err error) {
	statement := parsed.ParseResult.Stmts[index]
	builtins := scope.builtins()
	systemTime := scope.systemTimeOptions(builtins)
	selection, err := session.ResolveSystemTime(ctx, parsed, index, nil, systemTime)
	if err != nil {
		return outcome{}, err
	}
	if selection.Present && selection.Transaction {
		if len(parameters) != 0 {
			return outcome{}, &model.Error{Code: "42P02", Message: "transaction commands do not accept parameters"}
		}
		result, err := session.ExecuteSystemTimeTransaction(ctx, scope.txns, parsed, index, nil, systemTime)
		if err != nil {
			return outcome{}, err
		}
		return outcome{Tag: result.Tag, Committed: result.Commit != nil}, nil
	}
	if selection.Present {
		return database.execHistorical(ctx, scope, parsed, index, builtins, parameters, maxRows, stream)
	}
	if database.readOnly {
		// A historical-only database serves retained fixed snapshots only;
		// implicit latest reads and every write are rejected before planning.
		return outcome{}, &model.Error{Code: "0A000", Message: "historical read-only database requires AS OF SYSTEM TIME"}
	}
	out, err := database.runStatementCore(ctx, scope, parsed, index, statement, parameters, maxRows, stream, streams)
	if isSessionDDLStatement(statement) {
		scope.ddlSeq.Add(1)
	}
	return out, err
}

func (database *Catalog) runStatementCore(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, statement *zpgparse.RawStmt, parameters []expr.ParameterValue, maxRows int, stream rowStream, streams *qcopy.Streams) (_ outcome, err error) {
	builtins := scope.builtins()
	query, err := database.newStatementQuery(ctx)
	if err != nil {
		return outcome{}, err
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
		return outcome{}, err
	}
	if release != nil {
		defer func() { err = errors.Join(err, release()) }()
	}
	execution.Schema = schemaExecution
	execution.Copy = streams
	out := outcome{Rows: make([][]model.Value, 0)}
	var consume func(context.Context, *session.Prepared, *session.Rows) error
	if streams == nil {
		consume = func(statementCtx context.Context, prepared *session.Prepared, rows *session.Rows) error {
			var columns []bind.ResultColumn
			statementBuiltins := builtins
			if prepared != nil {
				columns = prepared.Columns()
				statementBuiltins = prepared.Builtin()
			}
			if stream != nil {
				return stream(statementCtx, columns, statementBuiltins, rows)
			}
			out.builtins = statementBuiltins
			for _, col := range columns {
				out.Columns = append(out.Columns, column{Name: col.Name, Type: col.Layout.Type, Layout: col.Layout, Nullable: col.Nullable})
			}
			return database.consumeRows(statementCtx, rows, &out, maxRows)
		}
	}
	result, err := scope.txns.RunStatement(ctx, statement, session.StatementExecution{
		Execution:  execution,
		Parameters: declaredParameterLayouts(parameters),
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
			return scope.prepareOptions(ctx, snapshot, reader, builtins)
		},
	}, consume)
	out.Tag, out.RowsAffected, out.Committed = result.Tag, result.RowsAffected, result.Commit.Committed
	return out, err
}

// runPreparedCore executes a previously bound and planned *session.Prepared
// directly through RunPrepared, skipping repeated binding and planning.
func (database *Catalog) runPreparedCore(ctx context.Context, scope *sessionScope, prepared *session.Prepared, parameters []expr.ParameterValue, maxRows int, stream rowStream) (_ outcome, err error) {
	builtins := scope.builtins()
	query, err := database.newStatementQuery(ctx)
	if err != nil {
		return outcome{}, err
	}
	defer func() { err = errors.Join(err, query.Close()) }()
	environment := database.executionEnvironment(query, builtins)
	environment.BorrowQuery = true
	environment.Parameters = parameters
	execution := session.PreparedExecution{
		Catalog:     database.catalog,
		Environment: environment,
		Statistics:  ptrTo(scope.statisticsCommandOptions(builtins)),
		TypeRuntime: scope.typeRuntimeOptions(builtins),
		Routines:    scope.routineExecutionOptions(builtins),
	}
	out := outcome{Rows: make([][]model.Value, 0)}
	consume := func(statementCtx context.Context, rows *session.Rows) error {
		columns := prepared.Columns()
		statementBuiltins := prepared.Builtin()
		if stream != nil {
			return stream(statementCtx, columns, statementBuiltins, rows)
		}
		out.builtins = statementBuiltins
		for _, col := range columns {
			out.Columns = append(out.Columns, column{Name: col.Name, Type: col.Layout.Type, Layout: col.Layout, Nullable: col.Nullable})
		}
		return database.consumeRows(statementCtx, rows, &out, maxRows)
	}
	result, err := scope.txns.RunPrepared(ctx, prepared, execution, consume)
	out.Tag, out.RowsAffected, out.Committed = result.Tag, result.RowsAffected, result.Commit.Committed
	return out, err
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
func (database *Catalog) execHistorical(ctx context.Context, scope *sessionScope, parsed *zpgparse.ExtendedParseResult, index int, builtins builtin.ResolveOptions, parameters []expr.ParameterValue, maxRows int, stream rowStream) (_ outcome, err error) {
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
		return outcome{}, err
	}
	defer func() { err = errors.Join(err, historical.Close()) }()
	query, err := database.newStatementQuery(ctx)
	if err != nil {
		return outcome{}, err
	}
	defer func() { err = errors.Join(err, query.Close()) }()
	portal, err := historical.Open(ctx, database.executionEnvironment(query, builtins))
	if err != nil {
		return outcome{}, err
	}
	defer func() { err = errors.Join(err, portal.Close()) }()
	out := outcome{Rows: make([][]model.Value, 0)}
	var columns []bind.ResultColumn
	statementBuiltins := builtins
	if prepared := historical.Prepared(); prepared != nil {
		columns = prepared.Columns()
		statementBuiltins = prepared.Builtin()
	}
	if stream != nil {
		return out, stream(ctx, columns, statementBuiltins, portal)
	}
	out.builtins = statementBuiltins
	for _, col := range columns {
		out.Columns = append(out.Columns, column{Name: col.Name, Type: col.Layout.Type, Layout: col.Layout, Nullable: col.Nullable})
	}
	var batch columnar.Batch
	for {
		if maxRows > 0 && len(out.Rows) >= maxRows {
			break
		}
		nextErr := portal.Next(ctx, &batch)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return out, nextErr
		}
		for lane := range batch.Rows() {
			if maxRows > 0 && len(out.Rows) >= maxRows {
				break
			}
			row := make([]model.Value, len(batch.Columns))
			for columnIndex := range batch.Columns {
				value, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[columnIndex], batch.Lane(lane), 32<<20)
				if copyErr != nil {
					return out, copyErr
				}
				row[columnIndex] = value
			}
			out.Rows = append(out.Rows, row)
		}
	}
	out.Tag, out.RowsAffected, out.Committed = "SELECT", uint64(len(out.Rows)), true
	return out, nil
}

// ptrTo returns the address of a locally constructed options value; the session
// distinguishes "family not configured" by nil pointer, not by zero fields.
func ptrTo[T any](value T) *T { return &value }

func (database *Catalog) consumeRows(ctx context.Context, rows *session.Rows, out *outcome, maxRows int) error {
	var batch columnar.Batch
	for {
		if maxRows > 0 && len(out.Rows) >= maxRows {
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
			if maxRows > 0 && len(out.Rows) >= maxRows {
				return nil
			}
			row := make([]model.Value, len(batch.Columns))
			for index := range batch.Columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(lane), 32<<20)
				if err != nil {
					return err
				}
				row[index] = value
			}
			out.Rows = append(out.Rows, row)
		}
	}
}

// newStatementQuery creates a fresh bounded work account for a statement,
// named object, or nested routine invocation. The caller owns and closes the
// returned query.
func (database *Catalog) newStatementQuery(ctx context.Context) (*runtime.Query, error) {
	workBytes := max(int64(256<<20), int64(database.memoryBudgetBytes))
	return runtime.NewQuery(ctx, database.pool, database.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes: workBytes, OutputBytes: 64 << 20, SpillBytes: 1 << 30,
		SpillMode: runtime.SpillEnabled, Directory: database.spill, MaxFiles: 64, MaxReaders: 64,
	})
}

// executionEnvironment assembles one invocation environment. Every budget is
// explicit and sized to satisfy operator admission invariants under the session
// work_mem scaling (default 64 MiB).
func (database *Catalog) executionEnvironment(query *runtime.Query, builtins builtin.ResolveOptions) exec.Environment {
	exprLimits := expr.DefaultLimits()
	exprLimits.MaxOutputBytes = 32 << 20
	return exec.Environment{
		Query: query, Keys: database.keys, Builtin: builtins, Catalog: database.source,
		MutationTargets: newStackMutationTargets{descriptors: database.currentDescriptors(), policies: database.policies},
		Limits: exec.Limits{
			Expressions: exprLimits, BatchRows: columnar.DefaultBatchSize, BatchBytes: 32 << 20, ParameterBytes: 32 << 20, MetadataBytes: 8 << 20,
		},
		// The shared spool owns one layout header and one append value header per
		// column, so its metadata budget admits the widest relation the catalog
		// accepts (IndexLimits.MaxColumns = 64) with room for reader state.
		Spool:     exrowstore.Limits{MemoryBytes: 4 << 20, MaxBytes: 256 << 20, MaxRows: 1 << 30, MaxRecordBytes: 1 << 20, MaxLargeRecordBytes: 32 << 20, MetadataBytes: 64 << 10, DecodeBytes: 1 << 20, MaxReaders: 32},
		Sort:      exsort.Options{MaxRecordBytes: 64 << 10, RunBytes: 4 << 20, WorkBytes: 64 << 20, MaxRuns: 32, MergeFanIn: 16},
		Join:      exjoin.Options{MaxRecordBytes: 64 << 10, MemoryBytes: 4 << 20, HashBytes: 8 << 20, WorkBytes: 64 << 20, PartitionBytes: 256 << 20, MaxPartitions: 32, MaxDepth: 4},
		Aggregate: exaggregate.Options{MaxRecordBytes: 64 << 10, GroupBytes: 16 << 20, WorkBytes: 64 << 20, PartitionMemoryBytes: 4 << 20, MaxPartitions: 32},
		Set:       exsetop.Options{MaxRecordBytes: 64 << 10, GroupBytes: 16 << 20, PartitionMemoryBytes: 4 << 20, MaxSpillBytes: 256 << 20, WorkBytes: 64 << 20, MaxPasses: 64},
		Window:    exwindow.Options{MaxRecordBytes: 64 << 10, PartitionMemoryBytes: 4 << 20, MaxPartitionBytes: 256 << 20, MaxPartitionRows: 1 << 24, WorkBytes: 64 << 20},
		Apply:     exapply.Options{MaxRowBytes: 1 << 20, WorkBytes: 4 << 20},
		Modify: exmodify.Options{
			// MaxRecordBytes sizes the baseline per-row workspace and internal sort
			// while MaxRowBytes admits multi-megabyte TOAST rows on demand under
			// WorkMemory admission.
			BatchRows: columnar.DefaultBatchSize, BatchBytes: 32 << 20, MaxRecordBytes: 64 << 10, MaxRowBytes: 32 << 20, MaxSpoolBytes: 256 << 20,
			MaxRows: 1 << 30, WorkBytes: 128 << 20, SortRunBytes: 4 << 20, SortMaxRuns: 4, SortMergeFanIn: 2,
		},
		Recursive: exrecursive.Options{
			MaxIterations: 100000,
			Spool:         exrowstore.Limits{MemoryBytes: 4 << 20, MaxBytes: 256 << 20, MaxRows: 1 << 24, MaxRecordBytes: 1 << 20, MaxLargeRecordBytes: 32 << 20, MetadataBytes: 64 << 10, DecodeBytes: 1 << 20, MaxReaders: 32},
			Seen:          exdistinctset.Options{MemoryBytes: 4 << 20, WorkBytes: 16 << 20, MaxKeyBytes: 64 << 10, MaxKeys: 1 << 24, MaxLevels: 16},
		},
	}
}
