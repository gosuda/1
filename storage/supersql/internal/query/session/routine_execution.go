package session

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type deferredRoutineTrigger struct {
	trigger       procedural.DeferredTrigger
	execution     PreparedExecution
	environment   exec.Environment
	sourceCatalog catalogrel.Scanner
	catalog       *catalog.Store
	options       RoutineExecutionOptions
	definitions   []procedural.Definition
}
type routineInvocationKey struct{}

type routineExecutor struct {
	transactions    *Transactions
	transaction     *txn.Transaction
	view            txn.ReadView
	scope           *preparedMutationScope
	environment     exec.Environment
	template        exec.Environment
	execution       PreparedExecution
	catalog         *catalog.Store
	sourceCatalog   catalogrel.Scanner
	settings        model.SettingsRuntime
	catalogSettings catalogrel.Settings
	baseFunctions   expr.Resolver
	baseBuiltin     builtin.ResolveOptions
	baseKeys        *builtin.KeyEncoder
	definitions     []procedural.Definition
	frame           *routineCapabilityFrame
	retiredFrames   []*routineCapabilityFrame
	mu              sync.Mutex
	nonAtomic       bool
	controlAllowed  bool
	routineDepth    int
	options         *RoutineExecutionOptions
}

func attachRoutineExecution(ctx context.Context, owner *Transactions, transaction *txn.Transaction, view txn.ReadView, scope *preparedMutationScope, environment *exec.Environment, sourceCatalog catalogrel.Scanner, store *catalog.Store, execution PreparedExecution, options *RoutineExecutionOptions, dependencies []model.Dependency, nonAtomic bool) (context.Context, func() error, error) {
	ctx, executor, err := attachRoutineExecutionState(ctx, owner, transaction, view, scope, environment, sourceCatalog, store, execution, options, dependencies, nonAtomic)
	if err != nil {
		return nil, nil, err
	}
	if executor == nil {
		return ctx, func() error { return nil }, nil
	}
	return ctx, executor.closeCapabilities, nil
}

func attachRoutineExecutionState(ctx context.Context, owner *Transactions, transaction *txn.Transaction, view txn.ReadView, scope *preparedMutationScope, environment *exec.Environment, sourceCatalog catalogrel.Scanner, store *catalog.Store, execution PreparedExecution, options *RoutineExecutionOptions, dependencies []model.Dependency, nonAtomic bool) (context.Context, *routineExecutor, error) {
	if options == nil {
		return ctx, nil, nil
	}
	if owner == nil || transaction == nil || view == nil || scope == nil || environment == nil || sourceCatalog == nil || store == nil || options.Planning == nil || options.NewQuery == nil {
		return nil, nil, errPreparedInvocation
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, nil, err
	}
	if err := options.Capabilities.validate(execution); err != nil {
		return nil, nil, err
	}
	catalogSettings, ok := environment.Settings.(catalogrel.Settings)
	if environment.Settings == nil || !ok {
		return nil, nil, errPreparedInvocation
	}
	template := *environment
	template.Query = nil
	template.Reader = nil
	template.CopySource = nil
	template.Parameters = nil
	template.Modify.Transaction = nil
	template.Modify.ConstraintTransaction = nil
	template.Modify.Deferral = nil
	configured := options.Capabilities
	executor := &routineExecutor{
		transactions: owner, transaction: transaction, view: view, scope: scope,
		environment: *environment, template: template, execution: execution,
		sourceCatalog: sourceCatalog, catalog: store, settings: environment.Settings, catalogSettings: catalogSettings,
		baseFunctions: configured.BaseFunctions, baseBuiltin: configured.BaseBuiltin,
		baseKeys: configured.BaseKeys, options: options, nonAtomic: nonAtomic,
	}
	definitions, err := loadRoutineDefinitions(ctx, store, view, owner.named.temporaryDescriptors(), dependencies, options.Limits.MaxStatements)
	if err != nil {
		return nil, nil, err
	}
	executor.definitions = definitions
	frame, err := executor.buildCapabilityFrame(ctx, view, false)
	if err != nil {
		return nil, nil, err
	}
	frame.borrowers = 1
	executor.installFrame(frame)
	applyRoutineCapabilities(environment, frame.environment)
	return procedural.WithSQLExecutor(ctx, executor), executor, nil
}

// loadRoutineDefinitions resolves the descriptors the statement depends on, so
// any session-temporary object it names resolves from the overlay rather than
// from the catalog the statement's reads are declared against.
func loadRoutineDefinitions(ctx context.Context, store *catalog.Store, view txn.ReadView, temporary []*model.Descriptor, dependencies []model.Dependency, limit int) (definitions []procedural.Definition, err error) {
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	if err := snapshot.AddSessionOverlay(temporary); err != nil {
		return nil, err
	}
	queue := make([]model.ObjectID, 0, len(dependencies))
	for _, dependency := range dependencies {
		queue = append(queue, dependency.Object)
	}
	return loadRoutineDefinitionsFromSnapshot(ctx, snapshot, queue, limit, false)
}

func reloadRoutineDefinitions(ctx context.Context, snapshot *catalog.CatalogSnapshot, definitions []procedural.Definition, limit int) ([]procedural.Definition, error) {
	queue := make([]model.ObjectID, 0, len(definitions))
	for _, definition := range definitions {
		queue = append(queue, definition.Object)
	}
	return loadRoutineDefinitionsFromSnapshot(ctx, snapshot, queue, limit, true)
}

func loadRoutineDefinitionsFromSnapshot(ctx context.Context, snapshot *catalog.CatalogSnapshot, queue []model.ObjectID, limit int, allowMissing bool) ([]procedural.Definition, error) {
	definitions := make([]procedural.Definition, 0, len(queue))
	seen := make(map[model.ObjectID]struct{}, len(queue))
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[0]
		queue = queue[1:]
		if id == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		if len(seen) > limit {
			return nil, procedural.ErrLimit
		}
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			if allowMissing && errors.Is(err, catalog.ErrUnknownObject) {
				continue
			}
			return nil, err
		}
		descriptor := published.Descriptor
		spec, err := descriptor.Spec(ctx)
		if err != nil {
			return nil, err
		}
		if spec.Table != nil {
			for _, trigger := range spec.Table.Triggers {
				if trigger.Routine != 0 {
					queue = append(queue, trigger.Routine)
				}
			}
		}
		if spec.View != nil {
			for _, trigger := range spec.View.Triggers {
				if trigger.Routine != 0 {
					queue = append(queue, trigger.Routine)
				}
			}
		}
		if spec.Routine == nil {
			continue
		}
		if spec.Routine.Language != "sql" && spec.Routine.Language != "plpgsql" {
			continue
		}
		if spec.OID == 0 {
			return nil, procedural.ErrInvalidProgram
		}
		definitions = append(definitions, procedural.Definition{ID: model.FunctionID(spec.OID), Object: spec.ID, Name: spec.Name.Name, Metadata: *spec.Routine})
		for _, dependency := range spec.Routine.Body.Dependencies {
			queue = append(queue, dependency.Object)
		}
	}
	return definitions, nil
}

func (executor *routineExecutor) RunRoutine(ctx context.Context, invocation procedural.RoutineInvocation, run func(context.Context) (procedural.SQLResult, error)) (result procedural.SQLResult, err error) {
	if run == nil {
		return result, errPreparedInvocation
	}
	invoke := func(invocationCtx context.Context) (result procedural.SQLResult, err error) {
		previousAllowed := executor.controlAllowed
		executor.controlAllowed = executor.nonAtomic && invocation.Procedure && !invocation.Atomic && !invocation.SecurityDefiner && len(invocation.Settings) == 0 && (executor.routineDepth == 0 || previousAllowed)
		executor.routineDepth++
		defer func() {
			executor.routineDepth--
			executor.controlAllowed = previousAllowed
		}()
		if len(invocation.Settings) != 0 {
			beforeCurrent, beforeRetained := executor.settingSnapshot()
			defer func() {
				executor.restoreSettings(beforeCurrent, beforeRetained)
				err = errors.Join(err, executor.replaceFrame(context.WithoutCancel(invocationCtx), executor.view, executor.frame.ownedView))
			}()
			for _, setting := range invocation.Settings {
				executor.transactions.settingMu.Lock()
				var changed SettingResult
				var settingErr error
				if setting.Reset {
					changed, settingErr = executor.transactions.settings.Reset(invocationCtx, setting.Name, SessionScope)
				} else {
					changed, settingErr = executor.transactions.settings.Set(invocationCtx, setting.Name, setting.Value, SessionScope)
				}
				executor.transactions.settingMu.Unlock()
				if settingErr != nil {
					return procedural.SQLResult{}, settingErr
				}
				if changed.Warning != nil {
					executor.Report(invocationCtx, *changed.Warning)
				}
			}
			if err := executor.replaceFrame(invocationCtx, executor.view, executor.frame.ownedView); err != nil {
				return procedural.SQLResult{}, err
			}
		}
		result, runErr := run(invocationCtx)
		if runErr != nil {
			return result, runErr
		}
		for _, row := range result.Rows {
			if len(row) != 1 {
				return procedural.SQLResult{}, settingError("42804", "returned row does not match routine result type")
			}
			if valueErr := executor.ValidateRoutineValue(invocationCtx, invocation.Result, row[0]); valueErr != nil {
				return procedural.SQLResult{}, valueErr
			}
		}
		return result, nil
	}
	if ctx.Value(routineInvocationKey{}) == executor {
		return invoke(ctx)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return invoke(context.WithValue(ctx, routineInvocationKey{}, executor))
}
func (executor *routineExecutor) ValidateRoutineValue(ctx context.Context, layout model.TypeLayout, value model.Value) error {
	if err := executor.scope.check(ctx); err != nil {
		return err
	}
	resources := executor.resources()
	outputBytes := executor.options.Limits.MaxResultBytes
	if outputBytes > math.MaxInt64 {
		outputBytes = math.MaxInt64
	}
	return builtin.ValidateResolvedValue(ctx, executor.environment.Builtin, layout, value, builtin.CallBudget{
		OutputBytes: int64(outputBytes), MemoryBytes: uint64(resources.EffectiveWorkMemoryBytes),
		Temporal: executor.environment.Temporal, Relations: executor.environment.Relations,
		Settings: executor.environment.Settings, TextSearch: executor.environment.TextSearch,
		Sequences: executor.environment.Sequences, Advisory: executor.environment.Advisory,
	}, executor.options.Limits.MaxDepth)
}

func (executor *routineExecutor) ExecuteSQL(ctx context.Context, request procedural.SQLRequest) (result procedural.SQLResult, err error) {
	if err := executor.scope.check(ctx); err != nil {
		return result, err
	}
	moment := readSQLMoment(ctx, executor.transactions.temporalClock)
	executor.transactions.temporal.statement = moment
	executor.environment.Temporal = executor.transactions.temporal
	executor.frame.environment.Temporal = executor.transactions.temporal
	allowControl := executor.nonAtomic && executor.controlAllowed && request.AllowTransactionControl
	if executor.scope.ambient || allowControl {
		return executor.executeSQL(ctx, request, allowControl)
	}
	err = executor.transaction.Statement(ctx, func(statement context.Context) (runErr error) {
		executor.scope.ambient = true
		defer func() { executor.scope.ambient = false }()
		result, runErr = executor.executeSQL(statement, request, false)
		return runErr
	})
	return result, err
}

func (executor *routineExecutor) executeSQL(ctx context.Context, request procedural.SQLRequest, allowControl bool) (result procedural.SQLResult, err error) {
	if err := executor.scope.check(ctx); err != nil {
		return result, err
	}
	if request.Statement == nil || request.MaxRows <= 0 || request.MaxBytes == 0 {
		return result, errPreparedInvocation
	}
	if !allowControl && routineTypedStatement(request.Statement) {
		return executor.executeReenteredStatement(ctx, request)
	}
	var checkBatch func(context.Context, *columnar.Batch) error
	hadReturning := true
	if executor.options.RewriteStatement != nil {
		var rewritten *zpgparse.RawStmt
		rewritten, checkBatch, hadReturning, err = executor.options.RewriteStatement(ctx, request.Statement)
		if err != nil {
			return result, err
		}
		if rewritten != nil {
			request.Statement = rewritten
		}
	}
	parameterLayouts := make([]model.TypeLayout, len(request.Parameters))
	for i := range request.Parameters {
		parameterLayouts[i] = request.Parameters[i].Layout
	}
	previousAllowed := executor.controlAllowed
	executor.controlAllowed = previousAllowed && allowControl
	defer func() { executor.controlAllowed = previousAllowed }()
	beforeCurrent, beforeRetained := executor.settingSnapshot()
	defer func() {
		if err != nil && !allowControl {
			executor.restoreSettings(beforeCurrent, beforeRetained)
		}
	}()
	snapshot, err := executor.catalog.TransactionSnapshot(executor.view, catalog.BorrowReader)
	if err != nil {
		return result, err
	}
	var bound *bind.BoundStatement
	var planning PrepareOptions
	planning, err = executor.options.Planning(ctx, snapshot, executor.view, request.Statement, parameterLayouts)
	if err == nil {
		resources, searchPath := executor.semanticSettings()
		planning.Normalize.Builtin = executor.environment.Builtin
		planning.Bind.Builtin = executor.environment.Builtin
		planning.Bind.CatalogRelations = executor.options.Capabilities.CatalogRelations
		planning.Bind.Temporal = executor.environment.Temporal
		planning.Bind.SearchPath = searchPath
		planning.Optimize.WorkingMemory = min(planning.Optimize.WorkingMemory, uint64(resources.EffectiveWorkMemoryBytes))
		boundRoutineStatementSource(&planning, request.Statement, executor.options.Limits.MaxProgramBytes)
		bound, err = bind.Bind(ctx, request.Statement, snapshot, planning.Bind, parameterLayouts)
	}
	var inline *procedural.Program
	var inlineDependencies []model.Dependency
	if err == nil && bound != nil {
		if command, ok := bound.Command().(bind.InlineRoutineCommand); ok {
			inline, inlineDependencies, err = (procedural.Compiler{Limits: executor.options.Limits}).CompileInline(ctx, command.Statement, snapshot, planning.Bind)
		}
	}
	closeErr := snapshot.Close()
	if err != nil || closeErr != nil {
		return result, errors.Join(err, closeErr)
	}
	if bound == nil {
		return result, errPreparedInvocation
	}
	if inline == nil && bound.Command() != nil {
		return result, settingError("0A000", "transaction and session commands are not allowed inside routines")
	}
	if allowControl && !bound.ProcedureCall() && inline == nil {
		return result, procedural.ErrInvalidProgram
	}
	dependencies := bound.Dependencies()
	dependencies = append(dependencies, inlineDependencies...)
	definitions, err := loadRoutineDefinitions(ctx, executor.catalog, executor.view, executor.transactions.named.temporaryDescriptors(), dependencies, executor.options.Limits.MaxStatements)
	if err != nil {
		return result, err
	}
	if len(definitions) != 0 {
		if err := executor.installDefinitions(ctx, definitions); err != nil {
			return result, err
		}
	}
	if inline != nil {
		return inline.Execute(ctx, nil)
	}
	query, err := executor.options.NewQuery(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, query.Close()) }()
	resources := executor.resources()
	if err := query.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); err != nil {
		return result, err
	}
	// Binding ran under the frame view, whose declaration covers catalog
	// discovery and the enclosing statement's own scans — never this body
	// statement's data spans. Optimization negotiates access through real
	// PlanScan calls, so the widened declaration must exist before planning;
	// one derived view then covers both planning and execution.
	reader, err := executor.openRoutineStatementView(ctx, bound, request.Statement, parameterLayouts)
	if err != nil {
		return result, err
	}
	planningSnapshot, err := executor.catalog.TransactionSnapshot(reader, catalog.BorrowReader)
	if err != nil {
		return result, errors.Join(err, reader.Close())
	}
	var prepared *Prepared
	planning, err = executor.options.Planning(ctx, planningSnapshot, reader, request.Statement, parameterLayouts)
	if err == nil {
		resources, searchPath := executor.semanticSettings()
		planning.Normalize.Builtin = executor.environment.Builtin
		planning.Bind.Builtin = executor.environment.Builtin
		planning.Bind.CatalogRelations = executor.options.Capabilities.CatalogRelations
		planning.Bind.Temporal = executor.environment.Temporal
		planning.Bind.SearchPath = searchPath
		planning.Optimize.WorkingMemory = min(planning.Optimize.WorkingMemory, uint64(resources.EffectiveWorkMemoryBytes))
		boundRoutineStatementSource(&planning, request.Statement, executor.options.Limits.MaxProgramBytes)
		prepared, err = prepareBound(ctx, bound, planning)
	}
	err = errors.Join(err, planningSnapshot.Close())
	if err != nil {
		return result, errors.Join(err, reader.Close())
	}
	if prepared == nil || prepared.bound == nil || prepared.graph == nil {
		return result, errors.Join(errPreparedInvocation, reader.Close())
	}
	environment := executor.environment
	environment.Query = query
	environment.BorrowQuery = false
	environment.Reader = reader
	environment.BorrowReader = false
	environment.Catalog, err = attachCatalogView(executor.environment.Catalog, reader)
	if err != nil {
		return result, errors.Join(err, reader.Close())
	}
	environment.Native.Database, _ = environment.Catalog.(model.DatabaseRuntime)
	environment.Modify.Transaction = executor.scope
	environment.Modify.Deferral = executor.transactions
	environment.Parameters = make([]expr.ParameterValue, len(request.Parameters))
	for i, parameter := range request.Parameters {
		environment.Parameters[i] = expr.ParameterValue{Number: uint32(i + 1), Layout: parameter.Layout, Value: parameter.Value}
	}
	portalContext := ctx
	frame := executor.retainFrame()
	defer func() { err = errors.Join(err, executor.releaseFrame(frame)) }()
	if executor.scope.ambient {
		portalContext = context.WithValue(ctx, routineStatementKey{}, executor.scope)
	}
	portal, err := prepared.Open(portalContext, environment)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, portal.Close()) }()
	columns := prepared.Columns()
	if checkBatch != nil {
		if !hadReturning {
			columns = nil
		} else if len(columns) > 0 {
			columns = columns[:len(columns)-1]
		}
	}
	result.Columns = make([]model.TypeLayout, len(columns))
	result.ColumnNames = make([]string, len(columns))
	for i := range columns {
		result.Columns[i] = columns[i].Layout
		result.ColumnNames[i] = columns[i].Name
	}
	used := uint64(len(result.Columns)) * uint64(unsafe.Sizeof(model.TypeLayout{}))
	if used > request.MaxBytes {
		return result, procedural.ErrLimit
	}
	for {
		remaining := request.MaxRows - len(result.Rows)
		batchLimit := min(remaining+1, columnar.DefaultBatchSize)
		if checkBatch != nil && !hadReturning {
			batchLimit = columnar.DefaultBatchSize
		}
		batch := columnar.Batch{MaxRows: batchLimit}
		nextErr := portal.Next(ctx, &batch)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return result, nextErr
		}
		if checkBatch != nil {
			if err := checkBatch(ctx, &batch); err != nil {
				return result, err
			}
			if !hadReturning {
				continue
			}
		}
		for rowIndex := range batch.Rows() {
			if len(result.Rows) >= request.MaxRows {
				return result, procedural.ErrLimit
			}
			header := uint64(len(batch.Columns)) * uint64(unsafe.Sizeof(model.Value{}))
			if header > request.MaxBytes-used {
				return result, procedural.ErrLimit
			}
			used += header
			row := make([]model.Value, len(batch.Columns))
			lane := batch.Lane(rowIndex)
			for columnIndex := range batch.Columns {
				value, size, copyErr := columnar.CopyValue(ctx, &batch.Columns[columnIndex], lane, request.MaxBytes-used)
				if copyErr != nil {
					return result, errors.Join(copyErr, procedural.ErrLimit)
				}
				used += size
				row[columnIndex] = value
			}
			result.Rows = append(result.Rows, row)
		}
	}
	if preparedWrites(prepared) {
		if err := portal.FinishWrites(ctx); err != nil {
			return result, err
		}
	}
	result.RowsAffected = portal.RowsAffected()
	return result, nil
}

// openRoutineStatementView derives the read set for one bound routine-body
// statement and opens the view covering it, mirroring the derived-dependency
// path in runPreparedStatement: the enclosing declaration plus every access
// path the bound statement's scans may negotiate, so optimization's PlanScan
// probes and the portal's reads are both covered. Catalog layout coverage is
// guaranteed before the view opens, matching the binding-view fallback.
func (executor *routineExecutor) openRoutineStatementView(ctx context.Context, bound *bind.BoundStatement, statement *zpgparse.RawStmt, parameterLayouts []model.TypeLayout) (reader txn.ReadView, err error) {
	scans, err := executor.transactions.statementScanDependencies(ctx, executor.view, bound, executor.execution, func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error) {
		snapshot, err := executor.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return nil, PrepareOptions{}, err
		}
		plan, err := executor.options.Planning(ctx, snapshot, view, statement, parameterLayouts)
		if err != nil {
			return nil, PrepareOptions{}, errors.Join(err, snapshot.Close())
		}
		return snapshot, plan, nil
	}, executor.environment.Builtin)
	if err != nil {
		return nil, err
	}
	dependencies := append(slices.Clone(executor.execution.Dependencies), scans...)
	if len(catalogReadDependencies(executor.catalog, dependencies)) == 0 {
		dependencies = append(catalogLayoutDependencies(executor.catalog), dependencies...)
	}
	dependencies = bindScanOccurrences(bound, dependencies)
	return executor.transaction.ReadViewForRowLocks(ctx, dependencies, boundRowLockPolicies(bound))
}

func boundRoutineStatementSource(planning *PrepareOptions, statement *zpgparse.RawStmt, maximum uint64) {
	if planning == nil || statement == nil || statement.Stmt == nil {
		return
	}
	extent := uint64(statement.StmtLocation) + uint64(statement.StmtLen)
	if extent == 0 {
		// A PL/pgSQL body statement is produced by the routine compiler and has no
		// location within any SQL text, so no span is meaningful. The program byte
		// budget is the bound instead; the IR builder only requires a non-zero one.
		extent = maximum
	}
	if extent == 0 {
		extent = uint64(math.MaxUint32)
	}
	if extent > uint64(math.MaxUint32) {
		extent = uint64(math.MaxUint32)
	}
	source := uint32(extent)
	if planning.Bind.Limits.SourceBytes == 0 {
		planning.Bind.Limits.SourceBytes = source
	}
	if planning.Normalize.Limits.SourceBytes == 0 {
		planning.Normalize.Limits.SourceBytes = source
	}
	if planning.Optimize.IR.SourceBytes == 0 {
		planning.Optimize.IR.SourceBytes = source
	}
}

func (executor *routineExecutor) TransactionControl(ctx context.Context, command procedural.TransactionCommand) error {
	if err := executor.scope.check(ctx); err != nil {
		return err
	}
	if !executor.nonAtomic || !executor.controlAllowed || executor.scope.ambient || executor.routineDepth == 0 {
		return settingError("2D000", "invalid transaction termination")
	}
	var err error
	if command.Rollback {
		err = executor.transactions.rollback(ctx, command.Chain)
	} else {
		_, err = executor.transactions.commit(ctx, command.Chain)
	}
	if err != nil {
		return err
	}
	if !executor.transactions.active {
		if err := executor.transactions.begin(ctx, TransactionStart{}); err != nil {
			return err
		}
	}
	transaction := executor.transactions.transaction
	if transaction == nil || transaction.State() != txn.Active {
		return errSessionTransactionState
	}
	// The replacement view carries the statements that follow this COMMIT. They are
	// bound against this view, and binding resolves catalog objects by name, so the
	// declaration must also cover the discovery scans binding performs. The outer
	// CALL's dependencies describe the CALL itself, not the routine body.
	dependencies, err := catalogDiscoveryDependencies(ctx, transaction, executor.catalog, catalogDiscovery{}, executor.execution.Dependencies, executor.environment.Limits.MetadataBytes)
	if err != nil {
		return err
	}
	view, err := transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	if err := executor.replaceFrame(ctx, view, true); err != nil {
		return err
	}
	return nil
}

func (executor *routineExecutor) RunSubtransaction(ctx context.Context, run func(context.Context) error) error {
	if err := executor.scope.check(ctx); err != nil {
		return err
	}
	advisoryMark, err := executor.transactions.advisoryStatementMark()
	if err != nil {
		return err
	}
	deferredMark := len(executor.transactions.deferredTriggers)
	deferredConstraintMark := len(executor.transactions.deferredConstraints)
	deferredConstraintBytes := executor.transactions.deferredConstraintBytes
	beforeCurrent, beforeRetained := executor.settingSnapshot()
	beforeDefinitions := append([]procedural.Definition(nil), executor.definitions...)
	previousAllowed := executor.controlAllowed
	executor.controlAllowed = false
	defer func() { executor.controlAllowed = previousAllowed }()
	runNested := func(statement context.Context) error {
		return executor.transaction.RunSubtransaction(context.WithValue(statement, routineStatementKey{}, executor.scope), run)
	}
	if executor.scope.ambient {
		err = runNested(ctx)
	} else {
		err = executor.transaction.Statement(ctx, func(statement context.Context) error {
			executor.scope.ambient = true
			defer func() { executor.scope.ambient = false }()
			return runNested(statement)
		})
	}
	if err != nil {
		executor.restoreSettings(beforeCurrent, beforeRetained)
		clear(executor.definitions)
		executor.definitions = beforeDefinitions
	}
	if err != nil && deferredMark <= len(executor.transactions.deferredTriggers) {
		clear(executor.transactions.deferredTriggers[deferredMark:])
		executor.transactions.deferredTriggers = executor.transactions.deferredTriggers[:deferredMark]
	}
	if err != nil && deferredConstraintMark <= len(executor.transactions.deferredConstraints) {
		clear(executor.transactions.deferredConstraints[deferredConstraintMark:])
		executor.transactions.deferredConstraints = executor.transactions.deferredConstraints[:deferredConstraintMark]
		executor.transactions.deferredConstraintBytes = deferredConstraintBytes
	}
	if err != nil {
		err = errors.Join(err, executor.transactions.rollbackAdvisoryStatement(advisoryMark))
		err = errors.Join(err, executor.replaceFrame(context.WithoutCancel(ctx), executor.view, executor.frame.ownedView))
	}
	return err
}
func (executor *routineExecutor) DeferTrigger(ctx context.Context, trigger procedural.DeferredTrigger) error {
	if err := executor.scope.check(ctx); err != nil {

		return err
	}
	if trigger.Program == nil || len(executor.transactions.deferredTriggers) >= executor.options.Limits.MaxDeferredEvents {
		return procedural.ErrLimit
	}
	owned, err := cloneDeferredTrigger(ctx, trigger, executor.options.Limits.MaxResultBytes)
	if err != nil {
		return err
	}
	environment := executor.template
	execution := executor.execution
	execution.Environment = environment
	execution.Routines = nil
	executor.transactions.deferredTriggers = append(executor.transactions.deferredTriggers, deferredRoutineTrigger{
		trigger: owned, execution: execution, environment: environment,
		sourceCatalog: executor.sourceCatalog, catalog: executor.catalog, options: *executor.options,
		definitions: append([]procedural.Definition(nil), executor.definitions...),
	})
	return nil
}

func (executor *routineExecutor) InvokeNativeTrigger(ctx context.Context, function model.FunctionID, trigger procedural.TriggerContext) (*model.Row, bool, error) {
	if err := executor.scope.check(ctx); err != nil {
		return nil, false, err
	}
	columns := make([]model.ColumnDescriptor, len(trigger.Layout.Children))
	for i, layout := range trigger.Layout.Children {
		name := ""
		if i < len(trigger.Layout.FieldNames) {
			name = trigger.Layout.FieldNames[i]
		}
		columns[i] = model.ColumnDescriptor{ID: model.ColumnID(i + 1), Name: name, Layout: layout, State: model.StatePublic}
	}
	event := model.TriggerEvent(0)
	switch trigger.Event {
	case "insert":
		event = model.TriggerInsert
	case "update":
		event = model.TriggerUpdate
	case "delete":
		event = model.TriggerDelete
	case "truncate":
		event = model.TriggerTruncate
	default:
		return nil, false, procedural.ErrInvalidProgram
	}
	return builtin.InvokeFullTextTrigger(ctx, function, builtin.FullTextTriggerContext{Columns: columns, Arguments: trigger.Arguments, Event: event, New: trigger.New, Settings: executor.environment.Settings, TextSearch: executor.environment.TextSearch, MaxBytes: executor.options.Limits.MaxResultBytes})
}
func cloneDeferredTrigger(ctx context.Context, source procedural.DeferredTrigger, limit uint64) (procedural.DeferredTrigger, error) {
	target := source
	target.Context.Arguments = slices.Clone(source.Context.Arguments)
	layout, size, err := model.CopyTypeLayout(ctx, source.Context.Layout, limit)
	if err != nil {
		return procedural.DeferredTrigger{}, err
	}
	target.Context.Layout = layout
	remaining := limit - size
	copyRow := func(source *model.Row) (*model.Row, error) {
		if source == nil {
			return nil, nil
		}
		target := &model.Row{Key: slices.Clone(source.Key), Values: make([]model.Value, len(source.Values))}
		if uint64(len(target.Key)) > remaining {
			return nil, procedural.ErrLimit
		}
		remaining -= uint64(len(target.Key))
		for i, value := range source.Values {
			owned, bytes, err := model.CopyValue(ctx, value, remaining)
			if err != nil {
				return nil, err
			}
			target.Values[i] = owned
			remaining -= bytes
		}
		return target, nil
	}
	target.Context.Old, err = copyRow(source.Context.Old)
	if err != nil {
		return procedural.DeferredTrigger{}, err
	}
	target.Context.New, err = copyRow(source.Context.New)
	if err != nil {
		return procedural.DeferredTrigger{}, err
	}
	copyRows := func(source []model.Row) ([]model.Row, error) {
		target := make([]model.Row, len(source))
		for i := range source {
			row, err := copyRow(&source[i])
			if err != nil {
				return nil, err
			}
			target[i] = *row
		}
		return target, nil
	}
	target.Context.OldTransition, err = copyRows(source.Context.OldTransition)
	if err != nil {
		return procedural.DeferredTrigger{}, err
	}
	target.Context.NewTransition, err = copyRows(source.Context.NewTransition)
	if err != nil {
		return procedural.DeferredTrigger{}, err
	}
	return target, nil
}

func (executor *routineExecutor) Report(ctx context.Context, warning model.Error) {
	if executor.options.Report == nil || ctx.Err() != nil {
		return
	}
	executor.options.Report(ctx, warning)
}

func (executor *routineExecutor) settingSnapshot() (current, retained settingValues) {
	executor.transactions.settingMu.Lock()
	current, retained = executor.transactions.settings.current, executor.transactions.settings.retained
	executor.transactions.settingMu.Unlock()
	return current, retained
}

func (executor *routineExecutor) restoreSettings(current, retained settingValues) {
	executor.transactions.settingMu.Lock()
	executor.transactions.settings.current, executor.transactions.settings.retained = current, retained
	executor.transactions.settingMu.Unlock()
}

func (executor *routineExecutor) resources() ResourcePolicy {
	executor.transactions.settingMu.Lock()
	resources := executor.transactions.settings.Resources()
	executor.transactions.settingMu.Unlock()
	return resources
}

func (executor *routineExecutor) semanticSettings() (ResourcePolicy, []string) {
	executor.transactions.settingMu.Lock()
	resources := executor.transactions.settings.Resources()
	searchPath := executor.transactions.settings.SearchPath()
	executor.transactions.settingMu.Unlock()
	return resources, searchPath
}
