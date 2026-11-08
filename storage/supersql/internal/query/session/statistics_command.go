package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	cpstats "github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type StatisticsCommandOptions struct {
	Catalog      *catalog.Store
	Store        *cpstats.Store
	KeyEncoder   storage.KeyEncoder
	TypeRuntime  *DeclaredTypeRuntimeOptions
	Binding      bind.CreateDeclarationOptions
	Expressions  IndexExpressionOptions
	Analyze      cpstats.AnalyzeOptions
	Dependencies []txn.ReadDependency
}

type StatisticsCommandResult struct {
	Tag      string
	Warnings []model.Error
	Commit   EndResult
}

func (transactions *Transactions) runStatisticsCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil || !isStatisticsCommand(statement.Stmt) {
		return ExecutionResult{}, false, nil
	}
	if options.Execution.Statistics == nil {
		return ExecutionResult{}, true, errPreparedInvocation
	}
	if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "statistics command does not accept parameters"), rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	if err := rejectPreparedEnvironment(options.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	command := *options.Execution.Statistics
	command.TypeRuntime = options.Execution.TypeRuntime
	result, err := ExecuteStatisticsCommand(ctx, transactions, statement.Stmt, command)
	result.Warnings = transactions.settings.visibleWarnings(result.Warnings)
	return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
}

func isStatisticsCommand(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_CreateStatsStmt, *zpgparse.Node_AlterStatsStmt:
		return true
	case *zpgparse.Node_RenameStmt:
		return statement.RenameStmt != nil && statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_STATISTIC_EXT
	case *zpgparse.Node_AlterObjectSchemaStmt:
		return statement.AlterObjectSchemaStmt != nil && statement.AlterObjectSchemaStmt.ObjectType == zpgparse.ObjectType_OBJECT_STATISTIC_EXT
	case *zpgparse.Node_DropStmt:
		return statement.DropStmt != nil && statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_STATISTIC_EXT
	case *zpgparse.Node_VacuumStmt:
		return statement.VacuumStmt != nil && !statement.VacuumStmt.IsVacuumcmd
	default:
		return false
	}
}

func ExecuteStatisticsCommand(ctx context.Context, transactions *Transactions, node *zpgparse.Node, options StatisticsCommandOptions) (result StatisticsCommandResult, err error) {
	if transactions == nil || node == nil || options.Catalog == nil || options.Store == nil || options.KeyEncoder == nil {
		return result, errSessionTransactionConfig
	}
	if err := options.Binding.Descriptors.Validate(); err != nil {
		return result, err
	}
	if err := options.Analyze.Limits.Validate(); err != nil {
		return result, err
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if transactions.sequences == nil || transactions.sequences.config.Catalog != options.Catalog {
		return result, transactions.fail(settingError("0A000", "statistics catalog services are not configured"))
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
	err = transactions.routineAtomicStatement(operation, func(ctx context.Context) error {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute statistics commands in a read-only transaction")
		}
		options.Binding.Bind.SearchPath = transactions.settings.SearchPath()
		options.Binding.Owner = transactions.settings.Principal().OID()
		options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = transactions.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
		resources := transactions.settings.Resources()
		if resources.EffectiveMaintenanceMemoryBytes <= 0 {
			return settingError("53200", "ANALYZE has no maintenance memory")
		}
		memory := uint64(resources.EffectiveMaintenanceMemoryBytes)
		options.Binding.Bind.Limits.MaxBytes = min(options.Binding.Bind.Limits.MaxBytes, memory)
		options.Binding.Descriptors.MaxBytes = min(options.Binding.Descriptors.MaxBytes, memory)
		options.Analyze.MaxLiveBytes = min(options.Analyze.MaxLiveBytes, memory)
		options.Analyze.MaxReservoirBytes = min(options.Analyze.MaxReservoirBytes, memory)
		options.Analyze.Limits.MaxBytes = min(options.Analyze.Limits.MaxBytes, memory)
		if vacuum := node.GetVacuumStmt(); vacuum != nil {
			bindingDependencies := options.Dependencies
			skipLocked, err := bind.AnalyzeSkipLocked(vacuum)
			if err != nil {
				return err
			}
			if skipLocked {
				var empty bool
				vacuum, bindingDependencies, empty, err = transactions.admitAnalyzeRelations(ctx, vacuum, options)
				if err != nil {
					return err
				}
				if empty {
					result.Tag = "ANALYZE"
					return nil
				}
			}
			bound, err := transactions.bindAnalyze(ctx, vacuum, options, bindingDependencies)
			if err != nil {
				return err
			}
			result.Tag = "ANALYZE"
			result.Warnings, err = transactions.executeAnalyze(ctx, bound, options)
			return err
		}
		bound, err := transactions.bindStatisticsDDL(ctx, node, options)
		if err != nil {
			return err
		}
		result.Tag, result.Warnings = bound.CommandTag(), bound.Warnings()
		for _, update := range bound.Updates() {
			if err := options.Catalog.LockDescriptor(ctx, transactions.transaction, update.Base.ID()); err != nil {
				return err
			}
		}
		return transactions.stageStatisticsDDL(ctx, bound, options)
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

func (transactions *Transactions) bindStatisticsDDL(ctx context.Context, node *zpgparse.Node, options StatisticsCommandOptions) (*bind.BoundStatisticsCommand, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	bound, handled, bindErr := bind.BindStatisticsCommand(ctx, node, snapshot, options.Binding)
	if !handled && bindErr == nil {
		bindErr = errPreparedInvocation
	}
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) stageStatisticsDDL(ctx context.Context, bound *bind.BoundStatisticsCommand, options StatisticsCommandOptions) error {
	if bound == nil {
		return errPreparedInvocation
	}
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range bound.Dependencies() {
		dependencies = append(dependencies, options.Catalog.DescriptorDependency(dependency.Object))
	}
	for _, update := range bound.Updates() {
		dependencies = append(dependencies, options.Catalog.DescriptorDependency(update.Base.ID()))
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
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
		var mutations []storage.Mutation
		for _, update := range bound.Updates() {
			change, err := options.Catalog.Update(ctx, snapshot, update.Base.Version(), update.Next)
			if err != nil {
				return err
			}
			mutations = append(mutations, change.Mutations...)
		}
		if len(mutations) == 0 {
			return nil
		}
		return transactions.transaction.ApplyMutations(ctx, mutations)
	}()
	return errors.Join(err, snapshot.Close(), view.Close())
}

func (transactions *Transactions) bindAnalyze(ctx context.Context, statement *zpgparse.VacuumStmt, options StatisticsCommandOptions, dependencies []txn.ReadDependency) (*bind.BoundAnalyze, error) {
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	bound, bindErr := bind.BindAnalyze(ctx, statement, snapshot, options.Binding)
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) admitAnalyzeRelations(ctx context.Context, statement *zpgparse.VacuumStmt, options StatisticsCommandOptions) (*zpgparse.VacuumStmt, []txn.ReadDependency, bool, error) {
	maximum := options.Binding.Bind.Limits.MaxNodes
	rootDependency, err := options.Catalog.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return nil, nil, false, err
	}
	schemas, acquired, err := transactions.scanAnalyzeNamespaceReferences(ctx, options.Catalog, []txn.ReadDependency{rootDependency}, 0, model.ObjectSchema, maximum)
	if err != nil {
		return nil, nil, false, err
	}
	if !acquired {
		restricted := proto.Clone(statement).(*zpgparse.VacuumStmt)
		restricted.Rels = nil
		return restricted, nil, true, nil
	}
	dependencies := []txn.ReadDependency{rootDependency}
	tableDependencies := make([]txn.ReadDependency, 0, len(schemas))
	for _, schema := range schemas {
		dependency, err := options.Catalog.NamespaceScanDependency(ctx, schema.ID, model.ObjectTable)
		if err != nil {
			return nil, nil, false, err
		}
		tableDependencies = append(tableDependencies, dependency)
		dependencies = append(dependencies, dependency)
	}
	tables := make([]catalog.NamespaceReference, 0)
	if len(tableDependencies) != 0 {
		view, acquired, err := transactions.transaction.ReadViewSkipLocked(ctx, tableDependencies)
		if err != nil {
			return nil, nil, false, err
		}
		if !acquired {
			restricted := proto.Clone(statement).(*zpgparse.VacuumStmt)
			restricted.Rels = nil
			return restricted, dependencies, true, nil
		}
		snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return nil, nil, false, errors.Join(err, view.Close())
		}
		for _, schema := range schemas {
			err = snapshot.ScanNamespaceReferences(ctx, schema.ID, model.ObjectTable, func(reference catalog.NamespaceReference) error {
				if uint32(len(schemas)+len(tables)) >= maximum {
					return settingError("54000", "ANALYZE relation admission limit exceeded")
				}
				tables = append(tables, reference)
				return nil
			})
			if err != nil {
				break
			}
		}
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return nil, nil, false, err
		}
	}
	schemaByName := make(map[string]catalog.NamespaceReference, len(schemas))
	for _, schema := range schemas {
		schemaByName[schema.Name.Name] = schema
	}
	schemaByID := make(map[model.ObjectID]catalog.NamespaceReference, len(schemas))
	for _, schema := range schemas {
		schemaByID[schema.ID] = schema
	}
	tableByName := make(map[model.NamespaceName]catalog.NamespaceReference, len(tables))
	for _, table := range tables {
		tableByName[table.Name] = table
	}
	admit := func(schema catalog.NamespaceReference, table catalog.NamespaceReference) (bool, error) {
		acquired, err := options.Catalog.LockDescriptorPairSkipLocked(ctx, transactions.transaction, schema.ID, table.ID)
		if err != nil || !acquired {
			return acquired, err
		}
		dependencies = append(dependencies, options.Catalog.DescriptorDependency(schema.ID), options.Catalog.DescriptorDependency(table.ID))
		return true, nil
	}
	restricted := proto.Clone(statement).(*zpgparse.VacuumStmt)
	restricted.Rels = nil
	if len(statement.Rels) == 0 {
		for _, table := range tables {
			schema, ok := schemaByID[table.Name.Parent]
			if !ok {
				return nil, nil, false, catalog.ErrCorruptCatalog
			}
			acquired, err := admit(schema, table)
			if err != nil {
				return nil, nil, false, err
			}
			if acquired {
				restricted.Rels = append(restricted.Rels, analyzeVacuumRelation(schema.Name.Name, table.Name.Name))
			}
		}
	} else {
		for _, node := range statement.Rels {
			relation := node.GetVacuumRelation()
			if relation == nil || relation.Relation == nil {
				restricted.Rels = append(restricted.Rels, node)
				continue
			}
			rangeVar := relation.Relation
			var schema catalog.NamespaceReference
			var table catalog.NamespaceReference
			found := false
			if rangeVar.Schemaname != "" {
				schema, found = schemaByName[rangeVar.Schemaname]
				if found {
					table, found = tableByName[model.NamespaceName{Parent: schema.ID, Kind: model.ObjectTable, Name: rangeVar.Relname}]
				}
			} else {
				for _, schemaName := range options.Binding.Bind.SearchPath {
					candidateSchema, ok := schemaByName[schemaName]
					if !ok {
						continue
					}
					candidateTable, ok := tableByName[model.NamespaceName{Parent: candidateSchema.ID, Kind: model.ObjectTable, Name: rangeVar.Relname}]
					if ok {
						schema, table, found = candidateSchema, candidateTable, true
						break
					}
				}
			}
			if !found {
				restricted.Rels = append(restricted.Rels, node)
				continue
			}
			acquired, err := admit(schema, table)
			if err != nil {
				return nil, nil, false, err
			}
			if acquired {
				restricted.Rels = append(restricted.Rels, node)
			}
		}
	}
	return restricted, dependencies, len(restricted.Rels) == 0, ctx.Err()
}

func (transactions *Transactions) scanAnalyzeNamespaceReferences(ctx context.Context, store *catalog.Store, dependencies []txn.ReadDependency, parent model.ObjectID, kind model.ObjectKind, maximum uint32) ([]catalog.NamespaceReference, bool, error) {
	view, acquired, err := transactions.transaction.ReadViewSkipLocked(ctx, dependencies)
	if err != nil || !acquired {
		return nil, acquired, err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, false, errors.Join(err, view.Close())
	}
	references := make([]catalog.NamespaceReference, 0)
	scanErr := snapshot.ScanNamespaceReferences(ctx, parent, kind, func(reference catalog.NamespaceReference) error {
		if uint32(len(references)) >= maximum {
			return settingError("54000", "ANALYZE namespace admission limit exceeded")
		}
		references = append(references, reference)
		return nil
	})
	return references, true, errors.Join(scanErr, snapshot.Close(), view.Close())
}

func analyzeVacuumRelation(schema, relation string) *zpgparse.Node {
	return &zpgparse.Node{Node: &zpgparse.Node_VacuumRelation{VacuumRelation: &zpgparse.VacuumRelation{Relation: &zpgparse.RangeVar{Schemaname: schema, Relname: relation, Location: -1}}}}
}

func (transactions *Transactions) executeAnalyze(ctx context.Context, bound *bind.BoundAnalyze, options StatisticsCommandOptions) ([]model.Error, error) {
	if bound == nil {
		return nil, errPreparedInvocation
	}
	targets := bound.Targets()
	admitted := targets
	var dependencies []txn.ReadDependency
	if !bound.SkipLocked() {
		dependencies = slicesCloneReadDependencies(options.Dependencies)
	}
	for _, dependency := range bound.Dependencies() {
		dependencies = append(dependencies, options.Catalog.DescriptorDependency(dependency.Object))
	}
	for _, target := range admitted {
		topology, err := options.Catalog.NamespaceScanDependency(ctx, target.Table.Name().Parent, model.ObjectIndex)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, topology)
		spec, err := target.Table.SpecWithin(ctx, options.Binding.Descriptors)
		if err != nil {
			return nil, err
		}
		for _, dependency := range spec.Dependencies {
			dependencies = append(dependencies, options.Catalog.DescriptorDependency(dependency.Object))
		}
		physical, keyLayout, _, err := analyzeTableLayout(ctx, target.Table, options.Binding.Descriptors)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, txn.ReadDependency{Target: physical, KeyLayout: keyLayout, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
		publication, err := options.Store.PublicationDependency(physical)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, publication)
		for _, index := range target.Indexes {
			spec, err := index.SpecWithin(ctx, options.Binding.Descriptors)
			if err != nil {
				return nil, err
			}
			for _, dependency := range spec.Dependencies {
				dependencies = append(dependencies, options.Catalog.DescriptorDependency(dependency.Object))
			}
			if !indexHasSelectedExpression(index, target.Columns) {
				continue
			}
			indexTarget, err := analyzeDescriptorTarget(index)
			if err != nil {
				return nil, err
			}
			publication, err := options.Store.PublicationDependency(indexTarget)
			if err != nil {
				return nil, err
			}
			dependencies = append(dependencies, publication)
		}
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	var warnings []model.Error
	err = func() error {
		compiler, err := NewIndexExpressionCompiler(snapshot, options.Expressions)
		if err != nil {
			return err
		}
		keyEncoder := options.KeyEncoder
		if encoder, ok := options.KeyEncoder.(*builtin.KeyEncoder); ok {
			resolve := builtin.ResolveOptions{
				InformationSchemaFunctions: options.Binding.Bind.CatalogRelations,
				InformationSchemaTypes:     options.Binding.Bind.CatalogRelations,
			}
			if options.TypeRuntime != nil {
				runtimeOptions := *options.TypeRuntime
				runtimeOptions.Expressions.Builtin = resolve
				types, err := transactions.declaredTypeRuntime(ctx, snapshot, runtimeOptions)
				if err != nil {
					return err
				}
				resolve.Types = types
			}
			keyEncoder = encoder.WithResolveOptions(resolve)
		}
		var mutations []storage.Mutation
		type report struct {
			table      string
			liveRows   uint64
			sampleRows uint64
			epoch      uint64
		}
		var reports []report
		for _, requested := range admitted {
			published, err := snapshot.Descriptor(ctx, requested.Table.ID())
			if err != nil {
				return err
			}
			if published.Descriptor.Version() != requested.Table.Version() || published.Descriptor.State() != model.StatePublic {
				return catalog.ErrDescriptorChanged
			}
			physical, keyLayout, layout, err := analyzeTableLayout(ctx, published.Descriptor, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			current, err := options.Store.Snapshot(view, view.Timestamp())
			if err != nil {
				return err
			}
			old, found, err := current.Lookup(ctx, physical)
			if err != nil {
				return err
			}
			epoch := uint64(1)
			if found {
				if old.Epoch == math.MaxUint64 {
					return cpstats.ErrLimit
				}
				epoch = old.Epoch + 1
			}
			extended, err := executableExtendedStatistics(ctx, published.Descriptor, layout, requested.Columns, snapshot, compiler, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			indexes, indexOld, err := executableIndexStatistics(ctx, requested, layout, snapshot, compiler, current, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			analyze := options.Analyze
			analyze.Target, analyze.Snapshot, analyze.Epoch = physical, view.Timestamp(), epoch
			analyze.Layout, analyze.KeyLayout, analyze.Extended, analyze.Indexes = layout, keyLayout, extended, indexes
			analyze.ColumnTargets = analyzeColumnTargets(published.Descriptor, requested.Columns)
			result, err := cpstats.AnalyzeBundle(ctx, view, keyEncoder, analyze)
			if err != nil {
				return err
			}
			next := result.Table
			if found && len(requested.Columns) != 0 {
				mergeSelectedStatistics(next, old, requested.Columns, extended)
			}
			change, err := options.Store.Publish(ctx, current, next)
			if err != nil {
				return err
			}
			mutations = append(mutations, change.Mutations...)
			for i, index := range result.Indexes {
				previous := indexOld[i]
				if previous != nil && len(requested.Columns) != 0 {
					selected := make([]model.ColumnID, len(index.Columns))
					for i, column := range index.Columns {
						selected[i] = column.ID
					}
					mergeSelectedStatistics(index, previous, selected, nil)
				}
				change, err := options.Store.Publish(ctx, current, index)
				if err != nil {
					return err
				}
				mutations = append(mutations, change.Mutations...)
			}
			if bound.Verbose() {
				reports = append(reports, report{table: published.Descriptor.Name().Name, liveRows: next.LiveRows.Value, sampleRows: next.SampleRows, epoch: next.Epoch})
			}
		}
		if len(mutations) != 0 {
			if err := transactions.transaction.ApplyMutations(ctx, mutations); err != nil {
				return err
			}
		}
		for _, report := range reports {
			warnings = append(warnings, model.Error{Code: "00000", Message: fmt.Sprintf("analyzed %s: %d live rows, %d sampled, epoch %d", report.table, report.liveRows, report.sampleRows, report.epoch)})
		}
		return nil
	}()
	return warnings, errors.Join(err, snapshot.Close(), view.Close())
}

func analyzeDescriptorTarget(descriptor *model.Descriptor) (storage.ObjectRef, error) {
	if descriptor == nil || descriptor.State() != model.StatePublic {
		return storage.ObjectRef{}, model.ErrInvalidDescriptor
	}
	physical := descriptor.Physical()
	target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
	return target, target.Validate()
}

func indexHasSelectedExpression(descriptor *model.Descriptor, selected []model.ColumnID) bool {
	metadata, ok := descriptor.Index()
	if !ok {
		return false
	}
	for _, key := range metadata.Keys {
		if key.Expression != nil && statisticsExpressionSelected(key.Expression, metadata.Table, selected) {
			return true
		}
	}
	return false
}

func statisticsExpressionSelected(expression *model.ExpressionDefinition, table model.ObjectID, selected []model.ColumnID) bool {
	if expression == nil {
		return false
	}
	if len(selected) == 0 {
		return true
	}
	for _, dependency := range expression.Dependencies {
		if dependency.Object == table && dependency.Column != 0 && !slices.Contains(selected, dependency.Column) {
			return false
		}
	}
	return true
}

func executableIndexStatistics(ctx context.Context, requested bind.AnalyzeTarget, tableLayout []storage.ColumnLayout, snapshot bind.CatalogSnapshot, compiler *IndexExpressionCompiler, current *cpstats.Snapshot, limits model.DescriptorLimits) ([]cpstats.IndexDefinition, []*cpstats.Table, error) {
	definitions := make([]cpstats.IndexDefinition, 0, len(requested.Indexes))
	previous := make([]*cpstats.Table, 0, len(requested.Indexes))
	for _, expected := range requested.Indexes {
		published, err := snapshot.Descriptor(ctx, expected.ID())
		if err != nil {
			return nil, nil, err
		}
		descriptor := published.Descriptor
		if descriptor.Version() != expected.Version() || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectIndex {
			return nil, nil, catalog.ErrDescriptorChanged
		}
		spec, err := descriptor.SpecWithin(ctx, limits)
		if err != nil {
			return nil, nil, err
		}
		if spec.Index.Table != requested.Table.ID() {
			return nil, nil, model.ErrInvalidDescriptor
		}
		definition := cpstats.IndexDefinition{}
		for ordinal, key := range spec.Index.Keys {
			if key.Expression == nil || !statisticsExpressionSelected(key.Expression, spec.Index.Table, requested.Columns) {
				continue
			}
			layout := model.TypeLayout{Type: key.Type, Children: slices.Clone(key.Children)}
			expression, err := compiler.CompileIndexExpression(ctx, key.Expression, tableLayout, layout)
			if err != nil {
				return nil, nil, err
			}
			definition.Columns = append(definition.Columns, cpstats.IndexColumn{
				ID:         model.ColumnID(ordinal + 1),
				Expression: expression,
				Layout:     layout,
				Target:     int(key.StatisticsTarget),
				TargetSet:  key.StatisticsTargetSet,
			})
		}
		if len(definition.Columns) == 0 {
			continue
		}
		if spec.Index.Predicate != nil {
			definition.Predicate, err = compiler.CompileIndexExpression(ctx, spec.Index.Predicate, tableLayout, model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
			if err != nil {
				return nil, nil, err
			}
		}
		definition.Target, err = analyzeDescriptorTarget(descriptor)
		if err != nil {
			return nil, nil, err
		}
		old, found, err := current.Lookup(ctx, definition.Target)
		if err != nil {
			return nil, nil, err
		}
		definition.Epoch = 1
		if found {
			if old.Epoch == math.MaxUint64 {
				return nil, nil, cpstats.ErrLimit
			}
			definition.Epoch = old.Epoch + 1
			previous = append(previous, old)
		} else {
			previous = append(previous, nil)
		}
		definitions = append(definitions, definition)
	}
	if !slices.IsSortedFunc(definitions, func(left, right cpstats.IndexDefinition) int {
		return cmp.Compare(left.Target.ObjectID, right.Target.ObjectID)
	}) {
		return nil, nil, model.ErrInvalidDescriptor
	}
	return definitions, previous, ctx.Err()
}
func analyzeColumnTargets(table *model.Descriptor, selected []model.ColumnID) []cpstats.ColumnTarget {
	metadata, ok := table.Table()
	if !ok {
		return nil
	}
	targets := make([]cpstats.ColumnTarget, 0, len(metadata.Columns))
	for _, column := range metadata.Columns {
		if !column.StatisticsTargetSet || column.State != model.StatePublic || column.SystemIdentity {
			continue
		}
		if len(selected) != 0 && !slices.Contains(selected, column.ID) {
			continue
		}
		targets = append(targets, cpstats.ColumnTarget{ID: column.ID, Target: int(column.StatisticsTarget)})
	}
	slices.SortFunc(targets, func(left, right cpstats.ColumnTarget) int {
		return cmp.Compare(left.ID, right.ID)
	})
	return targets
}

func analyzeTableLayout(ctx context.Context, descriptor *model.Descriptor, limits model.DescriptorLimits) (storage.ObjectRef, []storage.KeyColumn, []storage.ColumnLayout, error) {
	if descriptor == nil || descriptor.Kind() != model.ObjectTable || descriptor.State() != model.StatePublic {
		return storage.ObjectRef{}, nil, nil, model.ErrInvalidDescriptor
	}
	spec, err := descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return storage.ObjectRef{}, nil, nil, err
	}
	physicalIdentity := descriptor.Physical()
	physical := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physicalIdentity.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physicalIdentity.EncodingVersion}
	layout := make([]storage.ColumnLayout, 0, len(spec.Table.Columns))
	for _, column := range spec.Table.Columns {
		if column.State == model.StatePublic {
			stored := storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: slices.Clone(column.Layout.Children), Nullable: column.Nullable}
			if column.ConstantDefault != nil {
				stored.HasMissingValue, stored.MissingValue = true, *column.ConstantDefault
			}
			layout = append(layout, stored)
		}
	}
	keyLayout := make([]storage.KeyColumn, len(spec.Table.PrimaryKey))
	for i, key := range spec.Table.PrimaryKey {
		keyLayout[i] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: slices.Clone(key.Children), Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	return physical, keyLayout, layout, physical.Validate()
}

func executableExtendedStatistics(ctx context.Context, descriptor *model.Descriptor, layout []storage.ColumnLayout, selected []model.ColumnID, snapshot bind.CatalogSnapshot, compiler *IndexExpressionCompiler, limits model.DescriptorLimits) ([]cpstats.ExtendedDefinition, error) {
	spec, err := descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return nil, err
	}
	selectedSet := make(map[model.ColumnID]bool, len(selected))
	for _, id := range selected {
		selectedSet[id] = true
	}
	result := make([]cpstats.ExtendedDefinition, 0, len(spec.Table.ExtendedStatistics))
	for _, statistic := range spec.Table.ExtendedStatistics {
		eligible := len(selected) == 0
		if !eligible {
			eligible = true
			for _, key := range statistic.Keys {
				eligible = eligible && selectedSet[key]
			}
			for _, expression := range statistic.Expressions {
				for _, dependency := range expression.Dependencies {
					if dependency.Object == descriptor.ID() && dependency.Column != 0 {
						eligible = eligible && selectedSet[dependency.Column]
					}
				}
			}
		}
		if !eligible {
			continue
		}
		definition := cpstats.ExtendedDefinition{OID: statistic.OID, Keys: slices.Clone(statistic.Keys), Kinds: slices.Clone(statistic.Kinds), Target: int(statistic.Target), TargetSet: statistic.TargetSet}
		for _, expression := range statistic.Expressions {
			executable, resultLayout, err := compiler.CompileStatisticsExpression(ctx, expression, layout)
			if err != nil {
				return nil, err
			}
			definition.Expressions = append(definition.Expressions, cpstats.ExtendedExpression{
				Expression:       executable,
				Layout:           resultLayout,
				EqualityOperator: statisticsComparisonOperator("=", resultLayout.Type.ID),
				OrderingOperator: statisticsComparisonOperator("<", resultLayout.Type.ID),
			})
		}
		result = append(result, definition)
	}
	return result, ctx.Err()
}

func statisticsComparisonOperator(name string, typeID model.TypeID) model.OperatorID {
	for operator := range builtin.OperatorsNamed(name) {
		if operator.Left == typeID && operator.Right == typeID {
			return operator.ID
		}
	}
	return 0
}

func mergeSelectedStatistics(next, old *cpstats.Table, selected []model.ColumnID, analyzed []cpstats.ExtendedDefinition) {
	selectedSet := make(map[model.ColumnID]bool, len(selected))
	for _, id := range selected {
		selectedSet[id] = true
	}
	columns := make([]cpstats.Column, 0, len(old.Columns)+len(next.Columns))
	for _, column := range old.Columns {
		if !selectedSet[column.ID] {
			columns = append(columns, column)
		}
	}
	for _, column := range next.Columns {
		if selectedSet[column.ID] {
			columns = append(columns, column)
		}
	}
	slices.SortFunc(columns, func(a, b cpstats.Column) int { return cmp.Compare(a.ID, b.ID) })
	next.Columns = columns
	analyzedSet := make(map[model.OID]bool, len(analyzed))
	for _, definition := range analyzed {
		analyzedSet[definition.OID] = true
	}
	extended := make([]cpstats.ExtendedStatistic, 0, len(old.Extended)+len(next.Extended))
	for _, statistic := range old.Extended {
		if !analyzedSet[statistic.OID] {
			extended = append(extended, statistic)
		}
	}
	extended = append(extended, next.Extended...)
	slices.SortFunc(extended, func(a, b cpstats.ExtendedStatistic) int { return cmp.Compare(a.OID, b.OID) })
	next.Extended = extended
}

func validatePreparedStatistics(ctx context.Context, prepared *Prepared, options *StatisticsCommandOptions, view txn.ReadView) error {
	if prepared == nil || len(prepared.statistics) == 0 {
		return nil
	}
	if options == nil || options.Store == nil || view == nil {
		return errPreparedDependency
	}
	snapshot, err := options.Store.Snapshot(view, view.Timestamp())
	if err != nil {
		return err
	}
	for _, expected := range prepared.statistics {
		current, found, err := snapshot.Lookup(ctx, expected.Target)
		if err != nil {
			return err
		}
		if expected.Epoch == 0 {
			if found {
				return errPreparedDependency
			}
			continue
		}
		if !found || current.Epoch != expected.Epoch {
			return errPreparedDependency
		}
	}
	return ctx.Err()
}

type catalogExtendedStatistics struct {
	snapshot *catalog.CatalogSnapshot
	limits   model.DescriptorLimits
}

func (source *catalogExtendedStatistics) Definitions(ctx context.Context, target storage.ObjectRef) ([]opt.ExtendedStatisticDefinition, bool, error) {
	if source == nil || source.snapshot == nil {
		return nil, false, nil
	}
	published, err := source.snapshot.Descriptor(ctx, target.ObjectID)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	descriptor := published.Descriptor
	physical := descriptor.Physical()
	if descriptor.Kind() != model.ObjectTable || descriptor.State() != model.StatePublic || descriptor.Version() != target.DescriptorVersion || physical.Generation != target.Generation || physical.EncodingVersion != target.EncodingVersion {
		return nil, false, catalog.ErrDescriptorChanged
	}
	spec, err := descriptor.SpecWithin(ctx, source.limits)
	if err != nil {
		return nil, false, err
	}
	result := make([]opt.ExtendedStatisticDefinition, len(spec.Table.ExtendedStatistics))
	for i, statistic := range spec.Table.ExtendedStatistics {
		result[i] = opt.ExtendedStatisticDefinition{OID: statistic.OID, Keys: slices.Clone(statistic.Keys), ExpressionCount: len(statistic.Expressions), Kinds: slices.Clone(statistic.Kinds)}
	}
	return result, true, ctx.Err()
}
