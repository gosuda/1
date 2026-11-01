package session

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// ReadDependencies returns owned descriptor and physical scan dependencies for
// invoking an already bound plan. Residual filters do not narrow protected spans.
// This does not discover catalog namespace/absence reads needed during binding.
func (prepared *Prepared) ReadDependencies(ctx context.Context, store *catalog.Store, maximumBytes uint64) ([]txn.ReadDependency, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if prepared == nil || prepared.bound == nil || prepared.graph == nil {
		return nil, 0, errPreparedInvocation
	}
	descriptors := prepared.bound.DependencyCount()
	if descriptors != 0 && store == nil {
		return nil, 0, errPreparedInvocation
	}
	plan := prepared.graph.Plan()
	count := uint64(descriptors)
	for index := uint32(1); index <= plan.NodeCount(); index++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		node, _ := plan.Node(physical.NodeID(index))
		if !node.HasReadSpan() {
			continue
		}
		count++
		if node.Metadata().Algorithm == physical.SecondaryScan {
			if store == nil {
				return nil, 0, errPreparedInvocation
			}
			count += 2
		}
	}
	const header = uint64(unsafe.Sizeof(txn.ReadDependency{}))
	if count > maximumBytes/header || count > uint64(math.MaxInt) {
		return nil, 0, dependencyBudgetError()
	}
	used := count * header
	dependencies := make([]txn.ReadDependency, 0, int(count))
	const descriptorBytes = uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(storage.KeySpan{})) + 2*uint64(unsafe.Sizeof(model.Value{})) + 16
	for index := range descriptors {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if descriptorBytes > maximumBytes-used {
			return nil, 0, dependencyBudgetError()
		}
		dependency, _ := prepared.bound.Dependency(index)
		dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
		used += descriptorBytes
	}
	for index := uint32(1); index <= plan.NodeCount(); index++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		node, _ := plan.Node(physical.NodeID(index))
		if !node.HasReadSpan() {
			continue
		}
		read, bytes, err := node.CloneReadSpan(ctx, maximumBytes-used)
		if err != nil {
			return nil, 0, err
		}
		used += bytes
		dependencies = append(dependencies, txn.ReadDependency{Target: read.Target, Occurrence: read.Occurrence, KeyLayout: read.KeyLayout, Spans: read.Spans})
		if node.Metadata().Algorithm == physical.SecondaryScan {
			if descriptorBytes > maximumBytes-used {
				return nil, 0, dependencyBudgetError()
			}
			dependencies = append(dependencies, store.DescriptorDependency(read.Target.ObjectID))
			used += descriptorBytes
			if node.HasLookupReadSpan() {
				lookup, bytes, err := node.CloneLookupReadSpan(ctx, maximumBytes-used)
				if err != nil {
					return nil, 0, err
				}
				used += bytes
				dependencies = append(dependencies, txn.ReadDependency{Target: lookup.Target, Occurrence: lookup.Occurrence, KeyLayout: lookup.KeyLayout, Spans: lookup.Spans})
			} else {
				dependencies = append(dependencies, txn.ReadDependency{Target: read.Source, Occurrence: read.Occurrence})
			}
		}
	}
	return dependencies, used, nil
}
func dependencyBudgetError() error {
	return &model.Error{Code: "54000", Message: "prepared read dependency byte budget exceeded"}
}

// statementScanDependencies derives the data read set for a bound statement
// whose caller supplied no dependencies. Each scan relation is enumerated
// through the statement's own access planner while the binding view is still
// open, so declared targets and key layouts are exactly the requests
// optimization may negotiate: every candidate path target, index descriptor
// reads for secondary scans, lookup bases, and the bare table markers
// covering-only secondary scans retain. Spans stay unbounded; planning narrows
// them inside the declared objects.
func (transactions *Transactions) statementScanDependencies(ctx context.Context, view txn.ReadView, bound *bind.BoundStatement, options PreparedExecution, planning func(context.Context, txn.ReadView, builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error), builtins builtin.ResolveOptions) (dependencies []txn.ReadDependency, err error) {
	store := options.Catalog
	if bound == nil || store == nil {
		return nil, errPreparedInvocation
	}
	graph := bound.Graph()
	scans := false
	catalogScans := false
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		relation, ok := graph.Rel(id)
		if !ok {
			continue
		}
		if relation.Kind == ir.Scan && relation.Occurrence != 0 {
			scans = true
		}
		if relation.Kind == ir.CatalogScan {
			catalogScans = true
		}
	}
	if !scans && !catalogScans {
		return nil, ctx.Err()
	}
	maximum := options.Environment.Limits.MetadataBytes
	const dependencyBytes = 512
	full := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	add := func(dependency txn.ReadDependency) error {
		for _, existing := range dependencies {
			if existing.Target == dependency.Target && existing.Occurrence == dependency.Occurrence && len(existing.Spans) == len(dependency.Spans) && storage.KeyLayoutsEqual(existing.KeyLayout, dependency.KeyLayout) {
				if len(existing.Spans) == 0 || (len(existing.Spans) == 1 && existing.Spans[0].Lower.Unbounded && existing.Spans[0].Upper.Unbounded && dependency.Spans[0].Lower.Unbounded && dependency.Spans[0].Upper.Unbounded) {
					return nil
				}
			}
		}
		if uint64(len(dependencies)) >= maximum/dependencyBytes {
			return dependencyBudgetError()
		}
		dependencies = append(dependencies, dependency)
		return nil
	}
	if catalogScans {
		// Catalog emitters read every catalog collection plus the statistics
		// and sequence-state stores through the statement view; none is a
		// negotiated access path, so declare them all up front.
		for _, dependency := range catalogScanDependencies(options.Catalog, options.Environment.Catalog) {
			if err := add(dependency); err != nil {
				return nil, err
			}
		}
	}
	if !scans {
		return dependencies, ctx.Err()
	}
	if planning == nil {
		return nil, errPreparedInvocation
	}
	snapshot, plan, err := planning(ctx, view, builtins)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	planner := plan.Optimize.Access
	if planner == nil {
		// Optimization reports the missing planner itself; returning only the
		// binding catalog set preserves that diagnostic.
		return dependencies, nil
	}
	if len(boundRowLockPolicies(bound)) == 0 && len(bound.Parameters()) == 0 && plan.Optimize.Statistics == nil {
		trial := plan
		trial.ValidateBound = nil
		if prepared, prepErr := prepareBound(ctx, bound, trial); prepErr == nil && prepared != nil && prepared.graph != nil {
			if planDeps, _, depErr := prepared.ReadDependencies(ctx, store, maximum); depErr == nil {
				for _, dependency := range planDeps {
					if err := add(dependency); err != nil {
						return nil, err
					}
				}
				for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
					relation, ok := graph.Rel(id)
					if !ok || relation.Kind != ir.Scan || relation.Occurrence == 0 {
						continue
					}
					columns := make([]model.ColumnID, 0, relation.OutputCount())
					for i := uint32(0); i < relation.OutputCount(); i++ {
						if slot, ok := graph.Output(id, i); ok {
							if info, ok := graph.Slot(slot); ok && info.Column != 0 {
								columns = append(columns, info.Column)
							}
						}
					}
					access, accessErr := planner.Access(ctx, opt.AccessRequest{Object: relation.Object, Generation: relation.Generation, DescriptorVersion: relation.DescriptorVersion, Columns: columns, MaxPaths: plan.Optimize.MaxAccessPaths, MaxBytes: plan.Optimize.MaxBytes})
					if accessErr != nil {
						return nil, accessErr
					}
					for _, path := range access.Paths {
						if path.Index != 0 {
							if err := add(store.DescriptorDependency(path.Target.ObjectID)); err != nil {
								return nil, err
							}
						}
					}
				}
				return dependencies, ctx.Err()
			}
		}
	}
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relation, ok := graph.Rel(id)
		if !ok || relation.Kind != ir.Scan || relation.Occurrence == 0 {
			continue
		}
		columns := make([]model.ColumnID, 0, relation.OutputCount())
		for i := uint32(0); i < relation.OutputCount(); i++ {
			slot, ok := graph.Output(id, i)
			if !ok {
				continue
			}
			info, ok := graph.Slot(slot)
			if !ok || info.Column == 0 {
				continue
			}
			columns = append(columns, info.Column)
		}
		access, err := planner.Access(ctx, opt.AccessRequest{Object: relation.Object, Generation: relation.Generation, DescriptorVersion: relation.DescriptorVersion, Columns: columns, MaxPaths: plan.Optimize.MaxAccessPaths, MaxBytes: plan.Optimize.MaxBytes})
		if err != nil {
			return nil, err
		}
		for _, path := range access.Paths {
			if err := add(txn.ReadDependency{Target: path.Target, Occurrence: relation.Occurrence, KeyLayout: path.Keys, Spans: []storage.KeySpan{full}}); err != nil {
				return nil, err
			}
			if path.Index == 0 {
				continue
			}
			if err := add(store.DescriptorDependency(path.Target.ObjectID)); err != nil {
				return nil, err
			}
			if path.Lookup.Enabled {
				if err := add(txn.ReadDependency{Target: path.Lookup.Target, Occurrence: relation.Occurrence, KeyLayout: path.Lookup.Keys, Spans: []storage.KeySpan{full}}); err != nil {
					return nil, err
				}
			} else if err := add(txn.ReadDependency{Target: access.Target, Occurrence: relation.Occurrence}); err != nil {
				return nil, err
			}
		}
	}
	return dependencies, ctx.Err()
}

// namedStatementPlanView opens the read view covering one named statement's
// binding and planning. Caller-declared dependencies open directly. With nil
// dependencies the catalog discovery set covers the binding view, the bound
// statement's negotiated access paths then widen the declaration, and the
// reopened view covers every scan optimization may plan. bound and parameters
// are populated only on the derived path so callers do not bind twice.
func (transactions *Transactions) namedStatementPlanView(statement context.Context, options PreparedExecution, builtins builtin.ResolveOptions, parsed *zpgparse.RawStmt, argtypes []*zpgparse.Node) (view txn.ReadView, bound *bind.BoundStatement, parameters []model.TypeLayout, err error) {
	if options.Dependencies != nil {
		view, err = transactions.transaction.ReadView(statement, options.Dependencies)
		return view, nil, nil, err
	}
	if options.Catalog == nil || options.planningOptions == nil {
		return nil, nil, nil, errPreparedInvocation
	}
	discovery, err := catalogDiscoveryDependencies(statement, transactions.transaction, options.Catalog, catalogDiscovery{}, nil, options.Environment.Limits.MetadataBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	bindingView, err := transactions.transaction.ReadView(statement, discovery)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(argtypes) != 0 {
		if options.parameterTypeResolver == nil {
			err = errPreparedInvocation
		} else {
			parameters, err = options.parameterTypeResolver(statement, bindingView, builtins, argtypes)
		}
	}
	if err == nil {
		bound, err = transactions.bindNamedStatement(statement, bindingView, options, builtins, parsed, parameters)
	}
	var scans []txn.ReadDependency
	if err == nil {
		scans, err = transactions.statementScanDependencies(statement, bindingView, bound, options, options.planningOptions, builtins)
	}
	if err == nil {
		view, err = transactions.transaction.ReadView(statement, append(discovery, scans...))
	}
	err = errors.Join(err, bindingView.Close())
	if err != nil {
		return nil, nil, nil, err
	}
	return view, bound, parameters, nil
}

// bindNamedStatement binds parsed against the caller's view, which must
// already cover the catalog read set. parameters carries the resolved PREPARE
// argument layouts; it is nil for unparameterized statements.
func (transactions *Transactions) bindNamedStatement(statement context.Context, view txn.ReadView, options PreparedExecution, builtins builtin.ResolveOptions, parsed *zpgparse.RawStmt, parameters []model.TypeLayout) (bound *bind.BoundStatement, err error) {
	snapshot, planning, err := options.planningOptions(statement, view, builtins)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	planning.Bind.Builtin = planning.Normalize.Builtin
	return bind.Bind(statement, parsed, snapshot, planning.Bind, parameters)
}

func catalogReadDependencies(store *catalog.Store, dependencies []txn.ReadDependency) []txn.ReadDependency {
	if store == nil || len(dependencies) == 0 {
		return nil
	}
	layouts := store.Layouts()
	result := make([]txn.ReadDependency, 0, len(layouts))
	for _, dependency := range dependencies {
		for _, layout := range layouts {
			if dependency.Target == layout.Target {
				dependency.Occurrence = 0
				result = append(result, dependency)
				break
			}
		}
	}
	return result
}

// catalogLayoutDependencies declares an unbounded span over every catalog
// collection, covering every namespace and descriptor read binding performs.
// Catalog writes are rare, so the coarse declaration does not limit statement
// concurrency in practice.
func catalogLayoutDependencies(store *catalog.Store) []txn.ReadDependency {
	if store == nil {
		return nil
	}
	layouts := store.Layouts()
	dependencies := make([]txn.ReadDependency, 0, len(layouts))
	for _, layout := range layouts {
		dependencies = append(dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: store.KeyLayout(), Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
	}
	return dependencies
}

// catalogScanDependencies declares every collection a catalog relation scan
// may read: all catalog collections plus the statistics and sequence-state
// stores emitters reach through the statement view. Emitters are free to scan
// namespaces outside the binding discovery set, so coverage is unbounded.
func catalogScanDependencies(store *catalog.Store, scanner catalogrel.Scanner) []txn.ReadDependency {
	dependencies := catalogLayoutDependencies(store)
	provider, ok := scanner.(catalogrel.AuxiliaryScanStoresProvider)
	if !ok {
		return dependencies
	}
	full := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	for _, auxiliary := range provider.AuxiliaryScanStores() {
		dependencies = append(dependencies, txn.ReadDependency{Target: auxiliary.Target, KeyLayout: auxiliary.KeyLayout, Spans: full})
	}
	return dependencies
}

func boundHasCatalogScan(bound *bind.BoundStatement) bool {
	if bound == nil {
		return false
	}
	graph := bound.Graph()
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		if relation, ok := graph.Rel(id); ok && relation.Kind == ir.CatalogScan {
			return true
		}
	}
	return false
}

func bindScanOccurrences(bound *bind.BoundStatement, dependencies []txn.ReadDependency) []txn.ReadDependency {
	if bound == nil || len(dependencies) == 0 {
		return slicesCloneReadDependencies(dependencies)
	}
	type scanTarget struct {
		target     storage.ObjectRef
		occurrence model.ScanOccurrence
	}
	graph := bound.Graph()
	scans := make([]scanTarget, 0, graph.RelCount())
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		relation, ok := graph.Rel(id)
		if !ok || relation.Kind != ir.Scan || relation.Occurrence == 0 {
			continue
		}
		target := storage.ObjectRef{ObjectID: relation.Object, Generation: relation.Generation, DescriptorVersion: relation.DescriptorVersion, EncodingVersion: relation.EncodingVersion}
		duplicate := false
		for _, scan := range scans {
			if scan.target == target && scan.occurrence == relation.Occurrence {
				duplicate = true
				break
			}
		}
		if !duplicate {
			scans = append(scans, scanTarget{target: target, occurrence: relation.Occurrence})
		}
	}
	result := make([]txn.ReadDependency, 0, len(dependencies))
	for _, dependency := range dependencies {
		if dependency.Occurrence != 0 {
			result = append(result, dependency)
			continue
		}
		matched := false
		for _, scan := range scans {
			if dependency.Target != scan.target {
				continue
			}
			owned := dependency
			owned.Occurrence = scan.occurrence
			result = append(result, owned)
			matched = true
		}
		if !matched {
			result = append(result, dependency)
		}
	}
	return result
}

func openPreparedReadView(ctx context.Context, transaction *txn.Transaction, execution statementExecution, options PreparedExecution) (txn.ReadView, error) {
	if execution.prepared == nil {
		return nil, errPreparedInvocation
	}
	statistics, err := preparedStatisticsDependencies(execution.prepared, options.Statistics)
	if err != nil {
		return nil, err
	}
	limit := options.Environment.Limits.MetadataBytes
	claim, err := options.Environment.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, err
	}
	defer claim.Release()
	limit, err = claim.GrowUpTo(limit)
	if err != nil {
		return nil, err
	}
	// The plan's own read set always applies: caller declarations may carry only
	// the catalog discovery span, which never covers negotiated data scans.
	planDependencies, _, err := execution.prepared.ReadDependencies(ctx, options.Catalog, limit)
	if err != nil {
		return nil, err
	}
	dependencies := bindScanOccurrences(execution.prepared.bound, options.Dependencies)
	dependencies = append(dependencies, planDependencies...)
	dependencies = append(dependencies, statistics...)
	if boundHasCatalogScan(execution.prepared.bound) {
		dependencies = append(dependencies, catalogScanDependencies(options.Catalog, options.Environment.Catalog)...)
	}
	return openPreparedTransactionReadView(ctx, transaction, execution.prepared, dependencies)
}

func openPreparedTransactionReadView(ctx context.Context, transaction *txn.Transaction, prepared *Prepared, dependencies []txn.ReadDependency) (txn.ReadView, error) {
	policies := preparedRowLockPolicies(prepared)
	if len(policies) == 0 {
		return transaction.ReadView(ctx, dependencies)
	}
	return transaction.ReadViewForRowLocks(ctx, dependencies, policies)
}

func preparedRowLockPolicies(prepared *Prepared) []txn.RowLockPolicy {
	if prepared == nil {
		return nil
	}
	return boundRowLockPolicies(prepared.bound)
}

func boundRowLockPolicies(bound *bind.BoundStatement) []txn.RowLockPolicy {
	if bound == nil {
		return nil
	}
	graph := bound.Graph()
	var policies []txn.RowLockPolicy
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		relation, ok := graph.Rel(id)
		if !ok || relation.Kind != ir.LockRows {
			continue
		}
		for index := range relation.LockCount() {
			lock, ok := graph.RowLock(id, index)
			if !ok {
				continue
			}
			policy := txn.RowLockPolicy{
				Occurrence: lock.Occurrence,
				Strength:   txn.RowLockStrength(lock.Strength - 1),
				Wait:       txn.RowLockWait(lock.Wait),
			}
			found := false
			for i := range policies {
				if policies[i].Occurrence != policy.Occurrence {
					continue
				}
				if policy.Strength > policies[i].Strength {
					policies[i].Strength = policy.Strength
				}
				if rowLockWaitRank(policy.Wait) > rowLockWaitRank(policies[i].Wait) {
					policies[i].Wait = policy.Wait
				}
				found = true
				break
			}
			if !found {
				policies = append(policies, policy)
			}
		}
	}
	return policies
}

func rowLockWaitRank(wait txn.RowLockWait) uint8 {
	switch wait {
	case txn.WaitForRowLock:
		return 0
	case txn.SkipLockedRow:
		return 1
	case txn.NoWaitForRowLock:
		return 2
	default:
		return 0
	}
}

func preparedStatisticsDependencies(prepared *Prepared, options *StatisticsCommandOptions) ([]txn.ReadDependency, error) {
	if prepared == nil || len(prepared.statistics) == 0 {
		return nil, nil
	}
	if options == nil || options.Store == nil {
		return nil, errPreparedDependency
	}
	dependencies := make([]txn.ReadDependency, len(prepared.statistics))
	for i, version := range prepared.statistics {
		dependency, err := options.Store.PublicationDependency(version.Target)
		if err != nil {
			return nil, err
		}
		dependencies[i] = dependency
	}
	return dependencies, nil
}

func preparedHasSecondaryScan(prepared *Prepared) bool {
	plan := prepared.graph.Plan()
	for index := uint32(1); index <= plan.NodeCount(); index++ {
		node, _ := plan.Node(physical.NodeID(index))
		if node.Metadata().Algorithm == physical.SecondaryScan {
			return true
		}
	}
	return false
}

func validatePreparedIndexes(ctx context.Context, prepared *Prepared, snapshot *catalog.CatalogSnapshot) error {
	plan := prepared.graph.Plan()
	for index := uint32(1); index <= plan.NodeCount(); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		node, _ := plan.Node(physical.NodeID(index))
		if node.Metadata().Algorithm != physical.SecondaryScan {
			continue
		}
		source, target := node.ReadTargets()
		published, err := snapshot.Descriptor(ctx, target.ObjectID)
		if err != nil {
			return err
		}
		descriptor := published.Descriptor
		identity := descriptor.Physical()
		wrongKind := descriptor.Kind() != model.ObjectIndex || descriptor.State() != model.StatePublic
		wrongVersion := descriptor.Version() != target.DescriptorVersion || identity.Generation != target.Generation || identity.EncodingVersion != target.EncodingVersion
		if wrongKind || wrongVersion {
			return errPreparedDependency
		}
		spec, err := descriptor.Spec(ctx)
		if err != nil {
			return err
		}
		if spec.Index == nil || spec.Index.Table != source.ObjectID {
			return errPreparedDependency
		}
	}
	return nil
}
