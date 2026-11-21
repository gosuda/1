package supersql

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	controlstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// objectCommandOptions wires CREATE/ALTER/DROP/TRUNCATE/INDEX declarations,
// including their column and index expression compilers.
func (scope *sessionScope) objectCommandOptions(builtins builtin.ResolveOptions) session.ObjectCommandOptions {
	database := scope.database
	limits := database.irLimits()
	metadata := uint64(8 << 20)
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
		Limits:         schema.IndexLimits{MetadataBytes: metadata, EntryBytes: 32 << 20, MaxColumns: 1024},
		PolicyBytes:    8 * metadata,
		MaxDescriptors: 4096,
		Constraints:    database.policies,
	}
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
			Limits: controlstats.Limits{MaxBytes: 16 << 20, MaxColumns: 1024, MaxValues: 4096, MaxDepth: 32},
		},
	}
}

// typeCommandOptions wires CREATE/ALTER/DROP TYPE, DOMAIN and their cascades.
func (scope *sessionScope) typeCommandOptions(builtins builtin.ResolveOptions) session.TypeCommandOptions {
	database := scope.database
	return session.TypeCommandOptions{
		Binding: scope.declarationOptions(builtins), MaxDescriptors: 4096,
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
		MaxDescriptors:      4096,
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
			Sequence: scope.sequences, CatalogRelations: database.native.Registry,
			BaseFunctions: unresolvedFunctions{}, BaseBuiltin: builtins, BaseKeys: database.keys, PathBytes: 4096,
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
