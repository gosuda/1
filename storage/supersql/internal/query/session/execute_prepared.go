package session

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	qexplain "github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

var errPreparedInvocation = &model.Error{Code: "22023", Message: "invalid prepared session invocation"}
var errPreparedDependency = &model.Error{Code: "0A000", Message: "prepared query catalog dependency changed; prepare again"}

// PreparedExecution borrows Catalog and Dependencies. For RunPrepared, nil
// Dependencies derives descriptors and declared physical scan spans from the plan,
// including secondary-index protection; an explicit slice must cover all reads.
// RunStatement needs binding dependencies supplied explicitly. Environment transfers
// Query on every outcome. Reader and Modify's transaction/constraint ownership
// collaborators must be empty: the session supplies them from its transaction.
type RoutineExecutionOptions struct {
	Planning         func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader, *zpgparse.RawStmt, []model.TypeLayout) (PrepareOptions, error)
	NewQuery         func(context.Context) (*runtime.Query, error)
	Report           func(context.Context, model.Error)
	Limits           procedural.Limits
	Capabilities     RoutineTransactionCapabilities
	RewriteStatement func(ctx context.Context, raw *zpgparse.RawStmt) (*zpgparse.RawStmt, func(ctx context.Context, batch *columnar.Batch) error, bool, error)
}

// SchemaCommandExecution borrows one binding snapshot and copies the parser's
// immutable ONLINE sidecar before dispatch. Service options are database-owned.
type SchemaCommandExecution struct {
	Snapshot        bind.CatalogSnapshot
	Online          *zpgparse.OnlineDDLClause
	AlterDrop       OnlineSchemaOptions
	ConcurrentIndex ConcurrentIndexOptions
	StatementIndex  int
}

// ExplainExecutionOptions bounds one atomically staged report. The session
// creates all mutable instrumentation per EXPLAIN ANALYZE invocation.
type ExplainExecutionOptions struct {
	Limits qexplain.Limits
}

type PreparedExecution struct {
	Catalog      *catalog.Store
	Dependencies []txn.ReadDependency
	Environment  exec.Environment
	// Copy borrows one bounded direction-appropriate caller stream for the
	// invocation. Session execution never closes or flushes it.
	Copy                  *qcopy.Streams
	Routines              *RoutineExecutionOptions
	RoutineDDL            *RoutineCommandOptions
	TextSearchDDL         *TableCommandOptions
	TypeDDL               *TypeCommandOptions
	ObjectDDL             *ObjectCommandOptions
	Statistics            *StatisticsCommandOptions
	TypeRuntime           *DeclaredTypeRuntimeOptions
	Explain               *ExplainExecutionOptions
	Schema                *SchemaCommandExecution
	statementPlanner      func(context.Context, txn.ReadView, builtin.ResolveOptions, *zpgparse.RawStmt, []model.TypeLayout) (*Prepared, error)
	analyzedPlanner       func(context.Context, txn.ReadView, builtin.ResolveOptions, *Prepared, []expr.ParameterValue) (*Prepared, error)
	parameterTypeResolver func(context.Context, txn.ReadView, builtin.ResolveOptions, []*zpgparse.Node) ([]model.TypeLayout, error)
	argumentPlanner       func(context.Context, txn.ReadView, builtin.ResolveOptions, []*zpgparse.Node, []model.TypeLayout) (*Prepared, error)
	planningOptions       func(context.Context, txn.ReadView, builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error)
	explain               *explainInvocation
}

type ExecutionResult struct {
	// RowsAffected counts staged mutations; Commit distinguishes durable autocommit.
	RowsAffected uint64
	Commit       EndResult
	Tag          string
	Warnings     []model.Error
	Job          schema.Job
}

// Rows is borrowed only during RunPrepared's consumer. Next contexts control one
// fetch; transaction and statement capabilities remain bound to the invocation.
// The consumer must not call other Transactions methods or retain Rows/batches.
type rowsSource interface {
	Next(context.Context, *columnar.Batch) error
}

type Rows struct {
	mu       sync.Mutex
	portal   *exec.Portal
	command  *commandCursor
	source   rowsSource
	ctx      context.Context
	terminal error
	closed   bool
}

func (rows *Rows) Next(ctx context.Context, out *columnar.Batch) error {
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if out != nil {
		out.Reset()
	}
	if rows.closed {
		return storage.ErrClosed
	}
	if rows.terminal != nil {
		return rows.terminal
	}
	if err := ctx.Err(); err != nil {
		rows.terminal = err
		return err
	}
	call, cancel := context.WithCancel(rows.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	var err error
	switch {
	case rows.command != nil:
		err = rows.command.Next(call, out)
	case rows.source != nil:
		err = rows.source.Next(call, out)
	default:
		err = rows.portal.Next(call, out)
	}
	if err != nil {
		rows.terminal = err
	}
	return err
}

// RunPrepared serializes a complete API/protocol invocation, joining portal
// cleanup before commit. A directly prepared CALL may control transactions only
// in autocommit; SQL PREPARE has its narrower statement-kind contract. Other
// statements start/end an implicit transaction when no explicit block exists.
// Successful early consumption stops SELECT; writes are drained to completion.
// Consumer/fetch/cleanup failures roll back the statement and fail an explicit
// block. An indeterminate autocommit remains owned by Transactions.Resolve/Close.
func (transactions *Transactions) RunPrepared(ctx context.Context, prepared *Prepared, options PreparedExecution, consume func(context.Context, *Rows) error) (result ExecutionResult, err error) {
	var consumer func(context.Context, *Prepared, *Rows) error
	if consume != nil {
		consumer = func(ctx context.Context, _ *Prepared, rows *Rows) error {
			return consume(ctx, rows)
		}
	}
	nonAtomic := prepared != nil && prepared.bound != nil && prepared.bound.ProcedureCall()
	// The caller supplies this query and keeps it across statements; the session
	// only borrows it, so the portal and rejection paths must not close it.
	options.Environment.BorrowQuery = true
	result, err = transactions.runExecution(ctx, statementExecution{prepared: prepared, nonAtomic: nonAtomic}, options, consumer)
	if prepared != nil && prepared.Copy() != nil {
		result.Tag = "COPY"
	}
	return result, err
}

func (transactions *Transactions) runExecution(ctx context.Context, execution statementExecution, options PreparedExecution, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	transferred := false
	defer func() {
		if !transferred {
			err = rejectPreparedEnvironment(options.Environment, err)
		}
	}()
	if execution.bind == nil && execution.prepared != nil && execution.prepared.bound != nil && execution.prepared.bound.Command() == nil {
		if err := validateCopyExecution(execution.prepared, options.Copy, consume != nil); err != nil {
			return result, err
		}
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if execution.command != nil {
		execution.prepared, err = execution.command(operation)
		if err != nil {
			return result, transactions.fail(err)
		}
	}
	inline := false
	if execution.prepared != nil && execution.prepared.bound != nil {
		_, inline = execution.prepared.bound.Command().(bind.InlineRoutineCommand)
	}
	if execution.prepared != nil && execution.prepared.bound != nil && execution.prepared.bound.Command() != nil && !inline {
		named := namedSessionCommand(execution.prepared.bound.Command())
		if options.Environment.Reader != nil || options.Environment.Modify.Transaction != nil || options.Environment.Modify.ConstraintTransaction != nil || options.Environment.Modify.Deferral != nil || options.Environment.CopySource != nil || (!named && options.Copy != nil) {
			return result, transactions.fail(errPreparedInvocation)
		}
		if named {
			transferred = true
			return transactions.runNamedCommand(operation, execution.prepared, options, consume)
		}
		transferred = true
		if err := rejectPreparedEnvironment(options.Environment, nil); err != nil {
			return result, transactions.fail(err)
		}
		return transactions.runCommand(operation, execution.prepared, consume)
	}
	result, err = transactions.runPlanExecution(operation, execution, options, consume, &transferred)
	return result, err
}

func (transactions *Transactions) runPlanExecution(operation context.Context, execution statementExecution, options PreparedExecution, consume func(context.Context, *Prepared, *Rows) error, transferred *bool) (result ExecutionResult, err error) {
	inline := false
	if execution.prepared != nil && execution.prepared.bound != nil {
		_, inline = execution.prepared.bound.Command().(bind.InlineRoutineCommand)
	}
	missingPlan := !execution.valid() && !inline
	knownCopy := execution.prepared != nil && execution.prepared.Copy() != nil
	missingConsumer := !inline && (options.Environment.Query == nil || consume == nil && options.explain == nil && execution.bind == nil && !knownCopy)
	foreignOwnership := options.Environment.Reader != nil || options.Environment.Modify.Transaction != nil || options.Environment.Modify.ConstraintTransaction != nil || options.Environment.Modify.Deferral != nil || options.Environment.CopySource != nil

	if missingPlan || missingConsumer || foreignOwnership {
		return result, transactions.fail(errPreparedInvocation)
	}
	options.Environment.Builtin = transactions.settings.BuiltinOptions(options.Environment.Builtin.Collation, options.Environment.Builtin.DefaultCollation)
	resources := transactions.settings.Resources()
	if options.Environment.Query != nil {
		if err := options.Environment.Query.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); err != nil {
			return result, transactions.fail(err)
		}
	}
	constrainEnvironment(&options.Environment, resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes)
	operation = txn.WithLockTimeout(operation, resources.LockTimeout)
	automatic := !transactions.active
	if automatic {
		if err = transactions.begin(operation, TransactionStart{Historical: execution.timestamp(), ImplicitReadOnly: execution.readOnlyHint}); err != nil {
			return result, err
		}
	}
	options.Environment.Temporal = transactions.temporal
	if err = transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	options.Environment.Advisory = transactions.advisoryRuntime()
	if !automatic && execution.timestamp() != 0 {
		if err := transactions.selectPreparedHistory(operation, execution.timestamp()); err != nil {
			return result, err
		}
	}
	if transactions.settings.TransactionPolicy().ReadOnly && options.Environment.Sequences != nil {
		options.Environment.Sequences = readOnlySequences{SequenceRuntime: options.Environment.Sequences}
	}
	advisoryMark, err := transactions.advisoryStatementMark()
	if err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, transactions.fail(err)
	}
	completed := false
	beforeSettings := *transactions.settings
	deferredMark := len(transactions.deferredTriggers)
	deferredConstraintMark := len(transactions.deferredConstraints)
	deferredConstraintBytes := transactions.deferredConstraintBytes
	invocationGeneration := transactions.generation
	defer func() {
		if !completed {
			transactions.markFailed(errSessionStatementPanic)
			if automatic && transactions.active {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	if _, reentry := transactions.routineReentry(operation); reentry {
		*transferred = true
		result.RowsAffected, err = runPreparedStatement(operation, transactions, transactions.transaction, execution, options, consume, false)
	} else if automatic && (inline || execution.nonAtomic) {
		*transferred = true
		result.RowsAffected, err = runPreparedStatement(operation, transactions, transactions.transaction, execution, options, consume, true)
	} else {
		err = transactions.transaction.Statement(operation, func(statement context.Context) (runErr error) {
			*transferred = true
			result.RowsAffected, runErr = runPreparedStatement(statement, transactions, transactions.transaction, execution, options, consume, false)
			return runErr
		})
	}
	if inline {
		result.Tag = "DO"
	}
	err = errors.Join(err, transactions.markDataAccess())
	if err != nil && transactions.generation == invocationGeneration {
		err = errors.Join(err, transactions.rollbackAdvisoryStatement(advisoryMark))
	}
	if err != nil && transactions.generation == invocationGeneration && deferredMark <= len(transactions.deferredTriggers) {
		clear(transactions.deferredTriggers[deferredMark:])
		transactions.deferredTriggers = transactions.deferredTriggers[:deferredMark]
	}
	if err != nil && transactions.generation == invocationGeneration && deferredConstraintMark <= len(transactions.deferredConstraints) {
		clear(transactions.deferredConstraints[deferredConstraintMark:])
		transactions.deferredConstraints = transactions.deferredConstraints[:deferredConstraintMark]
		transactions.deferredConstraintBytes = deferredConstraintBytes
	}
	completed = true
	if err != nil {
		if transactions.generation == invocationGeneration {
			transactions.settings.current = beforeSettings.current
			transactions.settings.retained = beforeSettings.retained
			transactions.settings.customCurrent = beforeSettings.customCurrent
			transactions.settings.customRetained = beforeSettings.customRetained
		}
		result.RowsAffected = 0
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
		if err != nil && transactions.active && transactions.transaction.State() != txn.Indeterminate {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
	}
	return result, err
}

func (transactions *Transactions) selectPreparedHistory(ctx context.Context, timestamp model.Timestamp) error {
	if !transactions.settings.TransactionPolicy().ReadOnly {
		return transactions.fail(settingError("25006", "AS OF SYSTEM TIME requires a read-only transaction"))
	}
	if !transactions.transaction.HasDataAccess() {
		return transactions.setSystemTime(ctx, timestamp)
	}
	// An existing read view is never changed; Prepared.Open checks exact identity.
	return nil
}

type preparedScopeKey struct{}
type routineStatementKey struct{}
type preparedMutationScope struct {
	transaction *txn.Transaction
	active      atomic.Bool
	staging     atomic.Bool
	ambient     bool
}

func (scope *preparedMutationScope) check(ctx context.Context) error {
	if !scope.active.Load() || ctx.Value(preparedScopeKey{}) != scope {
		return errPreparedInvocation
	}
	return ctx.Err()
}
func (scope *preparedMutationScope) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	if err := scope.check(ctx); err != nil {
		return model.Row{}, err
	}
	return scope.transaction.Get(ctx, request)
}
func (scope *preparedMutationScope) ReadView(ctx context.Context, dependencies []txn.ReadDependency) (txn.ReadView, error) {
	if err := scope.check(ctx); err != nil {
		return nil, err
	}
	return scope.transaction.ReadView(ctx, dependencies)
}
func (scope *preparedMutationScope) ApplyMutations(ctx context.Context, mutations []storage.Mutation) error {
	if err := scope.check(ctx); err != nil {
		return err
	}
	return scope.transaction.ApplyMutations(ctx, mutations)
}
func (scope *preparedMutationScope) PrepareInsert(ctx context.Context, input txn.InsertPreparation) (*txn.InsertCandidate, error) {
	if err := scope.check(ctx); err != nil {
		return nil, err
	}
	return scope.transaction.PrepareInsert(ctx, input)
}
func (scope *preparedMutationScope) ApplyPreparedInsert(ctx context.Context, candidate *txn.InsertCandidate) error {
	if err := scope.check(ctx); err != nil {
		return err
	}
	return scope.transaction.ApplyPreparedInsert(ctx, candidate)
}
func (scope *preparedMutationScope) PrepareUpdate(ctx context.Context, input txn.UpdatePreparation) (*txn.UpdateCandidate, error) {
	if err := scope.check(ctx); err != nil {
		return nil, err
	}
	return scope.transaction.PrepareUpdate(ctx, input)
}
func (scope *preparedMutationScope) ApplyPreparedUpdate(ctx context.Context, candidate *txn.UpdateCandidate) error {
	if err := scope.check(ctx); err != nil {
		return err
	}
	return scope.transaction.ApplyPreparedUpdate(ctx, candidate)
}
func (scope *preparedMutationScope) LockRow(ctx context.Context, target storage.ObjectRef, key []byte, strength txn.RowLockStrength, wait txn.RowLockWait) (bool, error) {
	if err := scope.check(ctx); err != nil {
		return false, err
	}
	return scope.transaction.LockRow(ctx, target, key, strength, wait)
}
func (scope *preparedMutationScope) LockRows(ctx context.Context, requests []txn.RowLockRequest, wait txn.RowLockWait) (bool, error) {
	if err := scope.check(ctx); err != nil {
		return false, err
	}
	return scope.transaction.LockRows(ctx, requests, wait)
}
func (scope *preparedMutationScope) ConstraintDefaultValues(ctx context.Context, target storage.ObjectRef, columns []model.ColumnID, runtime model.ExpressionRuntime, maxBytes uint64) ([]model.Value, error) {
	if err := scope.check(ctx); err != nil {
		return nil, err
	}
	return scope.transaction.ConstraintDefaultValues(ctx, target, columns, runtime, maxBytes)
}
func (scope *preparedMutationScope) Statement(ctx context.Context, run func(context.Context) error) error {
	if err := scope.check(ctx); err != nil {
		return err
	}
	if run == nil {
		return errPreparedInvocation
	}
	// A statement reentered through a routine capability runs serially inside
	// the outer staging region and is already marked for this scope; the staging
	// guard only rejects a second outer statement, not routine nesting.
	if ctx.Value(routineStatementKey{}) == scope {
		return run(ctx)
	}
	if scope.staging.Swap(true) {
		return errPreparedInvocation
	}
	defer scope.staging.Store(false)
	if scope.ambient {
		return run(ctx)
	}
	return scope.transaction.Statement(ctx, func(statement context.Context) error {
		return run(context.WithValue(statement, routineStatementKey{}, scope))
	})
}

func invocationSampleSeed(id model.TxnID, serial uint64) uint64 {
	value := serial * 0x9e3779b97f4a7c15
	for _, octet := range id {
		value ^= uint64(octet)
		value *= 0x100000001b3
	}
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	value ^= value >> 31
	if value == 0 {
		return 1
	}
	return value
}

func runPreparedStatement(ctx context.Context, owner *Transactions, transaction *txn.Transaction, execution statementExecution, options PreparedExecution, consume func(context.Context, *Prepared, *Rows) error, nonAtomic bool) (affected uint64, err error) {
	environment := options.Environment
	if len(owner.named.temporary) != 0 {
		environment.MutationTargets = temporaryMutationTargets{named: &owner.named, fallback: environment.MutationTargets}
	}
	owner.sampleSerial++
	environment.SampleSeed = invocationSampleSeed(transaction.ID(), owner.sampleSerial)
	transferred := false
	defer func() {
		if !transferred {
			err = rejectPreparedEnvironment(environment, err)
		}
	}()
	if execution.prepared != nil {
		if inline, ok := execution.prepared.bound.Command().(bind.InlineRoutineCommand); ok {
			// The inline path never opens the caller's environment: it compiles the DO
			// body and creates its own nested queries, views and readers, all released
			// by its own defers. Marking the environment transferred keeps this frame's
			// cleanup from closing the caller-owned Query and Reader.
			transferred = true
			return runInlineRoutineStatement(ctx, owner, transaction, inline, options, &environment, nonAtomic)
		}
		if err := execution.prepared.validatePublication(transaction.PublicationOwner()); err != nil {
			return 0, err
		}
	}
	plainExplain := options.explain != nil && !options.explain.command.Options.Analyze
	deriveDependencies := options.Dependencies == nil
	if options.Routines != nil {
		// The routine body is bound against this statement's view, and binding
		// resolves catalog objects by name. Widen the declaration to the catalog
		// discovery set so those bind-time scans are covered; the prepared
		// dependencies describe the CALL itself, not the statements the routine runs.
		widened, discoveryErr := catalogDiscoveryDependencies(ctx, transaction, options.Catalog, owner.catalogDiscovery(), options.Dependencies, environment.Limits.MetadataBytes)
		if discoveryErr != nil {
			return 0, discoveryErr
		}
		options.Dependencies = widened
	}
	prepared := execution.prepared
	preparedHere := execution.bind != nil
	if prepared != nil {
		// EXECUTE reuses the pinned type runtime captured at prepare; key
		// encoding below resolves declared types against that same pin.
		owner.statementTypes = prepared.builtins.Types
	}
	var view txn.ReadView
	if preparedHere {
		bindingDependencies := execution.bindingDependencies
		if deriveDependencies {
			if bindingDependencies == nil {
				bindingDependencies = catalogReadDependencies(options.Catalog, options.Dependencies)
			}
			if bindingDependencies == nil {
				bindingDependencies = catalogLayoutDependencies(options.Catalog)
			}
		}
		bindingView, bindErr := transaction.ReadView(ctx, bindingDependencies)
		if bindErr != nil {
			return 0, bindErr
		}
		bound, bindErr := execution.bind(ctx, bindingView, environment.Builtin)
		if bindErr == nil && deriveDependencies {
			var scans []txn.ReadDependency
			scans, bindErr = owner.statementScanDependencies(ctx, bindingView, bound, options, options.planningOptions, environment.Builtin)
			if bindErr == nil {
				dependencies := append(slices.Clone(options.Dependencies), scans...)
				if len(catalogReadDependencies(options.Catalog, dependencies)) == 0 {
					dependencies = append(slices.Clone(bindingDependencies), dependencies...)
				}
				options.Dependencies = dependencies
			}
		}
		bindErr = errors.Join(bindErr, bindingView.Close())
		if bindErr != nil {
			return 0, bindErr
		}
		if bound == nil {
			return 0, errPreparedInvocation
		}
		options.Dependencies = bindScanOccurrences(bound, options.Dependencies)
		if plainExplain {
			view, err = transaction.ReadView(ctx, options.Dependencies)
		} else {
			policies := boundRowLockPolicies(bound)
			view, err = transaction.ReadViewForRowLocks(ctx, options.Dependencies, policies)
		}
		if err != nil {
			return 0, err
		}
		prepared, err = execution.prepareBound(ctx, view, environment.Builtin, bound)
		if err != nil {
			return 0, errors.Join(err, view.Close())
		}
		if prepared == nil || prepared.bound == nil || prepared.graph == nil {
			return 0, errors.Join(errPreparedInvocation, view.Close())
		}
		if err := prepared.validatePublication(transaction.PublicationOwner()); err != nil {
			return 0, errors.Join(err, view.Close())
		}
		execution.prepared, execution.bind, execution.prepareBound = prepared, nil, nil
	} else {
		if plainExplain {
			view, err = transaction.ReadView(ctx, options.Dependencies)
		} else {
			view, err = openPreparedReadView(ctx, transaction, execution, options)
		}
		if err != nil {
			return 0, err
		}
	}
	environment.Reader = view
	environment.Settings = owner.settingRuntime(ctx)
	environment.Catalog, err = owner.attachCatalogSettings(ctx, environment.Catalog)
	if err != nil {
		return 0, err
	}
	environment.Catalog, err = attachCatalogView(environment.Catalog, view)
	if err != nil {
		return 0, err
	}
	environment.Native.Random = owner
	environment.Native.Database, _ = environment.Catalog.(model.DatabaseRuntime)
	environment.Native.Triggers = environment.Triggers
	environment.Builtin.Types = prepared.builtins.Types
	environment.Builtin.InformationSchemaFunctions = prepared.builtins.InformationSchemaFunctions
	environment.Builtin.InformationSchemaTypes = prepared.builtins.InformationSchemaTypes
	if parameters := environment.Parameters; len(parameters) != 0 {
		// The bound statement's parameter list is authoritative: callers may
		// supply values without layouts and let binding infer them. NULL values
		// carry no storage tag of their own, so they adopt the bound storage.
		// Text-format values whose storage does not match the bound layout are
		// coerced through the type's input function, matching PostgreSQL's
		// wire-protocol text parameter handling.
		bound := prepared.bound.Parameters()
		reconciled := make([]expr.ParameterValue, len(parameters))
		for index, parameter := range parameters {
			if parameter.Number == 0 || uint64(parameter.Number) > uint64(len(bound)) {
				return 0, settingError("42P02", "parameter number out of range")
			}
			layout := bound[parameter.Number-1]
			value := parameter.Value
			if value.IsNull {
				// NULLs carry no payload; they adopt the bound storage tag.
				value.Type = layout.Type.Storage
			}
			if value.Type != layout.Type.Storage {
				if value.Type != model.TypeString {
					// Storage tags identify the value's physical units, so a
					// mismatched non-text payload cannot be reinterpreted
					// safely; callers must declare the layout the value
					// actually encodes.
					return 0, settingError("42804", "parameter storage does not match the bound type")
				}
				codec, coerceErr := builtin.ResolveTypeTextCodec(ctx, layout, environment.Builtin, environment.Limits.ParameterBytes)
				if coerceErr != nil {
					return 0, coerceErr
				}
				output := min(environment.Limits.Expressions.MaxOutputBytes, uint64(math.MaxInt64))
				budget := builtin.CallBudget{MemoryBytes: environment.Limits.Expressions.MaxStateBytes, OutputBytes: int64(output)}
				if value, coerceErr = codec.Input(ctx, []model.Value{value}, budget); coerceErr != nil {
					return 0, coerceErr
				}
			}
			reconciled[index] = expr.ParameterValue{Number: parameter.Number, Layout: layout, Value: value}
		}
		environment.Parameters = reconciled
	}
	if err = validateCopyExecution(prepared, options.Copy, consume != nil || options.explain != nil); err != nil {
		return 0, err
	}
	environment.Native.TypeInput = newTypeInputRuntime(environment.Relations, environment.Builtin, environment.Temporal, environment.Native)
	copyPlan := prepared.Copy()
	var copyWriter *qcopy.Writer
	if copyPlan != nil {
		switch copyPlan.Direction() {
		case qcopy.From:
			environment.CopySource, err = qcopy.NewSource(ctx, copyPlan, options.Copy, environment.Builtin, environment.Limits.MetadataBytes)
		case qcopy.To:
			copyWriter, err = qcopy.NewWriter(ctx, copyPlan, options.Copy, environment.Builtin, qcopy.Runtime{
				Temporal: environment.Temporal, Relations: environment.Relations, Settings: environment.Settings, TextSearch: environment.TextSearch, Advisory: environment.Advisory, Native: environment.Native,
			}, environment.Limits.MetadataBytes, environment.Limits.BatchBytes)
		default:
			err = errPreparedInvocation
		}
		if err != nil {
			return 0, err
		}
	}
	if encoder, ok := environment.Keys.(*builtin.KeyEncoder); ok {
		environment.Keys = encoder.WithResolveOptions(environment.Builtin)
	}
	if prepared.requiredTimestamp != 0 && view.Timestamp() != prepared.requiredTimestamp {
		return 0, settingError("0A000", "transaction cannot mix historical timestamps")
	}
	if !preparedHere {
		if err = validatePreparedStatistics(ctx, prepared, options.Statistics, view); err != nil {
			return 0, err
		}
	}
	if err = validatePreparedDependencies(ctx, prepared, options.Catalog, view, owner.named.temporaryDescriptors()); err != nil {
		return 0, err
	}
	if options.explain != nil && !options.explain.command.Options.Analyze {
		transferred = true
		if err := rejectPreparedEnvironment(environment, nil); err != nil {
			return 0, err
		}
		return 0, options.explain.publish(ctx, prepared, nil)
	}
	scope := &preparedMutationScope{transaction: transaction, ambient: !nonAtomic}
	scope.active.Store(true)
	defer scope.active.Store(false)
	ctx = context.WithValue(ctx, preparedScopeKey{}, scope)
	ctx, closeCapabilities, attachErr := attachRoutineExecution(ctx, owner, transaction, view, scope, &environment, options.Environment.Catalog, options.Catalog, options, options.Routines, prepared.Dependencies(), nonAtomic)
	if attachErr != nil {
		return 0, attachErr
	}
	defer func() { err = errors.Join(err, closeCapabilities()) }()
	environment.Modify.Transaction = scope
	environment.Modify.Deferral = owner
	environment.SessionIntrospection = owner
	var serializer *explainSerializer
	if options.explain != nil {
		serializer, err = newExplainSerializer(ctx, prepared, environment, options.explain.command.Options.Serialize)
		if err != nil {
			return 0, err
		}
	}
	transferred = true
	portal, err := prepared.Open(ctx, environment)
	if err != nil {
		return 0, err
	}
	if options.explain != nil {
		return runExplainAnalysis(ctx, prepared, portal, options.explain, serializer)
	}
	var copyAffected uint64
	if copyPlan != nil {
		if copyPlan.Direction() == qcopy.From {
			consume = func(context.Context, *Prepared, *Rows) error { return nil }
		} else {
			consume = func(ctx context.Context, _ *Prepared, rows *Rows) error {
				var err error
				copyAffected, err = copyWriter.Write(ctx, rows)
				return err
			}
		}
	}
	rows := &Rows{portal: portal, ctx: ctx}
	defer func() {
		cleanup := portal.Close()
		rows.mu.Lock()
		rows.closed = true
		rows.portal = nil
		rows.ctx = nil
		if rows.terminal != nil && rows.terminal != io.EOF {
			err = errors.Join(err, rows.terminal)
		}
		rows.mu.Unlock()
		if copyPlan != nil && copyPlan.Direction() == qcopy.To {
			affected = copyAffected
		} else {
			affected = portal.RowsAffected()
		}
		err = errors.Join(err, cleanup)
	}()
	err = consume(ctx, prepared, rows)
	if err == nil && prepared.bound.ProcedureCall() {
		// A procedure call is not a lazy query: the routine body is evaluated by
		// the root relation's scalar, so a consumer that never reads the result
		// would skip the body entirely and report success. Drive the portal to
		// completion so the procedure runs exactly once, in order, and any error
		// from its final statements surfaces here instead of being lost.
		var batch columnar.Batch
		for {
			nextErr := rows.Next(ctx, &batch)
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				err = nextErr
				break
			}
		}
	}
	if err == nil && preparedWrites(prepared) {
		err = portal.FinishWrites(ctx)
	}
	return 0, err
}

func validateCopyExecution(prepared *Prepared, streams *qcopy.Streams, hasConsumer bool) error {
	if prepared == nil {
		return errPreparedInvocation
	}
	plan := prepared.Copy()
	if plan == nil {
		if streams != nil || !hasConsumer {
			return errPreparedInvocation
		}
		return nil
	}
	if hasConsumer {
		return &model.Error{Code: "22023", Message: "COPY uses its caller-supplied stream instead of a row consumer"}
	}
	return qcopy.ValidateStreams(plan, streams)
}

func preparedWrites(prepared *Prepared) bool {
	plan := prepared.graph.Plan()
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, _ := plan.Node(id)
		if node.Metadata().Algorithm == physical.Modify {
			return true
		}
	}
	return false
}

func validatePreparedDependencies(ctx context.Context, prepared *Prepared, store *catalog.Store, view txn.ReadView, temporary []*model.Descriptor) (err error) {
	if prepared.bound.DependencyCount() == 0 && !preparedHasSecondaryScan(prepared) {
		return nil
	}
	if store == nil {
		return errPreparedInvocation
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return err
	}
	if err := snapshot.AddSessionOverlay(temporary); err != nil {
		return errors.Join(err, snapshot.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	for i := 0; i < prepared.bound.DependencyCount(); i++ {
		dependency, _ := prepared.bound.Dependency(i)
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		if published.Descriptor.Version() != dependency.Version {
			return errPreparedDependency
		}
	}
	return validatePreparedIndexes(ctx, prepared, snapshot)
}
