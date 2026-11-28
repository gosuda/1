package supersql

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// QueryEvent carries per-statement execution metrics delivered to a
// QueryObserver or OnSlowQuery callback after statement completion.
type QueryEvent struct {
	SQL              string
	Duration         time.Duration
	PlanningDuration time.Duration
	RowsAffected     uint64
	SharedHitBlocks  uint64
	SharedReadBlocks uint64
	SpillBytes       uint64
	Err              error
}

// QueryObserver receives one QueryEvent for each completed SQL statement.
// Implementations must be safe for concurrent invocation across sessions.
type QueryObserver interface {
	OnQuery(ctx context.Context, event QueryEvent)
}

// QueryObserverFunc adapts an ordinary function to QueryObserver.
type QueryObserverFunc func(ctx context.Context, event QueryEvent)

// OnQuery invokes fn(ctx, event).
func (fn QueryObserverFunc) OnQuery(ctx context.Context, event QueryEvent) {
	if fn != nil {
		fn(ctx, event)
	}
}

// DatabaseMetrics is a point-in-time snapshot of query, maintenance, storage,
// and block-cache counters for an opened Catalog.
type DatabaseMetrics struct {
	QueriesTotal        uint64
	QueriesFailed       uint64
	SlowQueriesTotal    uint64
	AutoAnalyzeRuns     uint64
	WALSegments         uint64
	LiveRunFiles        uint64
	UncompactedRunFiles uint64
	BlockCacheHits      uint64
	BlockCacheMisses    uint64
	BlockCacheEntries   uint64
}

type blockCacheEntriesProvider interface {
	BlockCacheEntries() uint64
}

// Metrics returns a live snapshot of query, auto-ANALYZE, storage backlog, and
// block-cache counters for the database.
func (database *Catalog) Metrics(ctx context.Context) DatabaseMetrics {
	if database == nil {
		return DatabaseMetrics{}
	}
	metrics := DatabaseMetrics{
		QueriesTotal:     database.queriesTotal.Load(),
		QueriesFailed:    database.queriesFailed.Load(),
		SlowQueriesTotal: database.slowQueriesTotal.Load(),
	}
	if database.autoAnalyze != nil {
		metrics.AutoAnalyzeRuns = database.autoAnalyze.AutoAnalyzeRuns()
	}
	if database.backend != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if wal, runs, err := database.backend.MaintenanceBacklog(ctx); err == nil {
			metrics.WALSegments = wal
			metrics.LiveRunFiles = runs
			metrics.UncompactedRunFiles = min(runs, database.backend.UncompactedRunFiles())
		}
		if hits, misses, ok := database.blockCacheStats(); ok {
			metrics.BlockCacheHits = hits
			metrics.BlockCacheMisses = misses
		}
		if provider, ok := any(database.backend).(blockCacheEntriesProvider); ok {
			metrics.BlockCacheEntries = provider.BlockCacheEntries()
		}
	}
	return metrics
}

func (database *Catalog) blockCacheStats() (hits, misses uint64, ok bool) {
	if database == nil || database.backend == nil {
		return 0, 0, false
	}
	provider, ok := any(database.backend).(exec.BufferCacheStatsProvider)
	if !ok {
		return 0, 0, false
	}
	hits, misses = provider.BlockCacheStats()
	return hits, misses, true
}

type internalQueryContextKey struct{}

func withInternalQuery(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalQueryContextKey{}, true)
}

func isInternalQuery(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	internal, _ := ctx.Value(internalQueryContextKey{}).(bool)
	return internal
}

func (database *Catalog) needsQueryEvent() bool {
	return database != nil && database.observing.Load() && (database.observer != nil || database.onSlowQuery != nil)
}

func (database *Catalog) observeQuery(ctx context.Context, event QueryEvent) {
	if database == nil || !database.observing.Load() || isInternalQuery(ctx) {
		return
	}
	database.queriesTotal.Add(1)
	if event.Err != nil {
		database.queriesFailed.Add(1)
	}
	isSlow := database.slowQueryThreshold > 0 && event.Duration >= database.slowQueryThreshold
	if isSlow {
		database.slowQueriesTotal.Add(1)
	}
	if database.observer != nil {
		database.observer.OnQuery(ctx, event)
	}
	if isSlow && database.onSlowQuery != nil {
		database.onSlowQuery(ctx, event)
	}
}

type tableTarget struct {
	schema string
	table  string
}

type tableMutationState struct {
	pendingMutations uint64
	lastLiveRows     uint64
}

type autoAnalyzeTracker struct {
	mu     sync.Mutex
	tables map[tableTarget]*tableMutationState
	runs   atomic.Uint64
}

func newAutoAnalyzeTracker() *autoAnalyzeTracker {
	return &autoAnalyzeTracker{tables: make(map[tableTarget]*tableMutationState)}
}

func normalizeTableTarget(schemaName, tableName string) (tableTarget, bool) {
	if tableName == "" {
		return tableTarget{}, false
	}
	if schemaName == "" {
		schemaName = "public"
	}
	if isIgnoredAutoAnalyzeSchema(schemaName) {
		return tableTarget{}, false
	}
	return tableTarget{schema: schemaName, table: tableName}, true
}

func isIgnoredAutoAnalyzeSchema(schemaName string) bool {
	return schemaName == "pg_catalog" ||
		schemaName == "information_schema" ||
		schemaName == "pg_temp" ||
		strings.HasPrefix(schemaName, "pg_temp_") ||
		strings.HasPrefix(schemaName, "pg_toast")
}

func (t *autoAnalyzeTracker) RecordTableMutation(schemaName, tableName string, rows uint64) {
	if t == nil || rows == 0 {
		return
	}
	key, ok := normalizeTableTarget(schemaName, tableName)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.tables[key]
	if state == nil {
		state = &tableMutationState{}
		t.tables[key] = state
	}
	if rows > math.MaxUint64-state.pendingMutations {
		state.pendingMutations = math.MaxUint64
	} else {
		state.pendingMutations += rows
	}
}

func (t *autoAnalyzeTracker) RecordTableAnalyzed(schemaName, tableName string, liveRows uint64) {
	if t == nil {
		return
	}
	key, ok := normalizeTableTarget(schemaName, tableName)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.tables[key]
	if state == nil {
		state = &tableMutationState{}
		t.tables[key] = state
	}
	state.pendingMutations = 0
	state.lastLiveRows = liveRows
}

func (t *autoAnalyzeTracker) RemoveTable(schemaName, tableName string) {
	if t == nil {
		return
	}
	key, ok := normalizeTableTarget(schemaName, tableName)
	if !ok {
		return
	}
	t.mu.Lock()
	delete(t.tables, key)
	t.mu.Unlock()
}

func (t *autoAnalyzeTracker) DueTables(minRows uint64, scaleFactor float64) []tableTarget {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	var due []tableTarget
	for key, state := range t.tables {
		if state.pendingMutations == 0 {
			continue
		}
		var scaled uint64
		if scaleFactor > 0 && state.lastLiveRows > 0 {
			product := scaleFactor * float64(state.lastLiveRows)
			if product >= float64(math.MaxUint64) {
				scaled = math.MaxUint64
			} else if product > 0 {
				scaled = uint64(product)
			}
		}
		threshold := minRows
		if scaled > math.MaxUint64-threshold {
			threshold = math.MaxUint64
		} else {
			threshold += scaled
		}
		if state.pendingMutations >= threshold {
			due = append(due, key)
		}
	}
	t.mu.Unlock()
	slices.SortFunc(due, func(a, b tableTarget) int {
		if c := cmp.Compare(a.schema, b.schema); c != 0 {
			return c
		}
		return cmp.Compare(a.table, b.table)
	})
	return due
}

func (t *autoAnalyzeTracker) trackedTables() []tableTarget {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	targets := make([]tableTarget, 0, len(t.tables))
	for key := range t.tables {
		targets = append(targets, key)
	}
	t.mu.Unlock()
	return targets
}

func (t *autoAnalyzeTracker) AutoAnalyzeRuns() uint64 {
	if t == nil {
		return 0
	}
	return t.runs.Load()
}

// RunAutoAnalyze executes ANALYZE on every table whose accumulated mutations
// have reached the configured auto-ANALYZE threshold, resetting each analyzed
// table's pending mutation counter. It returns the number of tables analyzed.
func (database *Catalog) RunAutoAnalyze(ctx context.Context) (int, error) {
	if database == nil {
		return 0, ErrDatabaseClosed
	}
	if err := database.checkOpen(); err != nil {
		return 0, err
	}
	analyzed, err := database.runAutoAnalyze(ctx)
	return analyzed, wrapError(err)
}

// runAutoAnalyzeIfEnabled runs one auto-ANALYZE pass from the background
// maintenance loop without taking database.mu. Benign per-table SQL failures
// (such as concurrent DDL or lock contention) are ignored so they never abort
// the maintenance loop; context cancellation is propagated immediately.
func (database *Catalog) runAutoAnalyzeIfEnabled(ctx context.Context) error {
	if database == nil || database.readOnly || database.maintenance.AutoAnalyzeDisabled {
		return nil
	}
	_, err := database.runAutoAnalyze(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && !autoAnalyzeBenignContention(err) && transientMaintenance(err) {
		return err
	}
	return nil
}

// autoAnalyzeBenignContention reports SQL-level concurrency contention errors
// (serialization failure / wound-wait, deadlock, lock timeout, transaction
// capacity) that are expected when background auto-ANALYZE overlaps with active
// DDL or DML. Ignoring them keeps retention and GC running on the same tick and
// avoids surfacing benign contention through Close.
func autoAnalyzeBenignContention(err error) bool {
	if errors.Is(err, txn.ErrManagerCapacity) {
		return true
	}
	switch SQLState(err) {
	case "40001", "40P01", "55P03", "53300":
		return true
	}
	return false
}

// runAutoAnalyze executes ANALYZE on due tables using a dedicated short-lived
// session without taking database.mu, making it safe to invoke from the
// background maintenance loop while Close is running.
func (database *Catalog) runAutoAnalyze(ctx context.Context) (int, error) {
	if database == nil || database.readOnly || database.autoAnalyze == nil {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	due := database.autoAnalyze.DueTables(database.maintenance.AutoAnalyzeMinRows, database.maintenance.AutoAnalyzeScaleFactor)
	if len(due) == 0 {
		return 0, nil
	}
	ctx = txn.WithNonWounding(withInternalQuery(ctx))
	scope, err := database.newSession(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = scope.close() }()

	analyzed := 0
	for _, target := range due {
		if err := ctx.Err(); err != nil {
			return analyzed, err
		}
		stmtSQL := fmt.Sprintf("ANALYZE (SKIP_LOCKED) %s.%s", quoteIdent(target.schema), quoteIdent(target.table))
		parsed, err := scope.txns.Parse(ctx, stmtSQL, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
		if err != nil {
			return analyzed, err
		}
		if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
			continue
		}
		_, err = database.runStatementCore(ctx, scope, parsed, 0, parsed.ParseResult.Stmts[0], nil, 0, nil, nil)
		if err != nil {
			if ctx.Err() != nil {
				return analyzed, ctx.Err()
			}
			code := SQLState(err)
			if code == "42P01" || code == "3F000" {
				// The table or schema was dropped (or was session-temporary).
				database.autoAnalyze.RemoveTable(target.schema, target.table)
				continue
			}
			return analyzed, err
		}
		liveRows, _ := database.lookupTableLiveRows(ctx, target.schema, target.table)
		database.autoAnalyze.RecordTableAnalyzed(target.schema, target.table, liveRows)
		database.autoAnalyze.runs.Add(1)
		analyzed++
	}
	return analyzed, nil
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func (database *Catalog) lookupTableLiveRows(ctx context.Context, schemaName, tableName string) (uint64, bool) {
	if database == nil || database.manager == nil || database.catalog == nil || database.statistics == nil {
		return 0, false
	}
	view, err := database.manager.CurrentReadView(ctx)
	if err != nil {
		return 0, false
	}
	defer func() { _ = view.Close() }()
	ts := view.Timestamp()
	catSnap, err := database.catalog.Snapshot(view, ts, catalog.BorrowReader)
	if err != nil {
		return 0, false
	}
	defer func() { _ = catSnap.Close() }()
	schemaDesc, err := catSnap.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
	if err != nil || schemaDesc.Descriptor == nil {
		return 0, false
	}
	tableDesc, err := catSnap.Resolve(ctx, model.NamespaceName{Parent: schemaDesc.Descriptor.ID(), Kind: model.ObjectTable, Name: tableName})
	if err != nil || tableDesc.Descriptor == nil {
		return 0, false
	}
	phys := tableDesc.Descriptor.Physical()
	target := storage.ObjectRef{
		ObjectID:          tableDesc.Descriptor.ID(),
		Generation:        phys.Generation,
		DescriptorVersion: tableDesc.Descriptor.Version(),
		EncodingVersion:   phys.EncodingVersion,
	}
	statSnap, err := database.statistics.Snapshot(view, ts)
	if err != nil {
		return 0, false
	}
	tbl, found, err := statSnap.Lookup(ctx, target)
	if err != nil || !found || tbl == nil || !tbl.LiveRows.Known {
		return 0, false
	}
	return tbl.LiveRows.Value, true
}

func (database *Catalog) resolveObjectTable(ctx context.Context, objectID model.ObjectID) (string, string, bool) {
	if database == nil || database.manager == nil || database.catalog == nil || objectID == 0 {
		return "", "", false
	}
	view, err := database.manager.CurrentReadView(ctx)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = view.Close() }()
	catSnap, err := database.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = catSnap.Close() }()
	tableDesc, err := catSnap.Descriptor(ctx, objectID)
	if err != nil || tableDesc.Descriptor == nil || tableDesc.Descriptor.Kind() != model.ObjectTable {
		return "", "", false
	}
	tableName := tableDesc.Descriptor.Name().Name
	parentID := tableDesc.Descriptor.Name().Parent
	schemaName := "public"
	if parentID != 0 {
		if schemaDesc, err := catSnap.Descriptor(ctx, parentID); err == nil && schemaDesc.Descriptor != nil {
			schemaName = schemaDesc.Descriptor.Name().Name
		}
	}
	return schemaName, tableName, true
}

func defaultSchemaForScope(scope *sessionScope) string {
	if scope != nil && scope.settings != nil {
		for _, entry := range scope.settings.SearchPath() {
			if entry == "" || entry == "$user" || isIgnoredAutoAnalyzeSchema(entry) {
				continue
			}
			return entry
		}
	}
	return "public"
}

func (database *Catalog) recordRangeVarMutation(scope *sessionScope, rel *zpgparse.RangeVar, rows uint64) {
	if database == nil || database.autoAnalyze == nil || rel == nil || rel.Relname == "" || rows == 0 {
		return
	}
	if rel.Relpersistence == "t" {
		return
	}
	schemaName := rel.Schemaname
	if schemaName == "" {
		schemaName = defaultSchemaForScope(scope)
	}
	database.autoAnalyze.RecordTableMutation(schemaName, rel.Relname, rows)
}

func (database *Catalog) recordStatementMutations(ctx context.Context, scope *sessionScope, statement *zpgparse.RawStmt, out outcome) {
	if database == nil || database.autoAnalyze == nil || statement == nil || statement.Stmt == nil {
		return
	}
	node := statement.Stmt
	if explainStmt := node.GetExplainStmt(); explainStmt != nil && explainStmt.Query != nil {
		node = explainStmt.Query
	}
	if vac := node.GetVacuumStmt(); vac != nil && !vac.IsVacuumcmd {
		if len(vac.Rels) == 0 {
			for _, target := range database.autoAnalyze.trackedTables() {
				liveRows, _ := database.lookupTableLiveRows(ctx, target.schema, target.table)
				database.autoAnalyze.RecordTableAnalyzed(target.schema, target.table, liveRows)
			}
			return
		}
		for _, item := range vac.Rels {
			vr := item.GetVacuumRelation()
			if vr == nil || vr.Relation == nil || vr.Relation.Relname == "" {
				continue
			}
			schemaName := vr.Relation.Schemaname
			if schemaName == "" {
				schemaName = defaultSchemaForScope(scope)
			}
			liveRows, _ := database.lookupTableLiveRows(ctx, schemaName, vr.Relation.Relname)
			database.autoAnalyze.RecordTableAnalyzed(schemaName, vr.Relation.Relname, liveRows)
		}
		return
	}
	if drop := node.GetDropStmt(); drop != nil && drop.RemoveType == zpgparse.ObjectType_OBJECT_TABLE {
		for _, obj := range drop.Objects {
			list := obj.GetList()
			if list == nil || len(list.Items) == 0 {
				continue
			}
			var parts []string
			for _, elem := range list.Items {
				if s := elem.GetString_(); s != nil {
					parts = append(parts, s.Sval)
				}
			}
			switch len(parts) {
			case 1:
				database.autoAnalyze.RemoveTable(defaultSchemaForScope(scope), parts[0])
			case 2:
				database.autoAnalyze.RemoveTable(parts[0], parts[1])
			case 3:
				database.autoAnalyze.RemoveTable(parts[1], parts[2])
			}
		}
		return
	}
	if out.RowsAffected == 0 {
		return
	}
	if ins := node.GetInsertStmt(); ins != nil {
		database.recordRangeVarMutation(scope, ins.Relation, out.RowsAffected)
		return
	}
	if upd := node.GetUpdateStmt(); upd != nil {
		database.recordRangeVarMutation(scope, upd.Relation, out.RowsAffected)
		return
	}
	if del := node.GetDeleteStmt(); del != nil {
		database.recordRangeVarMutation(scope, del.Relation, out.RowsAffected)
		return
	}
	if mrg := node.GetMergeStmt(); mrg != nil {
		database.recordRangeVarMutation(scope, mrg.Relation, out.RowsAffected)
		return
	}
	if cp := node.GetCopyStmt(); cp != nil && cp.IsFrom {
		database.recordRangeVarMutation(scope, cp.Relation, out.RowsAffected)
		return
	}
}

func (database *Catalog) recordPreparedMutations(ctx context.Context, prepared *session.Prepared, rowsAffected uint64) {
	if database == nil || database.autoAnalyze == nil || prepared == nil || rowsAffected == 0 {
		return
	}
	graph := prepared.Graph()
	if graph == nil || graph.Plan() == nil {
		return
	}
	plan := graph.Plan()
	for index := range plan.NodeCount() {
		id := physical.NodeID(index + 1)
		node, ok := plan.Node(id)
		if !ok {
			continue
		}
		meta := node.Metadata()
		if meta.Algorithm != physical.Modify {
			continue
		}
		logical, ok := plan.Graph(meta.Logical.Graph)
		if !ok {
			continue
		}
		rel, ok := logical.Rel(meta.Logical.Rel)
		if !ok || rel.Object == 0 {
			continue
		}
		if schemaName, tableName, ok := database.resolveObjectTable(ctx, rel.Object); ok {
			database.autoAnalyze.RecordTableMutation(schemaName, tableName, rowsAffected)
		}
	}
}

type rawSQLContextKey struct{}

func withRawSQL(ctx context.Context, sql string) context.Context {
	return context.WithValue(ctx, rawSQLContextKey{}, sql)
}

func rawSQLFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sql, _ := ctx.Value(rawSQLContextKey{}).(string)
	return sql
}

func statementSQLSlice(fullSQL string, stmt *zpgparse.RawStmt) string {
	if stmt == nil || fullSQL == "" {
		return fullSQL
	}
	start := int(stmt.StmtLocation)
	if start < 0 || start >= len(fullSQL) {
		return fullSQL
	}
	if stmt.StmtLen > 0 {
		end := start + int(stmt.StmtLen)
		if end <= len(fullSQL) {
			return strings.TrimSpace(fullSQL[start:end])
		}
	}
	return strings.TrimSpace(fullSQL[start:])
}

func resolveEventSQL(ctx context.Context, statement *zpgparse.RawStmt, fallback string) string {
	if raw := rawSQLFromContext(ctx); raw != "" {
		return raw
	}
	if statement != nil && statement.Stmt != nil {
		if deparsed, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{statement}}); err == nil && deparsed != "" {
			return deparsed
		}
	}
	return fallback
}

type queryScanObserver struct {
	blocksRead atomic.Uint64
}

func (o *queryScanObserver) ObserveScan(observation storage.ScanObservation) {
	if o == nil {
		return
	}
	if observation.BlocksRead.Known {
		o.blocksRead.Add(observation.BlocksRead.Value)
	}
}

type countingRowSource struct {
	source rowSource
	rows   *uint64
}

func (c countingRowSource) Next(ctx context.Context, batch *columnar.Batch) error {
	err := c.source.Next(ctx, batch)
	if err == nil && batch != nil {
		*c.rows += uint64(batch.Rows())
	}
	return err
}
