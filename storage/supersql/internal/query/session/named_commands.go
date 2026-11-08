package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func sessionCommandStatement(node *zpgparse.Node) bool {
	return node != nil && (node.GetTransactionStmt() != nil || node.GetVariableSetStmt() != nil || node.GetVariableShowStmt() != nil ||
		node.GetDoStmt() != nil || node.GetConstraintsSetStmt() != nil || node.GetLockStmt() != nil ||
		node.GetPrepareStmt() != nil || node.GetExecuteStmt() != nil || node.GetDeallocateStmt() != nil ||
		node.GetDeclareCursorStmt() != nil || node.GetFetchStmt() != nil || node.GetClosePortalStmt() != nil || node.GetDiscardStmt() != nil)
}

func namedSessionCommand(command bind.Command) bool {
	switch command.(type) {
	case bind.PrepareCommand, bind.ExecuteCommand, bind.DeallocateCommand, bind.DeclareCursorCommand, bind.FetchCursorCommand, bind.CloseCursorCommand, bind.DiscardCommand:
		return true
	default:
		return false
	}
}

func (transactions *Transactions) runNamedCommand(ctx context.Context, prepared *Prepared, options PreparedExecution, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	if prepared == nil || prepared.bound == nil || !transactions.named.enabled() {
		return result, transactions.fail(errors.Join(errNamedObjectsDisabled, rejectPreparedEnvironment(options.Environment, nil)))
	}
	command := prepared.bound.Command()
	if execute, ok := command.(bind.ExecuteCommand); ok {
		result, err = transactions.executeNamedPrepared(ctx, execute, options, consume)
		result.Warnings = append(prepared.Warnings(), result.Warnings...)
		return result, err
	}
	defer func() { err = rejectPreparedEnvironment(options.Environment, err) }()
	result.Warnings = append(result.Warnings, prepared.Warnings()...)
	switch command := command.(type) {
	case bind.PrepareCommand:
		result.Tag = "PREPARE"
		result.Commit, err = transactions.prepareNamedStatement(ctx, command, options)
	case bind.DeallocateCommand:
		result.Tag = "DEALLOCATE"
		err = transactions.deallocatePrepared(command)
	case bind.DeclareCursorCommand:
		result.Tag = "DECLARE CURSOR"
		err = transactions.declareCursor(ctx, command, options)
	case bind.FetchCursorCommand:
		return transactions.fetchCursor(ctx, command, options, prepared, consume)
	case bind.CloseCursorCommand:
		result.Tag = "CLOSE CURSOR"
		err = transactions.closeCursor(command)
	case bind.DiscardCommand:
		result.Tag = "DISCARD"
		result.Commit, err = transactions.discard(ctx, command, options)
	default:
		err = errPreparedInvocation
	}
	if err != nil {
		transactions.markFailed(err)
	}
	return result, err
}

func (transactions *Transactions) runNamedPlanning(ctx context.Context, options PreparedExecution, parsed *zpgparse.RawStmt, argtypes []*zpgparse.Node, run func(context.Context, txn.ReadView, builtin.ResolveOptions, *bind.BoundStatement, []model.TypeLayout) error) (commit EndResult, err error) {
	if run == nil || options.Dependencies == nil && options.planningOptions == nil {
		return commit, errPreparedInvocation
	}
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(ctx, TransactionStart{}); err != nil {
			return commit, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return commit, err
	}
	resources := transactions.settings.Resources()
	ctx = txn.WithLockTimeout(ctx, resources.LockTimeout)
	builtins := transactions.settings.BuiltinOptions(options.Environment.Builtin.Collation, options.Environment.Builtin.DefaultCollation)
	err = transactions.routineAtomicStatement(ctx, func(statement context.Context) (runErr error) {
		view, bound, parameters, runErr := transactions.namedStatementPlanView(statement, options, builtins, parsed, argtypes)
		if runErr != nil {
			return runErr
		}
		defer func() { runErr = errors.Join(runErr, view.Close()) }()
		return run(statement, view, builtins, bound, parameters)
	})
	if err != nil {
		transactions.markFailed(err)
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return commit, err
	}
	if automatic {
		commit, err = transactions.commit(ctx, false)
		if err != nil && transactions.active && transactions.transaction.State() != txn.Indeterminate {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}
	return commit, err
}

func (transactions *Transactions) prepareNamedStatement(ctx context.Context, command bind.PrepareCommand, options PreparedExecution) (EndResult, error) {
	statement := command.Statement
	if statement == nil || statement.Query == nil || statement.Name == "" {
		return EndResult{}, errPreparedInvocation
	}
	if _, exists := transactions.named.prepared[statement.Name]; exists {
		return EndResult{}, &model.Error{Code: "42P05", Message: "prepared statement already exists"}
	}
	if len(transactions.named.prepared) == transactions.named.options.MaxPrepared {
		return EndResult{}, &model.Error{Code: "54000", Message: "prepared statement limit exceeded"}
	}
	createdAt, err := namedObjectTime(transactions.temporal.statement)
	if err != nil {
		return EndResult{}, err
	}
	querySQL, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{{Stmt: statement.Query}}})
	if err != nil {
		return EndResult{}, err
	}
	parsed := &zpgparse.RawStmt{Stmt: statement.Query}
	searchPath := transactions.settings.SearchPath()
	var compiled *Prepared
	commit, err := transactions.runNamedPlanning(ctx, options, parsed, statement.Argtypes, func(statementContext context.Context, view txn.ReadView, builtins builtin.ResolveOptions, bound *bind.BoundStatement, parameters []model.TypeLayout) error {
		if bound != nil {
			// The derived path already bound the statement on the discovery view;
			// optimize it against the reopened, scan-covered planning view.
			if options.analyzedPlanner == nil {
				return errPreparedInvocation
			}
			var prepareErr error
			compiled, prepareErr = options.analyzedPlanner(statementContext, view, builtins, &Prepared{bound: bound}, nil)
			return prepareErr
		}
		if options.statementPlanner == nil {
			return errPreparedInvocation
		}
		if len(statement.Argtypes) != 0 {
			if options.parameterTypeResolver == nil {
				return errPreparedInvocation
			}
			var resolveErr error
			parameters, resolveErr = options.parameterTypeResolver(statementContext, view, builtins, statement.Argtypes)
			if resolveErr != nil {
				return resolveErr
			}
		}
		var prepareErr error
		compiled, prepareErr = options.statementPlanner(statementContext, view, builtins, parsed, parameters)
		return prepareErr
	})
	if err != nil {
		return commit, err
	}
	entryDependencies := options.Dependencies
	if entryDependencies == nil {
		// EXECUTE reopens a staleness-check view from these: the broad catalog
		// span covers the reads that revalidation performs.
		entryDependencies = catalogLayoutDependencies(options.Catalog)
	}
	entry, err := transactions.copyPreparedMetadata(ctx, statement.Name, querySQL, searchPath, createdAt, compiled, entryDependencies, 0)
	if err != nil {
		return commit, err
	}
	if _, exists := transactions.named.prepared[entry.name]; exists {
		return commit, &model.Error{Code: "42P05", Message: "prepared statement already exists"}
	}
	if err := transactions.named.reserve(entry.metadataBytes); err != nil {
		return commit, err
	}
	transactions.named.prepared[entry.name] = entry
	return commit, nil
}

func (transactions *Transactions) copyPreparedMetadata(ctx context.Context, name, sql string, searchPath []string, createdAt time.Time, compiled *Prepared, dependencies []txn.ReadDependency, reclaim uint64) (*namedPreparedStatement, error) {
	if compiled == nil || compiled.bound == nil {
		return nil, errPreparedInvocation
	}
	if compiled.bound.ProcedureCall() {
		return nil, &model.Error{Code: "42601", Message: "PREPARE does not accept CALL"}
	}
	parameters := compiled.Parameters()
	columns := compiled.Columns()
	analysis := *compiled
	analysis.graph = nil
	analysis.builtins.Types = nil
	analysis.builtins.InformationSchemaFunctions = nil
	analysis.builtins.InformationSchemaTypes = nil
	analysis.estimatedCost = 0
	analysis.planningDuration = 0
	entry := &namedPreparedStatement{
		name: strings.Clone(name), statement: strings.Clone(sql), preparedAt: createdAt,
		searchPath:       make([]string, len(searchPath)),
		parameterLayouts: make([]model.TypeLayout, len(parameters)),
		parameterTypes:   make([]model.TypeID, len(parameters)),
		resultColumns:    make([]bind.ResultColumn, len(columns)),
		resultTypes:      make([]model.TypeID, len(columns)),
		analysis:         &analysis,
	}
	entry.semantics = compiled.builtins
	entry.semantics.Types = nil
	entry.semantics.InformationSchemaFunctions = nil
	entry.semantics.InformationSchemaTypes = nil
	for i, path := range searchPath {
		entry.searchPath[i] = strings.Clone(path)
	}
	used := namedPreparedBytes(entry)
	if reclaim > transactions.named.metadataBytes {
		return nil, errPreparedInvocation
	}
	maximum := transactions.named.options.MetadataBytes - transactions.named.metadataBytes + reclaim
	if used > maximum {
		return nil, &model.Error{Code: "54000", Message: "prepared statement metadata limit exceeded"}
	}
	ownedDependencies, bytes, err := copyNamedDependencies(ctx, dependencies, maximum-used)
	if err != nil {
		return nil, err
	}
	entry.dependencies = ownedDependencies
	used += bytes
	for i, parameter := range parameters {
		layout, copied, copyErr := model.CopyTypeLayout(ctx, parameter, maximum-used)
		if copyErr != nil {
			return nil, copyErr
		}
		entry.parameterLayouts[i] = layout
		entry.parameterTypes[i] = layout.Type.ID
		used += copied
	}
	for i, column := range columns {
		layout, copied, copyErr := model.CopyTypeLayout(ctx, column.Layout, maximum-used)
		if copyErr != nil {
			return nil, copyErr
		}
		if uint64(len(column.Name)) > maximum-used-copied {
			return nil, model.ErrValueBudget
		}
		entry.resultColumns[i] = bind.ResultColumn{Name: strings.Clone(column.Name), Layout: layout, Nullable: column.Nullable}
		entry.resultTypes[i] = layout.Type.ID
		used += copied + uint64(len(column.Name))
	}
	entry.metadataBytes = used
	return entry, nil
}

func (transactions *Transactions) deallocatePrepared(command bind.DeallocateCommand) error {
	if command.All {
		for name, statement := range transactions.named.prepared {
			transactions.named.release(statement.metadataBytes)
			delete(transactions.named.prepared, name)
		}
		return nil
	}
	statement := transactions.named.prepared[command.Name]
	if statement == nil {
		return &model.Error{Code: "26000", Message: "prepared statement does not exist"}
	}
	transactions.named.release(statement.metadataBytes)
	delete(transactions.named.prepared, command.Name)
	return nil
}

func (transactions *Transactions) discard(ctx context.Context, command bind.DiscardCommand, options PreparedExecution) (EndResult, error) {
	switch command.Target {
	case zpgparse.DiscardMode_DISCARD_PLANS:
		for _, statement := range transactions.named.prepared {
			transactions.named.release(statement.genericBytes)
			statement.metadataBytes -= statement.genericBytes
			statement.generic = nil
			statement.genericBytes = 0
			statement.policy = opt.ParameterPolicy{}
			statement.policyCustomPlans = 0
		}
		return EndResult{}, nil
	case zpgparse.DiscardMode_DISCARD_SEQUENCES:
		sequences, ok := options.Environment.Sequences.(sequenceDiscardRuntime)
		if !ok || historicalNil(sequences) {
			return EndResult{}, errSequenceDiscardUnavailable
		}
		return EndResult{}, sequences.Discard(ctx)
	case zpgparse.DiscardMode_DISCARD_TEMP:
		return transactions.discardTemporary(ctx)
	case zpgparse.DiscardMode_DISCARD_ALL:
		if transactions.active {
			return EndResult{}, &model.Error{Code: "25001", Message: "DISCARD ALL cannot run inside a transaction block"}
		}
		commit, err := transactions.discardTemporary(ctx)
		err = errors.Join(err, transactions.closeAllCursors())
		err = errors.Join(err, transactions.deallocatePrepared(bind.DeallocateCommand{All: true}))
		err = errors.Join(err, resetSequenceSession(ctx, options.Environment.Sequences))
		err = errors.Join(err, transactions.settings.ResetAll(ctx))
		return commit, err
	default:
		return EndResult{}, errPreparedInvocation
	}
}

// resetSequenceSession applies the required DISCARD SEQUENCES capability to a
// provider, reporting an explicit configuration error when it is absent.
func resetSequenceSession(ctx context.Context, provider model.SequenceRuntime) error {
	if historicalNil(provider) {
		return errSequenceDiscardUnavailable
	}
	discarder, ok := provider.(sequenceDiscardRuntime)
	if !ok {
		return errSequenceDiscardUnavailable
	}
	return discarder.Discard(ctx)
}

func (transactions *Transactions) discardTemporary(ctx context.Context) (commit EndResult, err error) {
	if transactions.named.options.DiscardTemporary == nil && len(transactions.named.temporary) == 0 {
		return commit, nil
	}
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(ctx, TransactionStart{}); err != nil {
			return commit, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return commit, err
	}
	err = transactions.routineAtomicStatement(ctx, func(statement context.Context) (statementErr error) {
		mark := len(transactions.named.temporaryChanges)
		defer func() {
			if statementErr != nil {
				transactions.named.rollbackTemporary(mark)
			}
		}()
		if err := transactions.discardSessionTemporary(statement); err != nil {
			return err
		}
		if transactions.named.options.DiscardTemporary != nil {
			return transactions.named.options.DiscardTemporary(statement, transactions.transaction)
		}
		return nil
	})
	err = errors.Join(err, transactions.markDataAccess())
	if err != nil {
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return commit, err
	}
	if automatic {
		commit, err = transactions.commit(ctx, false)
		if err != nil && transactions.active && transactions.transaction.State() != txn.Indeterminate {

			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
	}
	return commit, err
}

func sameResultShape(left, right []bind.ResultColumn) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].Nullable != right[i].Nullable || !sameTypeLayout(left[i].Layout, right[i].Layout) {
			return false
		}
	}
	return true
}

func sameTypeLayout(left, right model.TypeLayout) bool {
	if left.Type != right.Type || len(left.Children) != len(right.Children) || len(left.FieldNames) != len(right.FieldNames) {
		return false
	}
	for i := range left.FieldNames {
		if left.FieldNames[i] != right.FieldNames[i] {
			return false
		}
	}
	for i := range left.Children {
		if !sameTypeLayout(left.Children[i], right.Children[i]) {
			return false
		}
	}
	return true
}
