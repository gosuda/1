package session

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) executeNamedPrepared(ctx context.Context, command bind.ExecuteCommand, options PreparedExecution, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	defer func() { result.Warnings = transactions.settings.visibleWarnings(result.Warnings) }()
	statement := command.Statement
	entry := (*namedPreparedStatement)(nil)
	if statement != nil {
		entry = transactions.named.prepared[statement.Name]
	}
	if entry == nil {
		return result, transactions.fail(errors.Join(&model.Error{Code: "26000", Message: "prepared statement does not exist"}, rejectPreparedEnvironment(options.Environment, nil)))
	}
	if len(statement.Params) != len(entry.parameterLayouts) {
		return result, transactions.fail(errors.Join(&model.Error{Code: "42601", Message: "wrong number of parameters for prepared statement"}, rejectPreparedEnvironment(options.Environment, nil)))
	}
	if options.statementPlanner == nil || options.analyzedPlanner == nil || options.argumentPlanner == nil || options.Environment.Query == nil || consume == nil && options.explain == nil || options.Copy != nil {
		return result, transactions.fail(errors.Join(errPreparedInvocation, rejectPreparedEnvironment(options.Environment, nil)))
	}
	options.Environment.Builtin = transactions.settings.BuiltinOptions(options.Environment.Builtin.Collation, options.Environment.Builtin.DefaultCollation)
	resources := transactions.settings.Resources()
	if err := options.Environment.Query.Constrain(resources.EffectiveWorkMemoryBytes, resources.EffectiveTempFileBytes); err != nil {
		return result, transactions.fail(errors.Join(err, rejectPreparedEnvironment(options.Environment, nil)))
	}
	ctx = txn.WithLockTimeout(ctx, resources.LockTimeout)
	options.Environment.Settings = transactions.settingRuntime(ctx)
	options.Environment.Catalog, err = transactions.attachCatalogSettings(ctx, options.Environment.Catalog)
	if err != nil {
		return result, transactions.fail(errors.Join(err, rejectPreparedEnvironment(options.Environment, nil)))
	}
	automatic := !transactions.active
	if automatic {
		if err = transactions.begin(ctx, TransactionStart{}); err != nil {
			return result, errors.Join(err, rejectPreparedEnvironment(options.Environment, nil))
		}
	}
	options.Environment.Temporal = transactions.temporal
	if err = transactions.ordinary(); err != nil {
		err = rejectPreparedEnvironment(options.Environment, err)
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(ctx)))
		}
		return result, err
	}
	if transactions.settings.TransactionPolicy().ReadOnly && options.Environment.Sequences != nil {
		options.Environment.Sequences = readOnlySequences{SequenceRuntime: options.Environment.Sequences}
	}
	completed := false
	beforeSettings := *transactions.settings
	deferredMark := len(transactions.deferredTriggers)
	defer func() {
		if !completed {
			transactions.markFailed(errSessionStatementPanic)
			transactions.closeError = errors.Join(transactions.closeError, rejectPreparedEnvironment(options.Environment, nil))
			if automatic {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(ctx)))
			}
		}
	}()
	var target *Prepared
	targetInvoked := false
	plainExplain := options.explain != nil && !options.explain.command.Options.Analyze
	err = transactions.routineAtomicStatement(ctx, func(statementContext context.Context) (runErr error) {
		dependencies, dependencyStale, runErr := transactions.namedPreparedPlanningDependencies(entry, options)
		if runErr != nil {
			return runErr
		}
		view, runErr := transactions.transaction.ReadView(statementContext, dependencies)
		if runErr != nil {
			return runErr
		}
		stale := dependencyStale
		if !stale {
			stale, runErr = transactions.namedPreparedStale(statementContext, entry, view, options)
		}
		if runErr == nil && stale {
			entry, runErr = transactions.reanalyzeNamedPrepared(statementContext, entry, view, options, dependencies)
			if runErr == nil {
				var stillStale bool
				dependencies, stillStale, runErr = transactions.namedPreparedPlanningDependencies(entry, options)
				if runErr == nil && stillStale {
					runErr = errPreparedDependency
				}
			}
		}
		if runErr == nil && options.Dependencies == nil && options.planningOptions != nil && entry.analysis != nil {
			// The caller declared no read set, so widen the declaration to the
			// bound statement's negotiated access paths before the argument and
			// generic/custom planning views open.
			var scans []txn.ReadDependency
			scans, runErr = transactions.statementScanDependencies(statementContext, view, entry.analysis.bound, options, options.planningOptions, options.Environment.Builtin)
			if runErr == nil {
				dependencies = append(dependencies, scans...)
			}
		}
		runErr = errors.Join(runErr, view.Close())
		if runErr != nil {
			return runErr
		}
		var arguments *Prepared
		if len(statement.Params) != 0 {
			argumentView, viewErr := transactions.transaction.ReadView(statementContext, dependencies)
			if viewErr != nil {
				return viewErr
			}
			arguments, runErr = options.argumentPlanner(statementContext, argumentView, options.Environment.Builtin, statement.Params, entry.parameterLayouts)
			runErr = errors.Join(runErr, argumentView.Close())
			if runErr != nil {
				return runErr
			}
		}
		var parameters []expr.ParameterValue
		if !plainExplain {
			parameters, runErr = transactions.evaluatePreparedArguments(statementContext, arguments, entry, dependencies, options)
			if runErr != nil {
				return runErr
			}
		}
		planningView, runErr := transactions.transaction.ReadView(statementContext, dependencies)
		if runErr != nil {
			return runErr
		}
		if plainExplain {
			target, runErr = transactions.selectNamedGeneric(statementContext, entry, planningView, options.Environment.Builtin, options)
		} else {
			target, runErr = transactions.selectNamedPrepared(statementContext, entry, planningView, options.Environment.Builtin, parameters, options)
		}
		runErr = errors.Join(runErr, planningView.Close())
		if runErr != nil {
			return runErr
		}
		if target == nil || target.bound == nil || target.bound.ProcedureCall() {
			return errPreparedInvocation
		}
		options.Environment.Parameters = parameters
		options.Dependencies = nil
		targetInvoked = true
		result.RowsAffected, runErr = runPreparedStatement(statementContext, transactions, transactions.transaction, statementExecution{prepared: target}, options, consume, target.bound.ProcedureCall())
		return runErr
	})
	if !targetInvoked {
		err = errors.Join(err, rejectPreparedEnvironment(options.Environment, nil))
	}
	err = errors.Join(err, transactions.markDataAccess())
	if err != nil && deferredMark <= len(transactions.deferredTriggers) {
		clear(transactions.deferredTriggers[deferredMark:])
		transactions.deferredTriggers = transactions.deferredTriggers[:deferredMark]
	}
	completed = true
	result.Tag = "EXECUTE"
	result.Warnings = append(result.Warnings, targetWarnings(target)...)
	if err != nil {
		transactions.settings.current = beforeSettings.current
		transactions.settings.retained = beforeSettings.retained
		result.RowsAffected = 0
		transactions.markFailed(err)
		if automatic {
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

func (transactions *Transactions) namedPreparedPlanningDependencies(entry *namedPreparedStatement, options PreparedExecution) ([]txn.ReadDependency, bool, error) {
	if entry == nil || entry.analysis == nil || entry.analysis.bound == nil {
		return nil, false, errPreparedInvocation
	}
	count := entry.analysis.bound.DependencyCount()
	if count != 0 && options.Catalog == nil {
		return nil, false, errPreparedInvocation
	}
	dependencies := make([]txn.ReadDependency, 0, len(entry.dependencies)+len(options.Dependencies)+count+len(entry.analysis.statistics))
	dependencies = append(dependencies, entry.dependencies...)
	dependencies = append(dependencies, options.Dependencies...)
	for index := range count {
		dependency, _ := entry.analysis.bound.Dependency(index)
		dependencies = append(dependencies, options.Catalog.DescriptorDependency(dependency.Object))
	}
	if entry.generic != nil && entry.generic.graph != nil {
		plan := entry.generic.graph.Plan()
		for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
			node, _ := plan.Node(id)
			if node.Metadata().Algorithm != physical.SecondaryScan {
				continue
			}
			if options.Catalog == nil {
				return nil, false, errPreparedInvocation
			}
			_, target := node.ReadTargets()
			dependencies = append(dependencies, options.Catalog.DescriptorDependency(target.ObjectID))
		}
	}
	statistics, err := preparedStatisticsDependencies(entry.analysis, options.Statistics)
	if err != nil {
		if options.Statistics != nil && options.Statistics.Store != nil && cachedPreparedInvalidation(err) {
			return dependencies, true, nil
		}
		return nil, false, err
	}
	return append(dependencies, statistics...), false, nil
}

func (transactions *Transactions) namedPreparedStale(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, options PreparedExecution) (bool, error) {
	if len(entry.analysis.statistics) != 0 && (options.Statistics == nil || options.Statistics.Store == nil) {
		return false, errPreparedDependency
	}
	if namedPreparedSettingsChanged(entry, transactions.settings.SearchPath(), options.Environment.Builtin) {
		return true, nil
	}
	if err := entry.analysis.validatePublication(transactions.transaction.PublicationOwner()); err != nil {
		if cachedPreparedInvalidation(err) {
			return true, nil
		}
		return false, err
	}
	if entry.generic != nil {
		if err := validatePreparedDependencies(ctx, entry.generic, options.Catalog, view, transactions.named.temporaryDescriptors()); err != nil {
			if cachedPreparedInvalidation(err) {
				return true, nil
			}
			return false, err
		}
	}
	if err := transactions.validateNamedAnalysisDependencies(ctx, entry.analysis, options.Catalog, view); err != nil {
		if cachedPreparedInvalidation(err) {
			return true, nil
		}
		return false, err
	}
	if err := validatePreparedStatistics(ctx, entry.analysis, options.Statistics, view); err != nil {
		if cachedPreparedInvalidation(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (transactions *Transactions) validateNamedAnalysisDependencies(ctx context.Context, analyzed *Prepared, store *catalog.Store, view txn.ReadView) (err error) {
	if analyzed == nil || analyzed.bound == nil {
		return errPreparedInvocation
	}
	if analyzed.bound.DependencyCount() == 0 {
		return nil
	}
	if store == nil {
		return errPreparedInvocation
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return err
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return errors.Join(err, snapshot.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	for index := range analyzed.bound.DependencyCount() {
		dependency, _ := analyzed.bound.Dependency(index)
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		if published.Descriptor.Version() != dependency.Version {
			return errPreparedDependency
		}
	}
	return nil
}

func namedPreparedSettingsChanged(entry *namedPreparedStatement, path []string, current builtin.ResolveOptions) bool {
	if entry == nil || !slices.Equal(entry.searchPath, path) {
		return true
	}
	current.Types = nil
	return !entry.semantics.SameSemantics(current)
}

func cachedPreparedInvalidation(err error) bool {
	return errors.Is(err, errPreparedDependency) || errors.Is(err, errPreparedPublication) ||
		errors.Is(err, catalog.ErrUnknownObject) || errors.Is(err, catalog.ErrDescriptorChanged)
}

func (transactions *Transactions) reanalyzeNamedPrepared(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, options PreparedExecution, dependencies []txn.ReadDependency) (*namedPreparedStatement, error) {
	parsed, err := zpgparse.Parse(entry.statement)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0] == nil || parsed.Stmts[0].Stmt == nil {
		return nil, errors.Join(err, errPreparedInvocation)
	}
	var compiled *Prepared
	if options.Dependencies == nil && options.planningOptions != nil {
		// No caller-declared read set: the stale-check view covers binding's
		// catalog reads, the bound statement's access paths widen the
		// declaration, and the reopened view covers negotiated planning scans.
		bound, bindErr := transactions.bindNamedStatement(ctx, view, options, options.Environment.Builtin, parsed.Stmts[0], entry.parameterLayouts)
		if bindErr != nil {
			return nil, bindErr
		}
		scans, scanErr := transactions.statementScanDependencies(ctx, view, bound, options, options.planningOptions, options.Environment.Builtin)
		if scanErr != nil {
			return nil, scanErr
		}
		if options.analyzedPlanner == nil {
			return nil, errPreparedInvocation
		}
		planningView, viewErr := transactions.transaction.ReadView(ctx, append(slices.Clone(dependencies), scans...))
		if viewErr != nil {
			return nil, viewErr
		}
		compiled, err = options.analyzedPlanner(ctx, planningView, options.Environment.Builtin, &Prepared{bound: bound}, nil)
		err = errors.Join(err, planningView.Close())
	} else {
		compiled, err = options.statementPlanner(ctx, view, options.Environment.Builtin, parsed.Stmts[0], entry.parameterLayouts)
	}
	if err != nil {
		return nil, err
	}
	if !sameResultShape(entry.resultColumns, compiled.Columns()) {
		return nil, &model.Error{Code: "0A000", Message: "cached plan must not change result type"}
	}
	binding := make([]txn.ReadDependency, 0, len(entry.dependencies)+len(options.Dependencies))
	binding = append(binding, entry.dependencies...)
	binding = append(binding, options.Dependencies...)
	replacement, err := transactions.copyPreparedMetadata(ctx, entry.name, entry.statement, transactions.settings.SearchPath(), entry.preparedAt, compiled, binding, entry.metadataBytes)
	if err != nil {
		return nil, err
	}
	replacement.genericPlans = entry.genericPlans
	replacement.customPlans = entry.customPlans
	if entry.metadataBytes > transactions.named.metadataBytes {
		return nil, errPreparedInvocation
	}
	retained := transactions.named.metadataBytes - entry.metadataBytes
	if replacement.metadataBytes > transactions.named.options.MetadataBytes-retained {
		return nil, &model.Error{Code: "54000", Message: "session named object metadata limit exceeded"}
	}
	transactions.named.metadataBytes = retained + replacement.metadataBytes
	transactions.named.prepared[replacement.name] = replacement
	return replacement, nil
}

func (transactions *Transactions) selectNamedPrepared(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, builtins builtin.ResolveOptions, parameters []expr.ParameterValue, options PreparedExecution) (*Prepared, error) {
	if entry == nil || entry.analysis == nil {
		return nil, errPreparedInvocation
	}
	if len(entry.parameterLayouts) != 0 && entry.policyCustomPlans < 5 {
		return transactions.planNamedCustom(ctx, entry, view, builtins, parameters, options)
	}
	generic, err := transactions.ensureNamedGeneric(ctx, entry, view, builtins, options)
	if err != nil {
		return nil, err
	}
	if len(entry.parameterLayouts) == 0 || entry.policy.Mode() == opt.GenericPlan {
		if entry.genericPlans != ^uint64(0) {
			entry.genericPlans++
		}
		return generic, nil
	}
	return transactions.planNamedCustom(ctx, entry, view, builtins, parameters, options)
}

func (transactions *Transactions) selectNamedGeneric(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, builtins builtin.ResolveOptions, options PreparedExecution) (*Prepared, error) {
	generic, err := transactions.ensureNamedGeneric(ctx, entry, view, builtins, options)
	if err != nil {
		return nil, err
	}
	if entry.genericPlans != ^uint64(0) {
		entry.genericPlans++
	}
	return generic, nil
}

func (transactions *Transactions) ensureNamedGeneric(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, builtins builtin.ResolveOptions, options PreparedExecution) (*Prepared, error) {
	if entry == nil || entry.analysis == nil {
		return nil, errPreparedInvocation
	}
	if entry.generic != nil {
		return entry.generic, nil
	}
	generic, err := options.analyzedPlanner(ctx, view, builtins, entry.analysis, nil)
	if err != nil {
		return nil, err
	}
	if generic == nil || generic.bound == nil || generic.graph == nil {
		return nil, errPreparedInvocation
	}
	bytes := preparedCachedPlanBytes(generic)
	if err := transactions.named.reserve(bytes); err != nil {
		return nil, err
	}
	if err := entry.policy.ObserveGeneric(generic.estimatedCost); err != nil {
		transactions.named.release(bytes)
		return nil, err
	}
	entry.generic, entry.genericBytes = generic, bytes
	entry.metadataBytes += bytes
	return generic, nil
}

func (transactions *Transactions) planNamedCustom(ctx context.Context, entry *namedPreparedStatement, view txn.ReadView, builtins builtin.ResolveOptions, parameters []expr.ParameterValue, options PreparedExecution) (*Prepared, error) {
	custom, err := options.analyzedPlanner(ctx, view, builtins, entry.analysis, parameters)
	if err != nil {
		return nil, err
	}
	if custom == nil || custom.bound == nil || custom.graph == nil {
		return nil, errPreparedInvocation
	}
	if err := entry.policy.ObserveCustom(custom.estimatedCost, custom.planningDuration, transactions.named.options.PlanningCostUnitsPerSecond); err != nil {
		return nil, err
	}
	if entry.policyCustomPlans < 5 {
		entry.policyCustomPlans++
	}
	if entry.customPlans != ^uint64(0) {
		entry.customPlans++
	}
	return custom, nil
}

func targetWarnings(prepared *Prepared) []model.Error {
	if prepared == nil {
		return nil
	}
	return prepared.Warnings()
}

func (transactions *Transactions) evaluatePreparedArguments(ctx context.Context, prepared *Prepared, entry *namedPreparedStatement, dependencies []txn.ReadDependency, options PreparedExecution) (parameters []expr.ParameterValue, err error) {
	if len(entry.parameterLayouts) == 0 {
		return nil, nil
	}
	query, err := transactions.named.options.NewQuery(context.WithoutCancel(transactions.life))
	if err != nil {
		return nil, err
	}
	argumentOptions := options
	argumentOptions.Dependencies = dependencies
	if prepared != nil {
		// Argument expressions plan their own reads; append the compiled
		// argument plan's exact read set so its execution view stays declared.
		planDependencies, _, err := prepared.ReadDependencies(ctx, options.Catalog, options.Environment.Limits.MetadataBytes)
		if err != nil {
			return nil, err
		}
		argumentOptions.Dependencies = append(slices.Clone(dependencies), planDependencies...)
	}
	argumentOptions.Environment.Query = query
	// This query is the session's own, not the caller's borrowed one.
	argumentOptions.Environment.BorrowQuery = false
	argumentOptions.Environment.Parameters = nil
	argumentOptions.explain = nil
	argumentOptions.Environment.Instrumentation = nil
	values := make([]model.Value, len(entry.parameterLayouts))
	seen := false
	_, err = runPreparedStatement(ctx, transactions, transactions.transaction, statementExecution{prepared: prepared}, argumentOptions, func(fetch context.Context, _ *Prepared, rows *Rows) error {
		batch := columnar.Batch{MaxRows: 2}
		if nextErr := rows.Next(fetch, &batch); nextErr != nil {
			if nextErr == io.EOF {
				return &model.Error{Code: "21000", Message: "EXECUTE parameter expressions produced no row"}
			}
			return nextErr
		}
		if batch.Rows() != 1 || len(batch.Columns) != len(values) {
			return &model.Error{Code: "21000", Message: "EXECUTE parameter expressions produced more than one row"}
		}
		remaining := argumentOptions.Environment.Limits.ParameterBytes
		for i := range batch.Columns {
			value, used, copyErr := columnar.CopyValue(fetch, &batch.Columns[i], batch.Lane(0), remaining)
			if copyErr != nil {
				return copyErr
			}
			values[i] = value
			remaining -= used
		}
		seen = true
		if nextErr := rows.Next(fetch, &batch); nextErr != io.EOF {
			if nextErr == nil {
				return &model.Error{Code: "21000", Message: "EXECUTE parameter expressions produced more than one row"}
			}
			return nextErr
		}
		return nil
	}, false)
	if err != nil {
		return nil, err
	}
	if !seen {
		return nil, errPreparedInvocation
	}
	parameters = make([]expr.ParameterValue, len(values))
	for i := range values {
		parameters[i] = expr.ParameterValue{Number: uint32(i + 1), Layout: entry.parameterLayouts[i], Value: values[i]}
	}
	return parameters, nil
}
