package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	cursorOptionBinary   int32 = 1 << 0
	cursorOptionScroll   int32 = 1 << 1
	cursorOptionNoScroll int32 = 1 << 2
	cursorOptionHold     int32 = 1 << 5
)

type namedCursor struct {
	name              string
	statement         string
	createdAt         time.Time
	holdable          bool
	binary            bool
	scrollable        bool
	transaction       *txn.Transaction
	serial            uint64
	prepared          *Prepared
	layouts           []model.TypeLayout
	portal            *exec.Portal
	closeCapabilities func() error
	scope             *preparedMutationScope
	spool             *rowstore.Spool
	spoolQuery        *runtime.Query
	position          int64
	lastMoved         uint64
	batchRows         int
	batchBytes        uint64
	metadataBytes     uint64
	sealed            bool
}

func cursorMetadataBytes(name, statement string) uint64 {
	return uint64(unsafe.Sizeof(namedCursor{})) + uint64(len(name)+len(statement))
}

func (cursor *namedCursor) closeSource() error {
	var err error
	if cursor.portal != nil {
		err = cursor.portal.Close()
		cursor.portal = nil
	}
	if cursor.closeCapabilities != nil {
		err = errors.Join(err, cursor.closeCapabilities())
		cursor.closeCapabilities = nil
	}
	if cursor.scope != nil {
		cursor.scope.active.Store(false)
	}
	return err
}

func (cursor *namedCursor) close() error {
	err := cursor.closeSource()
	if cursor.spool != nil {
		err = errors.Join(err, cursor.spool.Close())
		cursor.spool = nil
	}
	if cursor.spoolQuery != nil {
		err = errors.Join(err, cursor.spoolQuery.Close())
		cursor.spoolQuery = nil
	}
	cursor.prepared = nil
	cursor.layouts = nil
	cursor.transaction = nil
	return err
}

type cursorSettingsRuntime struct{ owner *Transactions }

func (runtime cursorSettingsRuntime) current(ctx context.Context) (settingRuntime, error) {
	operation, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation)
	if !ok || operation.owner != runtime.owner || !operation.active.Load() {
		return settingRuntime{}, errSessionTransactionReentry
	}
	return settingRuntime{owner: runtime.owner, operation: operation}, nil
}

func (runtime cursorSettingsRuntime) CurrentSetting(ctx context.Context, name string, missingOK bool) (model.Value, error) {
	current, err := runtime.current(ctx)
	if err != nil {
		return model.Value{}, err
	}
	return current.CurrentSetting(ctx, name, missingOK)
}

func (runtime cursorSettingsRuntime) SetConfig(ctx context.Context, name string, value *string, local bool, maximum uint64) (model.Value, error) {
	current, err := runtime.current(ctx)
	if err != nil {
		return model.Value{}, err
	}
	return current.SetConfig(ctx, name, value, local, maximum)
}

func (runtime cursorSettingsRuntime) FormatLocale(ctx context.Context) (model.FormatLocale, error) {
	current, err := runtime.current(ctx)
	if err != nil {
		return model.FormatLocale{}, err
	}
	return current.FormatLocale(ctx)
}

func (runtime cursorSettingsRuntime) CatalogSettings(ctx context.Context) ([]catalogrel.Setting, error) {
	current, err := runtime.current(ctx)
	if err != nil {
		return nil, err
	}
	return current.CatalogSettings(ctx)
}

func (transactions *Transactions) declareCursor(ctx context.Context, command bind.DeclareCursorCommand, options PreparedExecution) (err error) {
	declaration := command.Statement
	if declaration == nil || declaration.Query == nil || declaration.Portalname == "" || options.statementPlanner == nil {
		return errPreparedInvocation
	}
	if !transactions.active {
		return &model.Error{Code: "25001", Message: "DECLARE CURSOR can only be used in transaction blocks"}
	}
	if err := transactions.ordinary(); err != nil {
		return err
	}
	if _, exists := transactions.named.cursors[declaration.Portalname]; exists {
		return &model.Error{Code: "42P03", Message: "cursor already exists"}
	}
	if len(transactions.named.cursors) == transactions.named.options.MaxCursors {
		return &model.Error{Code: "54000", Message: "cursor limit exceeded"}
	}
	if declaration.Query.GetSelectStmt() == nil {
		return &model.Error{Code: "42P11", Message: "cursor query must be a SELECT"}
	}
	createdAt, err := namedObjectTime(transactions.temporal.statement)
	if err != nil {
		return err
	}
	statementSQL, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{{Stmt: declaration.Query}}})
	if err != nil {
		return err
	}
	if options.Environment.Spool.MaxRows > math.MaxInt64 {
		return &model.Error{Code: "22023", Message: "cursor row limit exceeds SQL cursor position range"}
	}
	metadataBytes := cursorMetadataBytes(declaration.Portalname, statementSQL)
	if err := transactions.named.reserve(metadataBytes); err != nil {
		return err
	}
	reserved := true
	defer func() {
		if reserved {
			transactions.named.release(metadataBytes)
		}
	}()
	resources := transactions.settings.Resources()
	builtins := transactions.settings.BuiltinOptions(options.Environment.Builtin.Collation, options.Environment.Builtin.DefaultCollation)
	var cursor *namedCursor
	err = transactions.routineAtomicStatement(txn.WithLockTimeout(ctx, resources.LockTimeout), func(statementContext context.Context) (runErr error) {
		parsed := &zpgparse.RawStmt{Stmt: declaration.Query}
		planningView, bound, _, runErr := transactions.namedStatementPlanView(statementContext, options, builtins, parsed, nil)
		if runErr != nil {
			return runErr
		}
		var compiled *Prepared
		if bound != nil {
			if options.analyzedPlanner == nil {
				runErr = errPreparedInvocation
			} else {
				compiled, runErr = options.analyzedPlanner(statementContext, planningView, builtins, &Prepared{bound: bound}, nil)
			}
		} else {
			compiled, runErr = options.statementPlanner(statementContext, planningView, builtins, parsed, nil)
		}
		runErr = errors.Join(runErr, planningView.Close())
		if runErr != nil {
			return runErr
		}
		if len(compiled.Parameters()) != 0 || preparedWrites(compiled) {
			return &model.Error{Code: "42P11", Message: "cursor query cannot contain parameters or data modification"}
		}
		sourceQuery, runErr := transactions.named.options.NewQuery(context.WithoutCancel(transactions.life))
		if runErr != nil {
			return runErr
		}
		if runErr = sourceQuery.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); runErr != nil {
			return errors.Join(runErr, sourceQuery.Close())
		}
		spoolQuery, runErr := transactions.named.options.NewQuery(context.WithoutCancel(transactions.life))
		if runErr != nil {
			return errors.Join(runErr, sourceQuery.Close())
		}
		if runErr = spoolQuery.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); runErr != nil {
			return errors.Join(runErr, sourceQuery.Close(), spoolQuery.Close())
		}
		layouts := preparedLayouts(compiled)
		spool, runErr := rowstore.New(statementContext, spoolQuery, layouts, options.Environment.Spool)
		if runErr != nil {
			return errors.Join(runErr, sourceQuery.Close(), spoolQuery.Close())
		}
		dependencies, _, runErr := compiled.ReadDependencies(statementContext, options.Catalog, options.Environment.Limits.MetadataBytes)
		if runErr != nil {
			return errors.Join(runErr, spool.Close(), spoolQuery.Close(), sourceQuery.Close())
		}
		view, runErr := openPreparedTransactionReadView(statementContext, transactions.transaction, compiled, dependencies)
		if runErr != nil {
			return errors.Join(runErr, spool.Close(), spoolQuery.Close(), sourceQuery.Close())
		}
		environment := options.Environment
		environment.Query = sourceQuery
		// This query is the session's own, not the caller's borrowed one.
		environment.BorrowQuery = false
		environment.SampleSeed = uint64(createdAt.UnixNano()) ^ (transactions.named.cursorSerial+1)*0x9e3779b97f4a7c15
		if environment.SampleSeed == 0 {
			environment.SampleSeed = 1
		}
		environment.Reader = view
		environment.Builtin.Types = compiled.builtins.Types
		environment.Builtin.InformationSchemaFunctions = compiled.builtins.InformationSchemaFunctions
		environment.Builtin.InformationSchemaTypes = compiled.builtins.InformationSchemaTypes
		transactions.statementTypes = compiled.builtins.Types
		if encoder, ok := environment.Keys.(*builtin.KeyEncoder); ok {
			environment.Keys = encoder.WithResolveOptions(environment.Builtin)
		}
		settings := cursorSettingsRuntime{owner: transactions}
		environment.Settings = settings
		environment.SessionIntrospection = transactions
		if environment.Catalog != nil {
			environment.Catalog, runErr = catalogrel.AttachSettings(environment.Catalog, settings)
			if runErr == nil {
				environment.Catalog, runErr = attachCatalogView(environment.Catalog, view)
			}
			if runErr != nil {
				return errors.Join(runErr, view.Close(), spool.Close(), spoolQuery.Close(), sourceQuery.Close())
			}
		}
		scope := &preparedMutationScope{transaction: transactions.transaction, ambient: true}
		environment.Modify.Transaction = scope
		environment.Modify.Deferral = transactions
		portalContext := context.WithValue(statementContext, preparedScopeKey{}, scope)
		var closeCapabilities func() error
		portalContext, closeCapabilities, runErr = attachRoutineExecution(portalContext, transactions, transactions.transaction, view, scope, &environment, options.Environment.Catalog, options.Catalog, options, options.Routines, compiled.Dependencies(), false)
		if runErr != nil {
			return errors.Join(runErr, view.Close(), spool.Close(), spoolQuery.Close(), sourceQuery.Close())
		}
		environment.Modify.Transaction = scope
		environment.Modify.Deferral = transactions
		portal, runErr := compiled.Open(context.WithoutCancel(portalContext), environment)
		if runErr != nil {
			return errors.Join(runErr, closeCapabilities(), spool.Close(), spoolQuery.Close())
		}
		transactions.named.cursorSerial++
		flags := declaration.Options
		cursor = &namedCursor{
			name: declaration.Portalname, statement: statementSQL, createdAt: createdAt,
			holdable: flags&cursorOptionHold != 0, binary: flags&cursorOptionBinary != 0,
			scrollable:  flags&cursorOptionScroll != 0 && flags&cursorOptionNoScroll == 0,
			transaction: transactions.transaction, serial: transactions.named.cursorSerial,
			prepared: compiled, layouts: layouts, portal: portal, closeCapabilities: closeCapabilities, scope: scope, spool: spool, spoolQuery: spoolQuery,
			metadataBytes: metadataBytes, batchRows: options.Environment.Limits.BatchRows,
			batchBytes: options.Environment.Limits.BatchBytes,
		}
		return nil
	})
	if err != nil {
		return err
	}
	transactions.named.cursors[cursor.name] = cursor
	reserved = false
	return nil
}

func preparedLayouts(prepared *Prepared) []model.TypeLayout {
	columns := prepared.Columns()
	layouts := make([]model.TypeLayout, len(columns))
	for i := range columns {
		layouts[i] = columns[i].Layout
	}
	return layouts
}

func (transactions *Transactions) closeCursor(command bind.CloseCursorCommand) error {
	if command.All {
		return transactions.closeAllCursors()
	}
	cursor := transactions.named.cursors[command.Name]
	if cursor == nil {
		return &model.Error{Code: "34000", Message: "cursor does not exist"}
	}
	delete(transactions.named.cursors, command.Name)
	transactions.named.release(cursor.metadataBytes)
	return cursor.close()
}

func (transactions *Transactions) closeAllCursors() error {
	var err error
	for name, cursor := range transactions.named.cursors {
		delete(transactions.named.cursors, name)
		transactions.named.release(cursor.metadataBytes)
		err = errors.Join(err, cursor.close())
	}
	return err
}

func (transactions *Transactions) rollbackCursorsTo(serial uint64) error {
	var err error
	for name, cursor := range transactions.named.cursors {
		if cursor.transaction == transactions.transaction && cursor.serial > serial {
			delete(transactions.named.cursors, name)
			transactions.named.release(cursor.metadataBytes)
			err = errors.Join(err, cursor.close())
		}
	}
	return err
}

func (transactions *Transactions) prepareCursorsForCommit(ctx context.Context) error {
	if transactions.transaction == nil {
		return nil
	}
	return transactions.routineAtomicStatement(ctx, func(statementContext context.Context) error {
		var err error
		for name, cursor := range transactions.named.cursors {
			if cursor.transaction != transactions.transaction {
				continue
			}
			if cursor.holdable {
				if materializeErr := cursor.materialize(statementContext); materializeErr != nil {
					delete(transactions.named.cursors, name)
					transactions.named.release(cursor.metadataBytes)
					err = errors.Join(err, materializeErr, cursor.close())
				}
				continue
			}
			delete(transactions.named.cursors, name)
			transactions.named.release(cursor.metadataBytes)
			err = errors.Join(err, cursor.close())
		}
		return err
	})
}

func (transactions *Transactions) finishTransactionCursors(committed bool) error {
	var err error
	for name, cursor := range transactions.named.cursors {
		if cursor.transaction != transactions.transaction {
			continue
		}
		if committed && cursor.holdable && cursor.sealed {
			cursor.transaction = nil
			continue
		}
		delete(transactions.named.cursors, name)
		transactions.named.release(cursor.metadataBytes)
		err = errors.Join(err, cursor.close())
	}
	return err
}

func (cursor *namedCursor) executionContext(ctx context.Context) context.Context {
	if cursor.scope == nil {
		return ctx
	}
	cursor.scope.active.Store(true)
	return context.WithValue(ctx, preparedScopeKey{}, cursor.scope)
}

func (cursor *namedCursor) materialize(ctx context.Context) (err error) {
	if cursor.sealed {
		return nil
	}
	if cursor.portal == nil || cursor.spool == nil {
		return errPreparedInvocation
	}
	call := cursor.executionContext(ctx)
	defer cursor.scope.active.Store(false)
	batch := columnar.Batch{MaxRows: cursor.batchRows}
	for {
		nextErr := cursor.portal.Next(call, &batch)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		if _, appendErr := cursor.spool.AppendBatch(call, &batch); appendErr != nil {
			return appendErr
		}
	}
	err = cursor.closeSource()
	if sealErr := cursor.spool.Seal(call); sealErr != nil {
		return errors.Join(err, sealErr)
	}
	cursor.sealed = true
	return err
}

func (transactions *Transactions) fetchCursor(ctx context.Context, command bind.FetchCursorCommand, options PreparedExecution, outer *Prepared, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	cursor := transactions.named.cursors[command.Name]
	if cursor == nil {
		return result, &model.Error{Code: "34000", Message: "cursor does not exist"}
	}
	automatic := !transactions.active
	if automatic {
		if err = transactions.begin(ctx, TransactionStart{}); err != nil {
			return result, err
		}
	}
	if err = transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return result, err
	}
	if cursor.transaction != nil && cursor.transaction != transactions.transaction {
		err = &model.Error{Code: "34000", Message: "cursor does not exist in this transaction"}
	} else {
		err = transactions.routineAtomicStatement(ctx, func(statementContext context.Context) error {
			source, sourceErr := cursor.newFetchSource(statementContext, command)
			if sourceErr != nil {
				return sourceErr
			}
			rows := &Rows{source: source, ctx: statementContext}
			defer func() {
				rows.mu.Lock()
				rows.closed = true
				rows.source = nil
				rows.ctx = nil
				rows.mu.Unlock()
			}()
			if command.Move {
				batch := columnar.Batch{MaxRows: cursor.batchRows}
				for {
					nextErr := source.Next(statementContext, &batch)
					if nextErr == io.EOF {
						return nil
					}
					if nextErr != nil {
						return nextErr
					}
				}
			}
			if consume == nil {
				return errPreparedInvocation
			}
			consumerErr := consume(statementContext, cursor.prepared, rows)
			rows.mu.Lock()
			if rows.terminal != nil && rows.terminal != io.EOF {
				consumerErr = errors.Join(consumerErr, rows.terminal)
			}
			rows.mu.Unlock()
			return consumerErr
		})
	}
	err = errors.Join(err, transactions.markDataAccess())
	moved := cursor.lastMoved
	cursor.lastMoved = 0
	verb := "FETCH"
	if command.Move {
		verb = "MOVE"
		result.RowsAffected = moved
	}
	result.Tag = fmt.Sprintf("%s %d", verb, moved)
	if outer != nil {
		result.Warnings = append(result.Warnings, outer.Warnings()...)
	}
	if err != nil {
		transactions.markFailed(err)
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(ctx, false)
		if err != nil && transactions.active && transactions.transaction.State() != txn.Indeterminate {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}
	return result, err
}

func fetchMagnitude(value int64) uint64 {
	if value >= 0 {
		return uint64(value)
	}
	return uint64(-(value + 1)) + 1
}

// lastMoved is updated only after rows are published or deliberately moved.
func (cursor *namedCursor) newFetchSource(ctx context.Context, command bind.FetchCursorCommand) (*cursorFetchSource, error) {
	cursor.lastMoved = 0
	direction, count := command.Direction, command.Count
	if direction == zpgparse.FetchDirection_FETCH_DIRECTION_UNDEFINED {
		direction = zpgparse.FetchDirection_FETCH_FORWARD
	}
	if !cursor.scrollable && (direction != zpgparse.FetchDirection_FETCH_FORWARD || count < 0) {
		return nil, &model.Error{Code: "55000", Message: "cursor can only scan forward"}
	}
	if direction == zpgparse.FetchDirection_FETCH_FORWARD && count > 0 && !cursor.sealed {
		return &cursorFetchSource{cursor: cursor, live: true, remaining: uint64(count), move: command.Move}, nil
	}
	if !cursor.sealed {
		if err := cursor.materialize(ctx); err != nil {
			return nil, err
		}
	}
	total := int64(cursor.spool.Rows())
	source := &cursorFetchSource{cursor: cursor, move: command.Move}
	switch direction {
	case zpgparse.FetchDirection_FETCH_FORWARD:
		if count < 0 {
			source.setBackward(total, fetchMagnitude(count))
		} else if count == 0 {
			source.setCurrent(total)
		} else {
			source.setForward(total, uint64(count))
		}
	case zpgparse.FetchDirection_FETCH_BACKWARD:
		if count < 0 {
			source.setForward(total, fetchMagnitude(count))
		} else if count == 0 {
			source.setCurrent(total)
		} else {
			source.setBackward(total, uint64(count))
		}
	case zpgparse.FetchDirection_FETCH_ABSOLUTE:
		target := count
		if count < 0 {
			target = total + 1 + count
		}
		source.setSingle(total, target)
	case zpgparse.FetchDirection_FETCH_RELATIVE:
		target := cursor.position + count
		if count > 0 && target < cursor.position {
			target = total + 1
		}
		if count < 0 && target > cursor.position {
			target = 0
		}
		source.setSingle(total, target)
	default:
		return nil, errPreparedInvocation
	}
	return source, nil
}

type cursorFetchSource struct {
	cursor    *namedCursor
	next      int64
	step      int64
	remaining uint64
	boundary  int64
	live      bool
	move      bool
}

func (source *cursorFetchSource) setCurrent(total int64) {
	source.boundary = source.cursor.position
	if source.cursor.position >= 1 && source.cursor.position <= total {
		source.next, source.step, source.remaining = source.cursor.position, 1, 1
	}
}

func (source *cursorFetchSource) setSingle(total, target int64) {
	source.step = 1
	switch {
	case target < 1:
		source.boundary = 0
	case target > total:
		source.boundary = total + 1
	default:
		source.next, source.remaining, source.boundary = target, 1, target
	}
}

func (source *cursorFetchSource) setForward(total int64, requested uint64) {
	source.step = 1
	start := source.cursor.position + 1
	if start < 1 {
		start = 1
	}
	available := uint64(0)
	if start <= total {
		available = uint64(total - start + 1)
	}
	source.next = start
	source.remaining = min(requested, available)
	source.boundary = start + int64(source.remaining) - 1
	if requested > available {
		source.boundary = total + 1
	}
}

func (source *cursorFetchSource) setBackward(total int64, requested uint64) {
	source.step = -1
	start := source.cursor.position - 1
	if source.cursor.position > total {
		start = total
	}
	available := uint64(0)
	if start >= 1 {
		available = uint64(start)
	}
	source.next = start
	source.remaining = min(requested, available)
	source.boundary = start - int64(source.remaining) + 1
	if requested > available {
		source.boundary = 0
	}
}

func (source *cursorFetchSource) Next(ctx context.Context, out *columnar.Batch) error {
	if out == nil || out.MaxRows < 0 {
		return errPreparedInvocation
	}
	if source.remaining == 0 {
		if !source.live {
			source.cursor.position = source.boundary
		}
		return io.EOF
	}
	maximum := out.MaxRows
	if maximum == 0 {
		maximum = source.cursor.batchRows
	}
	count := int(min(source.remaining, uint64(maximum)))
	if source.live {
		return source.nextLive(ctx, out, count)
	}
	builder, err := columnar.NewBuilder(source.cursor.layouts, columnar.BuilderOptions{MaxRows: count, MaxBytes: source.cursor.batchBytes})
	if err != nil {
		return err
	}
	defer builder.Close()
	if err := source.cursor.readRange(ctx, source.next, source.step, count, builder); err != nil {
		return err
	}
	if !source.move {
		if err := builder.Publish(out); err != nil {
			return err
		}
	} else {
		out.Reset()
	}
	source.next += source.step * int64(count)
	source.remaining -= uint64(count)
	source.cursor.lastMoved += uint64(count)
	if source.remaining == 0 {
		source.cursor.position = source.boundary
	} else {
		source.cursor.position = source.next - source.step
	}
	return nil
}

func (source *cursorFetchSource) nextLive(ctx context.Context, out *columnar.Batch, count int) error {
	call := source.cursor.executionContext(ctx)
	defer source.cursor.scope.active.Store(false)
	request := out.MaxRows
	batch := columnar.Batch{MaxRows: count}
	err := source.cursor.portal.Next(call, &batch)
	if err == io.EOF {
		closeErr := source.cursor.closeSource()
		sealErr := source.cursor.spool.Seal(call)
		source.cursor.sealed = sealErr == nil
		source.cursor.position = int64(source.cursor.spool.Rows()) + 1
		source.remaining = 0
		// Callers compare the end sentinel by identity; a joined io.EOF would
		// surface as an error. Close and seal failures replace it.
		if endErr := errors.Join(closeErr, sealErr); endErr != nil {
			return endErr
		}
		return io.EOF
	}
	if err != nil {
		return err
	}
	appended, err := source.cursor.spool.AppendBatch(call, &batch)
	if err != nil || appended != batch.Rows() {
		return errors.Join(err, errPreparedInvocation)
	}
	rows := uint64(batch.Rows())
	source.remaining -= rows
	source.cursor.position += int64(rows)
	source.cursor.lastMoved += rows
	if !source.move {
		*out = batch
		out.MaxRows = request
	} else {
		out.Reset()
		out.MaxRows = request
	}
	return nil
}

func (cursor *namedCursor) readRange(ctx context.Context, first, step int64, count int, builder *columnar.Builder) (err error) {
	if count <= 0 || (step != 1 && step != -1) {
		return errPreparedInvocation
	}
	reader, err := cursor.spool.Reader(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	positions := make([]rowstore.Position, count)
	last := first + step*int64(count-1)
	lower, upper := min(first, last), max(first, last)
	for ordinal := int64(1); ordinal <= upper; ordinal++ {
		position, _, nextErr := reader.NextRow(ctx)
		if nextErr != nil {
			return nextErr
		}
		if ordinal < lower {
			continue
		}
		index := ordinal - first
		if step < 0 {
			index = first - ordinal
		}
		positions[index] = position
	}
	for _, position := range positions {
		row, readErr := reader.Read(ctx, position)
		if readErr != nil {
			return readErr
		}
		if appendErr := builder.Append(ctx, row); appendErr != nil {
			return appendErr
		}
	}
	return nil
}
