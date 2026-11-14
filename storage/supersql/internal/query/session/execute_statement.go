package session

import (
	"context"
	"errors"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// StatementExecution supplies bounded planning against protected metadata and
// execution views. Dependencies must cover binding catalog reads and planned
// data spans. Planning is invoked once per view and may retain neither.
type StatementExecution struct {
	Execution  PreparedExecution
	Parameters []model.TypeLayout
	Planning   func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader) (PrepareOptions, error)
}

type statementExecution struct {
	prepared            *Prepared
	bind                func(context.Context, txn.ReadView, builtin.ResolveOptions) (*bind.BoundStatement, error)
	prepareBound        func(context.Context, txn.ReadView, builtin.ResolveOptions, *bind.BoundStatement) (*Prepared, error)
	bindingDependencies []txn.ReadDependency
	command             func(context.Context) (*Prepared, error)
	nonAtomic           bool
	readOnlyHint        bool
}

func (execution statementExecution) valid() bool {
	if execution.bind != nil || execution.prepareBound != nil {
		return execution.prepared == nil && execution.bind != nil && execution.prepareBound != nil
	}
	return execution.prepared != nil && execution.prepared.bound != nil && execution.prepared.graph != nil
}
func (execution statementExecution) timestamp() model.Timestamp {
	if execution.prepared == nil {
		return 0
	}
	return execution.prepared.requiredTimestamp
}

// RunStatement binds metadata first, then locks every declared data span before
// opening the planning/execution snapshot. Parameters are typed values, never
// SQL text interpolation. The consumer borrows Rows and may retain immutable
// Prepared metadata. Statement controls no public/legacy dispatch or fallback.
func (transactions *Transactions) RunStatement(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	// The session key encoder resolves declared types only for the statement
	// that pinned them; a path that skips activation must see no resolver
	// rather than a previous statement's snapshot. Routine bodies re-enter
	// RunStatement on this session, so the enclosing pin is restored on exit.
	previous := transactions.statementTypes
	transactions.statementTypes = nil
	defer func() { transactions.statementTypes = previous }()
	var root *zpgparse.Node
	if statement != nil {
		root = statement.Stmt
	}
	// The caller supplies this query and keeps it across statements; the session
	// only borrows it, so the portal and rejection paths must not close it.
	options.Execution.Environment.BorrowQuery = true
	if err := bind.ValidateStatementFeatures(ctx, root); err != nil {
		// A rejected statement is an ordinary statement error: inside a block it
		// aborts the transaction, and later commands report 25P02 until ROLLBACK
		// or ROLLBACK TO a savepoint. Every post-dispatch path marks failure;
		// this pre-dispatch gate must not silently keep the block usable.
		transactions.markFailed(err)
		return ExecutionResult{}, rejectPreparedEnvironment(options.Execution.Environment, err)
	}
	statement = normalizeSelectInto(statement)
	copyStatement := (*zpgparse.CopyStmt)(nil)
	if statement != nil && statement.Stmt != nil {
		copyStatement = statement.Stmt.GetCopyStmt()
	}
	if copyStatement != nil {
		if copyStatement.IsProgram || copyStatement.Filename != "" {
			err := &model.Error{Code: "42601", Message: "COPY accepts only caller-supplied STDIN or STDOUT"}
			return ExecutionResult{}, errors.Join(err, rejectPreparedEnvironment(options.Execution.Environment, nil))
		}
		direction := qcopy.To
		if copyStatement.IsFrom {
			direction = qcopy.From
		}
		if consume != nil {
			err := &model.Error{Code: "22023", Message: "COPY uses its caller-supplied stream instead of a row consumer"}
			return ExecutionResult{}, errors.Join(err, rejectPreparedEnvironment(options.Execution.Environment, nil))
		}
		if err := qcopy.ValidateDirection(direction, options.Execution.Copy); err != nil {
			return ExecutionResult{}, errors.Join(err, rejectPreparedEnvironment(options.Execution.Environment, nil))
		}
	} else if options.Execution.Copy != nil {
		return ExecutionResult{}, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	// Command families declare catalog coverage themselves when the caller
	// leaves their Dependencies empty: a public caller cannot enumerate the
	// read set before binding, so the session supplies the broad catalog span
	// and each path still layers its discovered descriptor and data
	// dependencies on top.
	if store := options.Execution.Catalog; store != nil {
		widen := func(dependencies []txn.ReadDependency) []txn.ReadDependency {
			if dependencies == nil {
				return catalogLayoutDependencies(store)
			}
			return dependencies
		}
		if configured := options.Execution.ObjectDDL; configured != nil {
			object := *configured
			object.Dependencies = widen(object.Dependencies)
			options.Execution.ObjectDDL = &object
		}
		if configured := options.Execution.TypeDDL; configured != nil {
			typed := *configured
			typed.Dependencies = widen(typed.Dependencies)
			options.Execution.TypeDDL = &typed
		}
		if configured := options.Execution.Statistics; configured != nil {
			statistics := *configured
			statistics.Dependencies = widen(statistics.Dependencies)
			options.Execution.Statistics = &statistics
		}
		if configured := options.Execution.RoutineDDL; configured != nil {
			routine := *configured
			routine.Dependencies = widen(routine.Dependencies)
			options.Execution.RoutineDDL = &routine
		}
		if configured := options.Execution.TextSearchDDL; configured != nil {
			textSearch := *configured
			textSearch.Dependencies = widen(textSearch.Dependencies)
			options.Execution.TextSearchDDL = &textSearch
		}
	}
	// Every dispatch branch below waits on the same lock manager, so the
	// session's lock_timeout bounds each branch's waits from this single point.
	// Prepared and named paths wrap their own contexts; re-wrapping there with the
	// same value is harmless.
	ctx = txn.WithLockTimeout(ctx, transactions.settings.Resources().LockTimeout)
	if result, handled, err := transactions.runStatisticsCommand(ctx, statement, options); handled {
		return result, err
	}
	if result, handled, err := transactions.runTextSearchCommand(ctx, statement, options); handled {
		return result, err
	}
	if result, handled, err := transactions.runDefinitionCommand(ctx, statement, options); handled {
		return result, err
	}
	if result, handled, err := transactions.runTypeCommand(ctx, statement, options); handled {
		return result, err
	}
	if result, handled, err := transactions.runObjectCommand(ctx, statement, options); handled {
		return result, err
	}
	if result, handled, err := transactions.runSchemaCommand(ctx, statement, options); handled {
		return result, err
	}
	var prepared *Prepared
	if options.Execution.Catalog != nil && options.Planning != nil {
		options.Execution.statementPlanner = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, parsed *zpgparse.RawStmt, parameters []model.TypeLayout) (result *Prepared, err error) {
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			prepared, err := Prepare(ctx, parsed, snapshot, planning, parameters)
			if err == nil {
				prepared.requiredTimestamp = transactions.historical
			}
			return prepared, err
		}
		options.Execution.analyzedPlanner = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, analyzed *Prepared, parameters []expr.ParameterValue) (result *Prepared, err error) {
			if analyzed == nil || analyzed.bound == nil {
				return nil, errPreparedInvocation
			}
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			if parameters != nil {
				planning.Optimize.Parameters = make([]span.Parameter, len(parameters))
				for i, parameter := range parameters {
					planning.Optimize.Parameters[i] = span.Parameter{Number: parameter.Number, Layout: parameter.Layout, Value: parameter.Value}
				}
			}
			prepared, err := prepareBound(ctx, analyzed.bound, planning)
			if err == nil {
				prepared.requiredTimestamp = transactions.historical
			}
			return prepared, err
		}
		options.Execution.parameterTypeResolver = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, names []*zpgparse.Node) (layouts []model.TypeLayout, err error) {
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			return bind.ResolveParameterTypes(ctx, names, snapshot, planning.Bind)
		}
		options.Execution.argumentPlanner = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, arguments []*zpgparse.Node, targets []model.TypeLayout) (result *Prepared, err error) {
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			bound, err := bind.BindUtilityArguments(ctx, arguments, targets, snapshot, planning.Bind)
			if err != nil {
				return nil, err
			}
			return prepareBound(ctx, bound, planning)
		}
		options.Execution.planningOptions = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error) {
			return transactions.statementPlanning(ctx, view, builtins, options)
		}
	}
	if statement != nil && statement.Stmt != nil && statement.Stmt.GetExplainStmt() != nil {
		return transactions.runExplainStatement(ctx, statement, options, consume)
	}
	if statement != nil && statement.Stmt != nil && sessionCommandStatement(statement.Stmt) {
		command := func(ctx context.Context) (*Prepared, error) {
			transaction := statement.Stmt.GetTransactionStmt()
			recovery := transaction != nil && (transaction.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT || transaction.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK || transaction.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO)
			if transactions.active && !recovery {
				if err := transactions.ordinary(); err != nil {
					return nil, err
				}
			}
			if statement.Stmt.GetLockStmt() != nil {
				if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
					return nil, settingError("42P02", "utility command does not accept parameters")
				}
				return transactions.bindTableLockCommand(ctx, statement, options)
			}
			if statement.Stmt.GetConstraintsSetStmt() != nil && transactions.active {
				if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
					return nil, settingError("42P02", "utility command does not accept parameters")
				}
				return transactions.bindConstraintModeCommand(ctx, statement, options)
			}
			bound, err := bind.BindCommand(ctx, statement, transactions.commandBindOptions())
			if err != nil {
				return nil, err
			}
			if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
				return nil, settingError("42P02", "utility command does not accept parameters")
			}
			prepared = &Prepared{bound: bound}
			return prepared, nil
		}
		return transactions.runExecution(ctx, statementExecution{command: command}, options.Execution, consume)
	}
	var bindStatement func(context.Context, txn.ReadView, builtin.ResolveOptions) (*bind.BoundStatement, error)
	var planBound func(context.Context, txn.ReadView, builtin.ResolveOptions, *bind.BoundStatement) (*Prepared, error)
	if statement != nil && statement.Stmt != nil && options.Execution.statementPlanner != nil {
		bindStatement = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions) (result *bind.BoundStatement, err error) {
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			planning.Bind.Builtin = planning.Normalize.Builtin
			return bind.Bind(ctx, statement, snapshot, planning.Bind, options.Parameters)
		}
		planBound = func(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, bound *bind.BoundStatement) (result *Prepared, err error) {
			snapshot, planning, err := transactions.statementPlanning(ctx, view, builtins, options)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			prepared, err := prepareBound(ctx, bound, planning)
			if err == nil {
				prepared.requiredTimestamp = transactions.historical
			}
			return prepared, err
		}
	}
	nonAtomic := statement != nil && statement.Stmt != nil && statement.Stmt.GetCallStmt() != nil
	readOnlyHint := statement != nil && isReadOnlyAutocommitSelect(statement.Stmt)
	execution := statementExecution{
		bind: bindStatement, prepareBound: planBound,
		bindingDependencies: catalogReadDependencies(options.Execution.Catalog, options.Execution.Dependencies),
		nonAtomic:           nonAtomic,
		readOnlyHint:        readOnlyHint,
	}
	result, execErr := transactions.runExecution(ctx, execution, options.Execution, consume)
	if statement != nil && statement.Stmt != nil && statement.Stmt.GetCopyStmt() != nil {
		result.Tag = "COPY"
	}
	return result, execErr
}

func isReadOnlyAutocommitSelect(root *zpgparse.Node) bool {
	if root == nil {
		return false
	}
	selectStmt := root.GetSelectStmt()
	if selectStmt == nil || selectStmt.IntoClause != nil || len(selectStmt.LockingClause) > 0 {
		return false
	}
	pure := true
	var walk func(protoreflect.Message)
	walk = func(message protoreflect.Message) {
		if !pure || !message.IsValid() {
			return
		}
		if node, ok := message.Interface().(*zpgparse.Node); ok && node != nil {
			switch inner := node.Node.(type) {
			case *zpgparse.Node_InsertStmt, *zpgparse.Node_UpdateStmt, *zpgparse.Node_DeleteStmt, *zpgparse.Node_MergeStmt, *zpgparse.Node_LockingClause:
				pure = false
				return
			case *zpgparse.Node_FuncCall:
				if inner.FuncCall == nil || !isReadOnlyFunctionCall(inner.FuncCall) {
					pure = false
					return
				}
			}
		}
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			if !pure {
				return false
			}
			switch {
			case field.IsList():
				list := value.List()
				for i := 0; i < list.Len() && pure; i++ {
					if field.Kind() == protoreflect.MessageKind {
						walk(list.Get(i).Message())
					}
				}
			case field.Kind() == protoreflect.MessageKind:
				walk(value.Message())
			}
			return pure
		})
	}
	walk(root.ProtoReflect())
	return pure
}

func isReadOnlyFunctionCall(call *zpgparse.FuncCall) bool {
	if call == nil || len(call.Funcname) == 0 || len(call.Funcname) > 2 {
		return false
	}
	if len(call.Funcname) == 2 {
		schemaNode := call.Funcname[0].GetString_()
		if schemaNode == nil || !strings.EqualFold(schemaNode.Sval, "pg_catalog") {
			return false
		}
	}
	nameNode := call.Funcname[len(call.Funcname)-1].GetString_()
	if nameNode == nil {
		return false
	}
	switch strings.ToLower(nameNode.Sval) {
	case "count", "sum", "avg", "min", "max", "coalesce", "nullif", "greatest", "least",
		"lower", "upper", "length", "char_length", "character_length", "octet_length",
		"substr", "substring", "trim", "btrim", "ltrim", "rtrim", "replace", "concat", "concat_ws",
		"abs", "round", "trunc", "floor", "ceil", "ceiling", "mod", "power", "sqrt", "sign",
		"now", "current_timestamp", "statement_timestamp", "transaction_timestamp", "clock_timestamp",
		"current_date", "current_time", "localtime", "localtimestamp", "extract", "date_part", "date_trunc",
		"current_setting", "current_user", "session_user", "current_role", "user",
		"current_schema", "current_schemas", "current_database", "pg_backend_pid",
		"row_number", "rank", "dense_rank", "ntile", "lag", "lead", "first_value", "last_value", "nth_value",
		"string_agg", "array_agg", "bool_and", "bool_or", "every", "json_agg", "jsonb_agg",
		"generate_series", "unnest", "array_length", "cardinality":
		return true
	default:
		return false
	}
}
func normalizeSelectInto(statement *zpgparse.RawStmt) *zpgparse.RawStmt {
	if statement == nil || statement.Stmt == nil {
		return statement
	}
	selectStatement := statement.Stmt.GetSelectStmt()
	if selectStatement == nil || selectStatement.IntoClause == nil {
		return statement
	}
	normalized := proto.Clone(statement).(*zpgparse.RawStmt)
	selectStatement = normalized.Stmt.GetSelectStmt()
	into := selectStatement.IntoClause
	selectStatement.IntoClause = nil
	normalized.Stmt = &zpgparse.Node{Node: &zpgparse.Node_CreateTableAsStmt{CreateTableAsStmt: &zpgparse.CreateTableAsStmt{
		Query:        &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: selectStatement}},
		Into:         into,
		Objtype:      zpgparse.ObjectType_OBJECT_TABLE,
		IsSelectInto: true,
	}}}
	return normalized
}

// searchPath returns the session search path with the session's temporary
// schema first once the session owns a temporary object. PostgreSQL resolves
// pg_temp before every catalog entry, so a session that created a temporary
// relation binds that name ahead of an identically named permanent one. The
// temporary schema itself is session-local: it exists only in the session
// overlay, never in the catalog the session's reads are declared against.
func (transactions *Transactions) searchPath() []string {
	path := transactions.settings.SearchPath()
	if len(transactions.named.temporary) == 0 || slices.Contains(path, temporarySchemaName) {
		return path
	}
	return append([]string{temporarySchemaName}, path...)
}

func (transactions *Transactions) statementPlanning(ctx context.Context, view txn.ReadView, builtins builtin.ResolveOptions, options StatementExecution) (*catalog.CatalogSnapshot, PrepareOptions, error) {
	snapshot, err := options.Execution.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, PrepareOptions{}, err
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, PrepareOptions{}, errors.Join(err, snapshot.Close())
	}
	// The declared-type runtime is pinned before the planning factory runs, so
	// the factory reuses this statement's runtime for its key encoder instead of
	// building a second one from the same snapshot.
	if options.Execution.TypeRuntime != nil {
		runtimeOptions := *options.Execution.TypeRuntime
		runtimeOptions.Expressions.Builtin = builtins
		if runtimeOptions.Expressions.Builtin.InformationSchemaTypes == nil {
			runtimeOptions.Expressions.Builtin.InformationSchemaTypes = options.Execution.TypeRuntime.Expressions.Builtin.InformationSchemaTypes
		}
		if runtimeOptions.Expressions.Builtin.InformationSchemaFunctions == nil {
			runtimeOptions.Expressions.Builtin.InformationSchemaFunctions = options.Execution.TypeRuntime.Expressions.Builtin.InformationSchemaFunctions
		}
		if runtimeOptions.Expressions.Functions == nil {
			runtimeOptions.Expressions.Functions = options.Execution.Environment.Functions
		}
		if options.Execution.Routines != nil {
			runtimeOptions.Routines = options.Execution.Routines.Limits
		}
		types, resolveErr := transactions.declaredTypeRuntime(ctx, snapshot, runtimeOptions)
		if resolveErr != nil {
			return nil, PrepareOptions{}, errors.Join(resolveErr, snapshot.Close())
		}
		builtins.Types = types
		transactions.statementTypes = types
	}
	planning, err := options.Planning(ctx, snapshot, view)
	if err != nil {
		return nil, PrepareOptions{}, errors.Join(err, snapshot.Close())
	}
	builtins.InformationSchemaFunctions = planning.Bind.CatalogRelations
	builtins.InformationSchemaTypes = planning.Bind.CatalogRelations
	planning.Normalize.Builtin = builtins
	planning.Bind.Temporal = transactions.temporal
	planning.Bind.SearchPath = transactions.searchPath()
	planning.Optimize.WorkingMemory = min(planning.Optimize.WorkingMemory, uint64(transactions.settings.Resources().EffectiveWorkMemoryBytes))
	planning.Optimize.StatisticsKeyEncoder = transactions.options.KeyEncoder
	if encoder, ok := transactions.options.KeyEncoder.(*builtin.KeyEncoder); ok {
		planning.Optimize.StatisticsKeyEncoder = encoder.WithResolveOptions(builtins)
	}
	if statistics := options.Execution.Statistics; statistics != nil && statistics.Store != nil {
		planning.Optimize.ExtendedStatistics = &catalogExtendedStatistics{snapshot: snapshot, limits: statistics.Binding.Descriptors}
		planning.Optimize.StatisticsLimits = statistics.Store.Limits()
	}
	if transactions.historical != 0 {
		validate := planning.ValidateBound
		planning.ValidateBound = func(ctx context.Context, bound *bind.BoundStatement) error {
			if err := historicalBound(ctx, bound, transactions.historical); err != nil {
				return err
			}
			if validate != nil {
				return validate(ctx, bound)
			}
			return nil
		}
	}
	return snapshot, planning, nil
}
