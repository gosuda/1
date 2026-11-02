package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
	"google.golang.org/protobuf/proto"
)

type ObjectCommandOptions struct {
	Binding          bind.CreateDeclarationOptions
	Dependencies     []txn.ReadDependency
	Columns          ColumnExpressionOptions
	Indexes          IndexExpressionOptions
	Limits           schema.IndexLimits
	PolicyBytes      uint64
	MaxDescriptors   uint32
	Constraints      *schema.IndexPolicyProvider
	// RewriteQuery rewrites the source query that populates a CREATE TABLE AS
	// or REFRESH MATERIALIZED VIEW target. The target is the relation this
	// command declares or refreshes, so only the source is offered.
	RewriteQuery     func(context.Context, *zpgparse.RawStmt) (*zpgparse.RawStmt, error)
	populate         func(context.Context, *bind.BoundObjectCommand, *DeclarationResult) (uint64, error)
}

type ObjectCommandResult struct {
	Tag          string
	Warnings     []model.Error
	Commit       EndResult
	RowsAffected uint64
}

func (transactions *Transactions) runObjectCommand(ctx context.Context, statement *zpgparse.RawStmt, invocation StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil || !isObjectCommand(statement.Stmt) {
		return ExecutionResult{}, false, nil
	}
	node := statement.Stmt
	if configured := invocation.Execution.Schema; configured != nil && configured.Online != nil {
		sidecar := *configured.Online
		if node.GetAlterTableStmt() != nil && sidecar.StatementIndex == configured.StatementIndex && sidecar.Location >= 0 {
			return ExecutionResult{}, false, nil
		}
	}
	if reindex := node.GetReindexStmt(); bind.ReindexConcurrent(reindex) && !transactions.reindexTargetTemporary(reindex) {
		return ExecutionResult{}, false, nil
	}
	if invocation.Execution.ObjectDDL == nil {
		return ExecutionResult{}, true, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if len(invocation.Parameters) != 0 || len(invocation.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "object command does not accept parameters"), rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if invocation.Execution.Environment.Reader != nil {
		return ExecutionResult{}, true, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	options := *invocation.Execution.ObjectDDL
	if invocation.Execution.TypeDDL != nil {
		options.Constraints = invocation.Execution.TypeDDL.Cascade.Policies
	}
	if truncate := node.GetTruncateStmt(); truncate != nil {
		result, err := transactions.runTruncate(ctx, truncate, options, invocation)
		return result, true, err
	}
	if index := node.GetIndexStmt(); index != nil && !index.Concurrent {
		var cascade *TypeCascadeOptions
		if invocation.Execution.TypeDDL != nil {
			configured := invocation.Execution.TypeDDL.Cascade
			cascade = &configured
		}
		result, err := transactions.runCreateIndex(ctx, index, options, cascade, invocation)
		return result, true, err
	}
	if reindex := node.GetReindexStmt(); reindex != nil {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		var cascade *TypeCascadeOptions
		if invocation.Execution.TypeDDL != nil {
			configured := invocation.Execution.TypeDDL.Cascade
			cascade = &configured
		}
		result, err := transactions.runReindex(ctx, reindex, options, cascade)
		return result, true, err
	}
	if alter := node.GetAlterTableStmt(); bind.ExpressionAlter(alter) || bind.TransactionalGenerationAlter(alter) || bind.SchemaJobAlter(alter) {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		var cascade *TypeCascadeOptions
		if invocation.Execution.TypeDDL != nil {
			configured := invocation.Execution.TypeDDL.Cascade
			cascade = &configured
		}
		result, err := transactions.runAlterTableExpression(ctx, alter, options, cascade)
		return result, true, err
	}
	if alter := node.GetAlterTableStmt(); bind.IdentitySequenceAlter(alter) {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := transactions.runAlterTableIdentitySequence(ctx, node, alter, options)
		return result, true, err
	}
	if alter := node.GetAlterTableStmt(); bind.IdentityDeclarationAlter(alter) {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		tableOptions := objectTableOptions(options)
		if invocation.Execution.TypeDDL != nil {
			tableOptions.Constraints = invocation.Execution.TypeDDL.Cascade.Policies
		}
		result, err := ExecuteAlterTableIdentity(ctx, transactions, alter, tableOptions)
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if create := node.GetCreateStmt(); create != nil {
		tableOptions := objectTableOptions(options)
		population := objectCommandRequiresPopulation(node)
		populationInvoked := false
		if population {
			tableOptions.populate = func(statementContext context.Context, declaration *bind.BoundDeclaration) (uint64, error) {
				populationInvoked = true
				return transactions.populateTableDeclaration(statementContext, declaration, invocation)
			}
		} else if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := ExecuteCreateTable(ctx, transactions, create, tableOptions)
		if population && !populationInvoked {
			err = rejectPreparedEnvironment(invocation.Execution.Environment, err)
		}
		return ExecutionResult{RowsAffected: result.RowsAffected, Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if create := node.GetCreateSchemaStmt(); create != nil {
		if len(create.SchemaElts) != 0 {
			result, err := transactions.runEmbeddedSchema(ctx, statement, create, invocation)
			return result, true, err
		}
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := ExecuteCreateSchema(ctx, transactions, create, objectTableOptions(options))
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if create := node.GetCreateSeqStmt(); create != nil {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := ExecuteCreateSequence(ctx, transactions, create, SequenceCommandOptions{Binding: options.Binding, Dependencies: options.Dependencies})
		return ExecutionResult{Tag: "CREATE SEQUENCE", Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if alter := node.GetAlterSeqStmt(); alter != nil {
		if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := ExecuteAlterSequence(ctx, transactions, alter, SequenceCommandOptions{Binding: options.Binding, Dependencies: options.Dependencies})
		return ExecutionResult{Tag: "ALTER SEQUENCE", Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	population := objectCommandRequiresPopulation(node)
	populationInvoked := false
	if population {
		if invocation.Execution.Environment.Query == nil || invocation.Planning == nil || invocation.Execution.Catalog == nil {
			return ExecutionResult{}, true, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
		}
		options.populate = func(statementContext context.Context, bound *bind.BoundObjectCommand, admission *DeclarationResult) (uint64, error) {
			populationInvoked = true
			return transactions.populateObjectCommand(statementContext, bound, node, invocation, admission)
		}
	} else if err := rejectPreparedEnvironment(invocation.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	result, err := ExecuteObjectCommand(ctx, transactions, node, options)
	if population && !populationInvoked {
		err = rejectPreparedEnvironment(invocation.Execution.Environment, err)
	}
	return ExecutionResult{RowsAffected: result.RowsAffected, Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
}

func (transactions *Transactions) reindexTargetTemporary(statement *zpgparse.ReindexStmt) bool {
	if transactions == nil || statement == nil || statement.Relation == nil {
		return false
	}
	if schema := statement.Relation.Schemaname; schema != "" && schema != temporarySchemaName {
		return false
	}
	schemaID := transactions.named.temporaryNames[model.NamespaceName{Kind: model.ObjectSchema, Name: temporarySchemaName}]
	if schemaID == 0 {
		return false
	}
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectMaterializedView}
	if statement.Kind == zpgparse.ReindexObjectType_REINDEX_OBJECT_INDEX {
		kinds = []model.ObjectKind{model.ObjectIndex}
	}
	for _, kind := range kinds {
		name := model.NamespaceName{Parent: schemaID, Kind: kind, Name: statement.Relation.Relname}
		if id := transactions.named.temporaryNames[name]; id != 0 && transactions.named.temporary[id] != nil {
			return true
		}
	}
	return false
}

func (transactions *Transactions) runAlterTableIdentitySequence(ctx context.Context, node *zpgparse.Node, statement *zpgparse.AlterTableStmt, options ObjectCommandOptions) (result ExecutionResult, err error) {
	if transactions.sequences == nil {
		return result, transactions.fail(settingError("0A000", "catalog DDL services are not configured"))
	}
	automatic, err := transactions.beginEmbeddedSchema(ctx)
	if err != nil {
		return result, err
	}
	completed := false
	defer func() {
		if !completed && automatic {
			_ = transactions.Rollback(context.WithoutCancel(ctx), false)
		}
	}()
	options.Binding.Bind.SearchPath = declarationSearchPath(transactions.settings.SearchPath(), options.Binding.SchemaContext)
	options.Binding.Owner = transactions.settings.Principal().OID()
	options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
	options.Binding.Bind.Temporal = transactions.temporal
	options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
	alterSequence, err := transactions.bindIdentitySequenceAlter(ctx, statement, options)
	if err != nil {
		return result, transactions.failEmbeddedSchema(ctx, err, automatic)
	}
	if alterSequence != nil {
		sequenceResult, sequenceErr := ExecuteAlterSequence(ctx, transactions, alterSequence, SequenceCommandOptions{Binding: options.Binding, Dependencies: options.Dependencies})
		if sequenceErr != nil {
			return result, transactions.failEmbeddedSchema(ctx, sequenceErr, automatic)
		}
		result.Warnings = append(result.Warnings, sequenceResult.Warnings...)
	}
	options.Binding.IdentitySequenceAltered = true
	objectResult, err := ExecuteObjectCommand(ctx, transactions, node, options)
	if err != nil {
		return result, transactions.failEmbeddedSchema(ctx, err, automatic)
	}
	result.Tag = objectResult.Tag
	result.Warnings = append(result.Warnings, objectResult.Warnings...)
	if automatic {
		result.Commit, err = transactions.Commit(ctx, false)
		if err != nil {
			completed = true
			return result, err
		}
	}
	completed = true
	return result, nil
}

func (transactions *Transactions) bindIdentitySequenceAlter(ctx context.Context, statement *zpgparse.AlterTableStmt, options ObjectCommandOptions) (*zpgparse.AlterSeqStmt, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	bound, bindErr := bind.BindIdentitySequenceAlter(ctx, statement, snapshot, options.Binding)
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) runEmbeddedSchema(ctx context.Context, raw *zpgparse.RawStmt, statement *zpgparse.CreateSchemaStmt, invocation StatementExecution) (result ExecutionResult, err error) {
	if statement.IfNotExists {
		return result, errors.Join(settingError("42601", "CREATE SCHEMA elements cannot be used with IF NOT EXISTS"), rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if statement.Schemaname == "" {
		return result, errors.Join(settingError("42601", "CREATE SCHEMA elements require a schema name"), rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	if len(statement.SchemaElts) > invocation.Execution.ObjectDDL.Binding.Descriptors.MaxItems {
		return result, errors.Join(model.ErrDescriptorLimit, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	automatic, err := transactions.beginEmbeddedSchema(ctx)
	if err != nil {
		return result, errors.Join(err, rejectPreparedEnvironment(invocation.Execution.Environment, nil))
	}
	completed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if automatic {
				_ = transactions.Rollback(context.WithoutCancel(ctx), false)
			}
			panic(recovered)
		}
		if !completed && automatic {
			_ = transactions.Rollback(context.WithoutCancel(ctx), false)
		}
	}()
	embedded := embeddedSchemaInvocation(invocation, statement.Schemaname)
	schemaOnly := proto.Clone(raw).(*zpgparse.RawStmt)
	schemaOnly.Stmt.GetCreateSchemaStmt().SchemaElts = nil
	created, err := transactions.RunStatement(ctx, schemaOnly, embedded, nil)
	if err != nil {
		completed = true
		return result, transactions.failEmbeddedSchema(ctx, err, automatic)
	}
	result.Tag = "CREATE SCHEMA"
	result.Warnings = append(result.Warnings, created.Warnings...)
	for _, element := range statement.SchemaElts {
		node := proto.Clone(element).(*zpgparse.Node)
		if err := qualifyEmbeddedSchemaObject(statement.Schemaname, node); err != nil {
			completed = true
			return result, transactions.failEmbeddedSchema(ctx, errors.Join(err, rejectPreparedEnvironment(invocation.Execution.Environment, nil)), automatic)
		}
		child, err := transactions.RunStatement(ctx, &zpgparse.RawStmt{Stmt: node, StmtLocation: raw.StmtLocation, StmtLen: raw.StmtLen}, embedded, nil)
		if err != nil {
			completed = true
			return result, transactions.failEmbeddedSchema(ctx, err, automatic)
		}
		result.Warnings = append(result.Warnings, child.Warnings...)
	}
	if automatic {
		result.Commit, err = transactions.Commit(ctx, false)
		if err != nil {
			completed = true
			return result, err
		}
	}
	completed = true
	return result, nil
}

func embeddedSchemaInvocation(invocation StatementExecution, schema string) StatementExecution {
	result := invocation
	if invocation.Execution.ObjectDDL != nil {
		options := *invocation.Execution.ObjectDDL
		options.Binding.SchemaContext = schema
		result.Execution.ObjectDDL = &options
	}
	if invocation.Execution.RoutineDDL != nil {
		options := *invocation.Execution.RoutineDDL
		options.Binding.SchemaContext = schema
		result.Execution.RoutineDDL = &options
	}
	return result
}

func (transactions *Transactions) beginEmbeddedSchema(ctx context.Context) (bool, error) {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(operation, TransactionStart{}); err != nil {
			return false, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return false, err
	}
	return automatic, nil
}

func (transactions *Transactions) failEmbeddedSchema(ctx context.Context, cause error, automatic bool) error {
	operation, finish, err := transactions.enter(context.WithoutCancel(ctx))
	if err != nil {
		return errors.Join(cause, err)
	}
	defer finish()
	if transactions.active {
		transactions.markFailed(cause)
	}
	if automatic && transactions.active {
		cause = errors.Join(cause, transactions.endRollback(operation))
	}
	return cause
}

func qualifyEmbeddedSchemaObject(schema string, node *zpgparse.Node) error {
	var relation *zpgparse.RangeVar
	switch {
	case node == nil:
		return settingError("42601", "invalid CREATE SCHEMA element")
	case node.GetCreateStmt() != nil:
		relation = node.GetCreateStmt().Relation
	case node.GetIndexStmt() != nil:
		if node.GetIndexStmt().Concurrent {
			return settingError("25001", "CREATE INDEX CONCURRENTLY cannot be used in CREATE SCHEMA")
		}
		relation = node.GetIndexStmt().Relation
	case node.GetCreateSeqStmt() != nil:
		relation = node.GetCreateSeqStmt().Sequence
	case node.GetCreateTrigStmt() != nil:
		relation = node.GetCreateTrigStmt().Relation
	case node.GetViewStmt() != nil:
		relation = node.GetViewStmt().View
	case node.GetGrantStmt() != nil:
		return settingError("0A000", "authorization statements are not supported")
	default:
		return settingError("42601", "invalid CREATE SCHEMA element")
	}
	if relation == nil || relation.Relname == "" {
		return settingError("42601", "CREATE SCHEMA element has no object name")
	}
	if relation.Catalogname != "" {
		return settingError("0A000", "cross-database CREATE SCHEMA element is not supported")
	}
	if relation.Relpersistence == "t" {
		return settingError("42P16", "temporary objects cannot be CREATE SCHEMA elements")
	}
	if relation.Schemaname != "" && relation.Schemaname != schema {
		return settingError("42P06", "CREATE SCHEMA element names a different schema")
	}
	relation.Schemaname = schema
	return nil
}

func objectTableOptions(options ObjectCommandOptions) TableCommandOptions {
	return TableCommandOptions{Binding: options.Binding, Dependencies: options.Dependencies, Columns: options.Columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes, Constraints: options.Constraints}
}
func declarationSearchPath(path []string, schema string) []string {
	if schema == "" {
		return path
	}
	result := make([]string, 0, len(path)+1)
	result = append(result, schema)
	for _, name := range path {
		if name != schema {
			result = append(result, name)
		}
	}
	return result
}
func objectCommandRequiresPopulation(node *zpgparse.Node) bool {
	if create := node.GetCreateStmt(); create != nil {
		return create.Partbound != nil
	}
	if create := node.GetCreateTableAsStmt(); create != nil {
		return create.Into != nil && !create.Into.SkipData
	}
	if refresh := node.GetRefreshMatViewStmt(); refresh != nil {
		return !refresh.SkipData
	}
	if alter := node.GetAlterTableStmt(); alter != nil && alter.Objtype == zpgparse.ObjectType_OBJECT_TABLE && bind.HierarchyAlter(alter) && len(alter.Cmds) == 1 {
		command := alter.Cmds[0].GetAlterTableCmd()
		return command != nil && (command.Subtype == zpgparse.AlterTableType_AT_AttachPartition || command.Subtype == zpgparse.AlterTableType_AT_AddInherit)
	}
	return false
}

func (transactions *Transactions) populateObjectCommand(ctx context.Context, bound *bind.BoundObjectCommand, node *zpgparse.Node, invocation StatementExecution, admission *DeclarationResult) (affected uint64, err error) {
	environment := &invocation.Execution.Environment
	transferred := false
	defer func() {
		if !transferred {
			err = rejectPreparedEnvironment(*environment, err)
		}
	}()
	environment.Builtin = transactions.settings.BuiltinOptions(environment.Builtin.Collation, environment.Builtin.DefaultCollation)
	resources := transactions.settings.Resources()
	if err := environment.Query.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); err != nil {
		return 0, err
	}
	environment.Settings = transactions.settingRuntime(ctx)
	environment.Catalog, err = transactions.attachCatalogSettings(ctx, environment.Catalog)
	if err != nil {
		return 0, err
	}
	environment.Temporal = transactions.temporal
	parent, child, indexes, partition, attached, err := hierarchyAttachment(ctx, bound)
	if err != nil {
		return 0, err
	}
	if attached {
		if partition {
			return transactions.populatePartitionAttachment(ctx, parent, child, indexes, invocation)
		}
		return transactions.populateInheritanceAttachment(ctx, child, invocation)
	}
	target, statement, err := objectPopulationStatement(ctx, bound, node, invocation.Execution.ObjectDDL.Binding.Descriptors)
	if err != nil {
		return 0, err
	}
	if invocation.Execution.ObjectDDL != nil && invocation.Execution.ObjectDDL.RewriteQuery != nil {
		insert := statement.Stmt.GetInsertStmt()
		rewritten, err := invocation.Execution.ObjectDDL.RewriteQuery(ctx, &zpgparse.RawStmt{Stmt: insert.SelectStmt})
		if err != nil {
			return 0, err
		}
		insert.SelectStmt = rewritten.Stmt
	}
	// The population insert writes a relation this statement created, which the
	// committed catalog has not published yet. Admission already admitted that
	// relation's mutation target, so resolve through the admission result first
	// and fall back to the stack resolver for the committed relations a refresh
	// rewrites.
	if admission != nil && len(admission.targets) != 0 {
		environment.MutationTargets = populationMutationTargets{declared: admission, fallback: environment.MutationTargets}
	}
	// Population runs a nested INSERT ... SELECT as a statement of its own: its
	// binding resolves the view query's relations, types, operators and routines
	// by name through the catalog, and its planning reads the descriptors and
	// index metadata of every relation it may scan. The public path leaves the
	// invocation's own dependency list empty, so the object command's declared
	// catalog read set — the same set the enclosing declaration binds against —
	// covers the binding view, and the execution view adds the scan set the
	// bound statement's access paths negotiate.
	bindingDependencies := invocation.Execution.Dependencies
	if len(bindingDependencies) == 0 && invocation.Execution.ObjectDDL != nil {
		bindingDependencies = invocation.Execution.ObjectDDL.Dependencies
	}
	options := invocation.Execution
	options.planningOptions = func(statementContext context.Context, view txn.ReadView, builtins builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error) {
		return transactions.statementPlanning(statementContext, view, builtins, invocation)
	}
	execution := statementExecution{
		bind: func(statementContext context.Context, view txn.ReadView, builtins builtin.ResolveOptions) (*bind.BoundStatement, error) {
			snapshot, planning, err := options.planningOptions(statementContext, view, builtins)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			planning.Bind.Builtin = planning.Normalize.Builtin
			planning.Bind.MaterializedViewTarget = target.ID()
			return bind.Bind(statementContext, statement, snapshot, planning.Bind, nil)
		},
		prepareBound: func(statementContext context.Context, view txn.ReadView, builtins builtin.ResolveOptions, bound *bind.BoundStatement) (*Prepared, error) {
			snapshot, planning, err := options.planningOptions(statementContext, view, builtins)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, snapshot.Close()) }()
			planning.Bind.MaterializedViewTarget = target.ID()
			return prepareBound(statementContext, bound, planning)
		},
		bindingDependencies: bindingDependencies,
	}
	consume := func(context.Context, *Prepared, *Rows) error { return nil }
	transferred = true
	return runPreparedStatement(ctx, transactions, transactions.transaction, execution, options, consume, false)
}

// populationMutationTargets resolves the mutation target of a nested population
// statement. The declaration admitted by the enclosing object command owns the
// relations that statement created; every other relation is committed and comes
// from the stack resolver.
type populationMutationTargets struct {
	declared *DeclarationResult
	fallback execution.MutationTargets
}

func (targets populationMutationTargets) ResolveMutationTarget(ctx context.Context, ref storage.ObjectRef) (execution.MutationTarget, error) {
	if target, err := targets.declared.ResolveMutationTarget(ctx, ref); err == nil {
		return target, nil
	}
	if targets.fallback == nil {
		return execution.MutationTarget{}, storage.ErrInvalidTarget
	}
	return targets.fallback.ResolveMutationTarget(ctx, ref)
}

func objectPopulationStatement(ctx context.Context, bound *bind.BoundObjectCommand, node *zpgparse.Node, limits model.DescriptorLimits) (*model.Descriptor, *zpgparse.RawStmt, error) {
	if bound == nil || node == nil {
		return nil, nil, errPreparedInvocation
	}
	var target *model.Descriptor
	for _, descriptor := range bound.Creates() {
		if descriptor.Kind() == model.ObjectTable || descriptor.Kind() == model.ObjectMaterializedView {
			target = descriptor
			break
		}
	}
	if target == nil {
		for _, update := range bound.Updates() {
			if update.Next != nil && update.Next.Kind() == model.ObjectMaterializedView && update.Base.Physical().Generation != update.Next.Physical().Generation {
				target = update.Next
				break
			}
		}
	}
	if target == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	spec, err := target.SpecWithin(ctx, limits)
	if err != nil {
		return nil, nil, err
	}
	if spec.Table == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	var relation *zpgparse.RangeVar
	var query *zpgparse.Node
	if create := node.GetCreateTableAsStmt(); create != nil && create.Into != nil && create.Into.Rel != nil {
		relation = proto.Clone(create.Into.Rel).(*zpgparse.RangeVar)
		query = proto.Clone(create.Query).(*zpgparse.Node)
	} else if refresh := node.GetRefreshMatViewStmt(); refresh != nil && refresh.Relation != nil && spec.View != nil {
		relation = proto.Clone(refresh.Relation).(*zpgparse.RangeVar)
		raw := &zpgparse.RawStmt{}
		if err := proto.Unmarshal(spec.View.Query.Encoding, raw); err != nil || raw.Stmt == nil {
			return nil, nil, model.ErrInvalidDescriptor
		}
		query = raw.Stmt
	} else {
		return nil, nil, model.ErrInvalidDescriptor
	}
	relation.Inh = true
	relation.Relpersistence = spec.Table.Persistence
	columns := make([]*zpgparse.Node, 0, len(spec.Table.Columns))
	for _, column := range spec.Table.Columns {
		if column.State != model.StatePublic || column.SystemIdentity || column.Generated != nil {
			continue
		}
		columns = append(columns, &zpgparse.Node{Node: &zpgparse.Node_ResTarget{ResTarget: &zpgparse.ResTarget{Name: column.Name, Location: -1}}})
	}
	insert := &zpgparse.InsertStmt{Relation: relation, Cols: columns, SelectStmt: query, Override: zpgparse.OverridingKind_OVERRIDING_NOT_SET}
	return target, &zpgparse.RawStmt{Stmt: &zpgparse.Node{Node: &zpgparse.Node_InsertStmt{InsertStmt: insert}}}, nil
}

func isObjectCommand(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_CreateStmt, *zpgparse.Node_CreateSchemaStmt, *zpgparse.Node_CreateSeqStmt, *zpgparse.Node_AlterSeqStmt, *zpgparse.Node_ViewStmt, *zpgparse.Node_CreateTableAsStmt, *zpgparse.Node_RefreshMatViewStmt, *zpgparse.Node_TruncateStmt:
		return true
	case *zpgparse.Node_IndexStmt:
		return statement.IndexStmt != nil && !statement.IndexStmt.Concurrent
	case *zpgparse.Node_AlterTableStmt:
		return bind.HierarchyAlter(statement.AlterTableStmt) || bind.OrdinaryObjectAlter(statement.AlterTableStmt) || bind.IdentityDeclarationAlter(statement.AlterTableStmt) || bind.TransactionalGenerationAlter(statement.AlterTableStmt) || bind.SchemaJobAlter(statement.AlterTableStmt) || bind.TriggerStateAlter(statement.AlterTableStmt)
	case *zpgparse.Node_ReindexStmt:
		return statement.ReindexStmt != nil
	case *zpgparse.Node_DropStmt:
		if statement.DropStmt == nil || statement.DropStmt.Concurrent {
			return false
		}
		if bind.TextSearchLifecycleKind(node) {
			return true
		}
		switch statement.DropStmt.RemoveType {
		case zpgparse.ObjectType_OBJECT_SCHEMA, zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_INDEX, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW:
			return true
		}
	case *zpgparse.Node_RenameStmt:
		if statement.RenameStmt == nil {
			return false
		}
		if bind.TextSearchLifecycleKind(node) {
			return true
		}
		if statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_TABCONSTRAINT {
			return true
		}
		switch statement.RenameStmt.RenameType {
		case zpgparse.ObjectType_OBJECT_SCHEMA, zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_INDEX, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW, zpgparse.ObjectType_OBJECT_COLUMN:
			return true
		}
	case *zpgparse.Node_AlterObjectSchemaStmt:
		if statement.AlterObjectSchemaStmt == nil {
			return false
		}
		if bind.TextSearchLifecycleKind(node) {
			return true
		}
		switch statement.AlterObjectSchemaStmt.ObjectType {
		case zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW:
			return true
		}
	case *zpgparse.Node_CommentStmt:
		if statement.CommentStmt == nil {
			return false
		}
		if bind.DescribedObjectCommentKind(statement.CommentStmt.Objtype) {
			return true
		}
		switch statement.CommentStmt.Objtype {
		case zpgparse.ObjectType_OBJECT_SCHEMA, zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_INDEX, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW, zpgparse.ObjectType_OBJECT_COLUMN:
			return true
		}
	}
	return false
}

func ExecuteObjectCommand(ctx context.Context, transactions *Transactions, node *zpgparse.Node, options ObjectCommandOptions) (result ObjectCommandResult, err error) {
	if transactions == nil || node == nil || options.MaxDescriptors == 0 {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if transactions.sequences == nil {
		return result, transactions.fail(settingError("0A000", "catalog DDL services are not configured"))
	}
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(operation, TransactionStart{}); err != nil {
			return result, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	complete := false
	defer func() {
		if !complete {
			transactions.markFailed(errSessionStatementPanic)
			if automatic {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	err = transactions.routineAtomicStatement(operation, func(ctx context.Context) (statementErr error) {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute object DDL in a read-only transaction")
		}
		temporaryMark := len(transactions.named.temporaryChanges)
		defer func() {
			if statementErr != nil {
				transactions.named.rollbackTemporary(temporaryMark)
			}
		}()
		options.Binding.Bind.SearchPath = declarationSearchPath(transactions.settings.SearchPath(), options.Binding.SchemaContext)
		options.Binding.Owner = transactions.settings.Principal().OID()
		options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = transactions.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
		options.Binding.Encoder = transactions.options.KeyEncoder
		options.Binding.CurrentSetting = func(name string) (string, error) {
			value, err := transactions.settings.Show(name)
			if err != nil {
				return "", err
			}
			if value.IsNull || value.Type != model.TypeString {
				return "", settingError("22023", "session setting has no textual representation")
			}
			return value.StrVal, nil
		}
		commandNode := node
		if temporaryObjectNode(node) {
			commandNode = proto.Clone(node).(*zpgparse.Node)
			if err := qualifyTemporaryObject(commandNode); err != nil {
				return err
			}
			if err := transactions.ensureTemporarySchema(ctx, &options.Binding); err != nil {
				return err
			}
		}
		if len(transactions.named.temporary) != 0 && !slices.Contains(options.Binding.Bind.SearchPath, temporarySchemaName) {
			options.Binding.Bind.SearchPath = append([]string{temporarySchemaName}, options.Binding.Bind.SearchPath...)
		}
		bound, err := transactions.bindObjectCommand(ctx, commandNode, options)
		if err != nil {
			return err
		}
		result.Tag, result.Warnings = bound.CommandTag(), bound.Warnings()
		if len(bound.Creates()) == 0 && len(bound.Updates()) == 0 && len(bound.Drops()) == 0 {
			if len(bound.Warnings()) == 0 {
				return model.ErrInvalidDescriptor
			}
			return nil
		}
		temporaryCommand := objectCommandTemporary(ctx, bound, options.Binding.Descriptors)
		lockedForValidation := false
		if len(bound.Validations()) != 0 {
			if !temporaryCommand {
				if err := transactions.lockObjectCommandDescriptors(ctx, bound); err != nil {
					return err
				}
				lockedForValidation = true
			}
			if err := transactions.validateObjectConstraints(ctx, bound, options); err != nil {
				return err
			}
		}
		var admission *DeclarationResult
		var stageErr error
		if temporaryCommand {
			transactions.named.temporaryOptions = &options
			if err := transactions.named.applyTemporaryCommand(bound); err != nil {
				return err
			}
			stageErr = transactions.stageTemporaryObjectCommand(ctx, bound, options)
		} else {
			if !lockedForValidation {
				if err := transactions.lockObjectCommandDescriptors(ctx, bound); err != nil {
					return err
				}
			}
			admission, stageErr = transactions.stageObjectCommandAdmission(ctx, bound, options)
		}
		if stageErr != nil {
			return stageErr
		}
		if bound.Populate() {
			if options.populate == nil {
				return errPreparedInvocation
			}
			result.RowsAffected, stageErr = options.populate(ctx, bound, admission)
			return stageErr
		}
		return nil
	})
	err = errors.Join(err, transactions.markDataAccess())
	complete = true
	if err != nil {
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
	}
	return result, err
}
func (transactions *Transactions) lockObjectCommandDescriptors(ctx context.Context, command *bind.BoundObjectCommand) error {
	for _, update := range command.Updates() {
		if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, update.Base.ID()); err != nil {
			return err
		}
	}
	for _, descriptor := range command.Drops() {
		if err := transactions.sequences.config.Catalog.LockDescriptor(ctx, transactions.transaction, descriptor.ID()); err != nil {
			return err
		}
	}
	return nil
}

func (transactions *Transactions) validateObjectConstraints(ctx context.Context, command *bind.BoundObjectCommand, options ObjectCommandOptions) (err error) {
	if command == nil || options.Constraints == nil {
		return settingError("22023", "constraint validation services are not configured")
	}
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range command.Dependencies() {
		if transactions.named.temporary[dependency.Object] == nil {
			dependencies = append(dependencies, transactions.sequences.config.Catalog.DescriptorDependency(dependency.Object))
		}
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return err
	}
	overlays := command.Creates()
	for _, update := range command.Updates() {
		if update.Next != nil {
			overlays = append(overlays, update.Next)
		}
	}
	if len(overlays) != 0 {
		if err := snapshot.AddSessionOverlay(overlays); err != nil {
			return err
		}
	}
	validationIndexes := make([]*model.Descriptor, 0, len(command.Creates()))
	for _, descriptor := range command.Creates() {
		if descriptor != nil && descriptor.Kind() == model.ObjectIndex {
			validationIndexes = append(validationIndexes, descriptor)
		}
	}
	for _, validation := range command.Validations() {
		var update *bind.ObjectMutation
		for _, candidate := range command.Updates() {
			if candidate.Base == nil || candidate.Next == nil || candidate.Next.Kind() != model.ObjectTable {
				continue
			}
			spec, specErr := candidate.Next.SpecWithin(ctx, options.Binding.Descriptors)
			if specErr != nil {
				return specErr
			}
			if spec.Table == nil {
				return model.ErrInvalidDescriptor
			}
			if slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.ID == validation }) {
				copy := candidate
				update = &copy
				break
			}
		}
		if update == nil {
			return model.ErrInvalidDescriptor
		}
		source, sourceVersion := update.Base, update.Base.Version()
		if update.Base.Physical() != update.Next.Physical() {
			source, sourceVersion = update.Next, update.Next.Version()
		}
		if err := options.Constraints.ValidateConstraintWithIndexes(ctx, transactions.transaction, snapshot, sourceVersion, source, update.Next, validation, validationIndexes); err != nil {
			return err
		}
	}
	return nil
}

func (transactions *Transactions) bindObjectCommand(ctx context.Context, node *zpgparse.Node, options ObjectCommandOptions) (*bind.BoundObjectCommand, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	bound, handled, bindErr := bind.BindObjectCommand(ctx, node, snapshot, options.Binding)
	if !handled && bindErr == nil {
		bindErr = errPreparedInvocation
	}
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func objectCommandTemporary(ctx context.Context, bound *bind.BoundObjectCommand, limits model.DescriptorLimits) bool {
	if bound == nil {
		return false
	}
	temporary := func(descriptor *model.Descriptor) bool {
		spec, err := descriptor.SpecWithin(ctx, limits)
		if err != nil {
			return false
		}
		return spec.Table != nil && spec.Table.Persistence == "t" || spec.Sequence != nil && spec.Sequence.Persistence == "t" || spec.View != nil && spec.View.Persistence == "t"
	}
	for _, descriptor := range bound.Creates() {
		if temporary(descriptor) {
			return true
		}
	}
	for _, update := range bound.Updates() {
		if temporary(update.Next) {
			return true
		}
	}
	for _, descriptor := range bound.Drops() {
		if temporary(descriptor) {
			return true
		}
	}
	return false
}
func (transactions *Transactions) stageTemporaryObjectCommand(ctx context.Context, bound *bind.BoundObjectCommand, options ObjectCommandOptions) error {
	if bound == nil {
		return errPreparedInvocation
	}
	store := transactions.sequences.config.Catalog
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range bound.Dependencies() {
		if !transactions.named.isTemporary(dependency.Object) {
			dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
		}
	}
	if err := transactions.stageTemporarySequenceDrops(ctx, bound.Drops()); err != nil {
		return err
	}
	retired := append(objectUpdateRetirements(bound.Updates()), objectDescriptorRetirements(bound.Drops())...)
	retirementID, err := transactions.prepareObjectRetirement(ctx, retired)
	if err != nil {
		return err
	}
	if retirementID != 0 {
		dependency, err := transactions.sequences.config.Identities.RetirementDependency(retirementID)
		if err != nil {
			return err
		}
		dependencies = append(dependencies, dependency)
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	err = func() error {
		if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
			return err
		}
		for _, dependency := range bound.Dependencies() {
			if transactions.named.isTemporary(dependency.Object) {
				continue
			}
			published, err := snapshot.Descriptor(ctx, dependency.Object)
			if err != nil {
				return err
			}
			if published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
				return catalog.ErrDescriptorChanged
			}
		}
		if objectDeclarationNeedsAdmission(bound) {
			descriptors := bound.Creates()
			for _, update := range bound.Updates() {
				if update.Base != nil && update.Next != nil && update.Base.Physical().Generation != update.Next.Physical().Generation {
					descriptors = append(descriptors, update.Next)
				}
			}
			columns := options.Columns
			columns.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
			columns.Temporal = transactions.temporal
			admitted, err := admitDeclaration(ctx, transactions.transaction, snapshot, descriptors, DeclarationOptions{Catalog: store, Encoder: transactions.options.KeyEncoder, Columns: columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes})
			if err != nil {
				return err
			}
			transactions.named.setTemporaryTargets(admitted)
		}
		if retirementID != 0 {
			return transactions.sequences.config.Identities.StageRetirements(ctx, transactions.transaction, retirementID, retired)
		}
		return nil
	}()
	return errors.Join(err, snapshot.Close(), view.Close())
}

func (transactions *Transactions) stageObjectCommand(ctx context.Context, bound *bind.BoundObjectCommand, options ObjectCommandOptions) error {
	_, err := transactions.stageObjectCommandAdmission(ctx, bound, options)
	return err
}

// stageObjectCommandAdmission stages one object command and reports the
// declaration admission result, which owns the mutation targets of the
// relations the command created.
func (transactions *Transactions) stageObjectCommandAdmission(ctx context.Context, bound *bind.BoundObjectCommand, options ObjectCommandOptions) (*DeclarationResult, error) {
	if bound == nil {
		return nil, errPreparedInvocation
	}
	store := transactions.sequences.config.Catalog
	if declaration, ok := bound.Declaration(); ok && objectDeclarationNeedsAdmission(bound) {
		retired := objectUpdateRetirements(bound.Updates())
		if err := transactions.acquireObjectRetirement(ctx, retired); err != nil {
			return nil, err
		}
		columns := options.Columns
		columns.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		columns.Temporal = transactions.temporal
		return StageDeclaration(ctx, transactions.transaction, declaration, DeclarationOptions{Catalog: store, Encoder: transactions.options.KeyEncoder, Columns: columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes})
	}
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range bound.Dependencies() {
		dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
	}
	for _, descriptor := range bound.Creates() {
		reads, err := store.CreateDependencies(ctx, descriptor)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, reads...)
	}
	for _, update := range bound.Updates() {
		dependencies = append(dependencies, store.DescriptorDependency(update.Base.ID()))
	}
	for _, descriptor := range bound.Drops() {
		dependencies = append(dependencies, store.DescriptorDependency(descriptor.ID()))
	}
	plannedDrops, err := transactions.planObjectDrops(ctx, dependencies, bound, options.MaxDescriptors)
	if err != nil {
		return nil, err
	}
	retired := append(objectUpdateRetirements(bound.Updates()), objectDescriptorRetirements(plannedDrops)...)
	retirementID, err := transactions.prepareObjectRetirement(ctx, retired)
	if err != nil {
		return nil, err
	}
	if retirementID != 0 {
		dependency, err := transactions.sequences.config.Identities.RetirementDependency(retirementID)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	for _, descriptor := range plannedDrops {
		dependencies = append(dependencies, store.DescriptorDependency(descriptor.ID()))
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	var admission *DeclarationResult
	err = func() error {
		for _, dependency := range bound.Dependencies() {
			published, err := snapshot.Descriptor(ctx, dependency.Object)
			if err != nil {
				return err
			}
			if published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
				return catalog.ErrDescriptorChanged
			}
		}
		updates := bound.Updates()
		catalogUpdates := make([]catalog.ObjectUpdate, len(updates))
		for i := range updates {
			catalogUpdates[i] = catalog.ObjectUpdate{Base: updates[i].Base, Next: updates[i].Next}
		}
		changes, _, err := store.ApplyObjectChanges(ctx, snapshot, catalog.ObjectChangeSet{Creates: bound.Creates(), Updates: catalogUpdates, Drops: bound.Drops(), Cascade: bound.Cascade(), MaxDescriptors: options.MaxDescriptors})
		if err != nil {
			return err
		}
		var mutations []storage.Mutation
		for _, change := range changes {
			mutations = append(mutations, change.Mutations...)
		}
		if len(mutations) != 0 {
			if err := transactions.transaction.ApplyMutations(ctx, mutations); err != nil {
				return err
			}
		}
		if objectDeclarationNeedsAdmission(bound) {
			overlay := bound.Creates()
			for _, update := range bound.Updates() {
				overlay = append(overlay, update.Next)
			}
			if err := snapshot.AddSessionOverlay(overlay); err != nil {
				return err
			}
			columns := options.Columns
			columns.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
			columns.Temporal = transactions.temporal
			admitted, err := admitDeclaration(ctx, transactions.transaction, snapshot, overlay, DeclarationOptions{Catalog: store, Encoder: transactions.options.KeyEncoder, Columns: columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes})
			if err != nil {
				return err
			}
			admission = admitted
		}
		if retirementID != 0 {
			return transactions.sequences.config.Identities.StageRetirements(ctx, transactions.transaction, retirementID, retired)
		}
		return nil
	}()
	return admission, errors.Join(err, snapshot.Close(), view.Close())
}

func objectDeclarationNeedsAdmission(bound *bind.BoundObjectCommand) bool {
	for _, descriptor := range bound.Creates() {
		if descriptor.Kind() == model.ObjectTable || descriptor.Kind() == model.ObjectMaterializedView {
			return true
		}
	}
	for _, update := range bound.Updates() {
		if update.Base != nil && update.Next != nil && update.Base.Physical().Generation != update.Next.Physical().Generation {
			return true
		}
	}
	return false
}

func (transactions *Transactions) planObjectDrops(ctx context.Context, dependencies []txn.ReadDependency, bound *bind.BoundObjectCommand, maximum uint32) ([]*model.Descriptor, error) {
	if len(bound.Drops()) == 0 {
		return nil, nil
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	store := transactions.sequences.config.Catalog
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	planned, planErr := store.PlanObjectDrops(ctx, snapshot, bound.Drops(), bound.Cascade(), maximum)
	return planned, errors.Join(planErr, snapshot.Close(), view.Close())
}

func objectUpdateRetirements(updates []bind.ObjectMutation) []storage.ObjectGeneration {
	var retired []storage.ObjectGeneration
	for _, update := range updates {
		if update.Base == nil || update.Next == nil {
			continue
		}
		before, after := update.Base.Physical(), update.Next.Physical()
		if before.Generation != 0 && before.Generation != after.Generation {
			retired = append(retired, storage.ObjectGeneration{ObjectID: update.Base.ID(), Generation: before.Generation})
		}
	}
	return retired
}

func objectDescriptorRetirements(descriptors []*model.Descriptor) []storage.ObjectGeneration {
	var retired []storage.ObjectGeneration
	for _, descriptor := range descriptors {
		if descriptor == nil {
			continue
		}
		physical := descriptor.Physical()
		if physical.Generation != 0 {
			retired = append(retired, storage.ObjectGeneration{ObjectID: descriptor.ID(), Generation: physical.Generation})
		}
	}
	return retired
}

func (transactions *Transactions) acquireObjectRetirement(ctx context.Context, retired []storage.ObjectGeneration) error {
	id, err := transactions.prepareObjectRetirement(ctx, retired)
	if err != nil || id == 0 {
		return err
	}
	dependency, err := transactions.sequences.config.Identities.RetirementDependency(id)
	if err != nil {
		return err
	}
	view, err := transactions.transaction.ReadView(ctx, []txn.ReadDependency{dependency})
	if err != nil {
		return err
	}
	if err := view.Close(); err != nil {
		return err
	}
	return transactions.sequences.config.Identities.StageRetirements(ctx, transactions.transaction, id, retired)
}

func (transactions *Transactions) prepareObjectRetirement(ctx context.Context, retired []storage.ObjectGeneration) (schema.RetirementID, error) {
	if len(retired) == 0 {
		return 0, nil
	}
	allocation, err := transactions.sequences.config.Identities.AllocateIdentity(ctx, transactions.transaction)
	return schema.RetirementID(allocation.ObjectID), err
}
