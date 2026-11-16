package session

import (
	"context"
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
)

// DeclarationOptions borrows database collaborators. PolicyBytes bounds retained
// policy ownership per target; compiler and descriptor limits remain independent.
type DeclarationOptions struct {
	Catalog     *catalog.Store
	Encoder     storage.KeyEncoder
	Columns     ColumnExpressionOptions
	Indexes     IndexExpressionOptions
	Limits      schema.IndexLimits
	PolicyBytes uint64
}

// DeclarationResult owns immutable mutation capabilities. Its catalog identities
// are usable only while the creating transaction's branch remains visible.
type DeclarationResult struct {
	targets []execution.MutationTarget
	// rows holds the admitted table normalizer per table-shaped target. A
	// caller that keeps the target's descriptor out of the catalog cannot
	// rebuild it later, so it retains the normalizer with the target.
	rows map[model.ObjectID]*txn.RowNormalizer
}

// Targets returns the mutation targets this declaration admitted, in
// descriptor-id order. A session that keeps descriptors out of the catalog
// retains them itself so later statements can still resolve their write policy.
func (r *DeclarationResult) Targets() []execution.MutationTarget {
	if r == nil {
		return nil
	}
	return r.targets
}

func (r *DeclarationResult) ResolveMutationTarget(ctx context.Context, target storage.ObjectRef) (execution.MutationTarget, error) {
	if err := ctx.Err(); err != nil {
		return execution.MutationTarget{}, err
	}
	if r != nil {
		for _, candidate := range r.targets {
			d := candidate.Descriptor
			p := d.Physical()
			if target == (storage.ObjectRef{ObjectID: d.ID(), Generation: p.Generation, DescriptorVersion: d.Version(), EncodingVersion: p.EncodingVersion}) {
				return candidate, nil
			}
		}
	}
	return execution.MutationTarget{}, storage.ErrInvalidTarget
}

// StageDeclaration stages catalog rows and policies in the caller's Statement.
// It revalidates external dependencies under PCC and never commits. The caller
// must propagate errors to roll back both identity reservations and publication.
func StageDeclaration(ctx context.Context, transaction *txn.Transaction, bound *bind.BoundDeclaration, options DeclarationOptions) (*DeclarationResult, error) {
	if transaction == nil || bound == nil || options.Catalog == nil || options.Encoder == nil || options.PolicyBytes == 0 || options.Limits.MetadataBytes == 0 {
		return nil, &model.Error{Code: "22023", Message: "invalid declaration staging options"}
	}
	if err := transaction.RequireStatement(ctx); err != nil {
		return nil, err
	}
	descriptors := bound.Descriptors()
	updates := bound.Updates()
	if bound.Existing() != nil {
		return validateExistingDeclaration(ctx, transaction, bound.Existing(), options.Catalog)
	}
	if len(descriptors) == 0 && len(updates) == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	base := bound.Base()
	if base != nil {
		if len(descriptors) != 1 || len(updates) != 0 || descriptors[0].ID() != base.ID() || descriptors[0].Version() != base.Version()+1 {
			return nil, model.ErrInvalidDescriptor
		}
		updates = append(updates, bind.ObjectMutation{Base: base, Next: descriptors[0]})
		descriptors = nil
	}
	var result *DeclarationResult
	err := func() error {
		dependencies := make([]txn.ReadDependency, 0, len(descriptors)*3+len(updates)+len(bound.Dependencies()))
		for _, d := range descriptors {
			created, err := options.Catalog.CreateDependencies(ctx, d)
			if err != nil {
				return err
			}
			dependencies = append(dependencies, created...)
		}
		for _, update := range updates {
			if update.Base == nil || update.Next == nil || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 {
				return model.ErrInvalidDescriptor
			}
			dependencies = append(dependencies, options.Catalog.DescriptorDependency(update.Base.ID()))
		}
		external := bound.Dependencies()
		for _, dep := range external {
			dependencies = append(dependencies, options.Catalog.DescriptorDependency(dep.Object))
		}
		view, err := transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		if err := stageDeclarationMetadata(ctx, transaction, snapshot, descriptors, updates, external, options.Catalog); err != nil {
			return errors.Join(err, snapshot.Close(), view.Close())
		}
		if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
			return err
		}
		// Admission compiles stored generated columns, defaults and check
		// constraints by binding their expressions, and binding resolves user
		// types and operators by name through the schema and per-kind
		// namespaces. Those reads run under the snapshot opened below, so
		// declare them first; the discovery pass reads only name indexes and
		// declares exactly the rows it found.
		dependencies, err = catalogDiscoveryDependencies(ctx, transaction, options.Catalog, catalogDiscovery{}, dependencies, options.Limits.MetadataBytes)
		if err != nil {
			return err
		}
		// Resolving an attached partition's hierarchy scans the schema and table
		// namespaces and resolves a descriptor row per discovered entry, so those
		// reads must be declared before the snapshot that performs them is
		// opened. The relational catalog discovery already declares exactly that
		// read set (namespace ranges plus a descriptor row per discovered
		// object) and is the established protected-discovery path.
		if declarationNeedsHierarchy(descriptors) {
			dependencies, err = declarationRelationalCatalogDependencies(ctx, transaction, options.Catalog, dependencies, options.Limits.MetadataBytes)
			if err != nil {
				return err
			}
		}
		view, err = transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err = options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		inventory, relationalSchemas, err := declarationRelationalRequirements(ctx, snapshot, descriptors, options.Limits.MetadataBytes)
		if err != nil {
			return errors.Join(err, snapshot.Close(), view.Close())
		}
		if inventory || len(relationalSchemas) != 0 {
			if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
				return err
			}
			if inventory {
				dependencies, err = declarationRelationalCatalogDependencies(ctx, transaction, options.Catalog, dependencies, options.Limits.MetadataBytes)
				if err != nil {
					return err
				}
			} else {
				for _, schemaID := range relationalSchemas {
					dependencies = append(dependencies, options.Catalog.DescriptorDependency(schemaID))
				}
			}
			view, err = transaction.ReadView(ctx, dependencies)
			if err != nil {
				return err
			}
			snapshot, err = options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
			if err != nil {
				return errors.Join(err, view.Close())
			}
		}
		result, err = admitDeclaration(ctx, transaction, snapshot, descriptors, options)
		return errors.Join(err, snapshot.Close(), view.Close())
	}()
	if err != nil {
		return nil, err
	}
	return result, nil
}

func declarationRelationalRequirements(ctx context.Context, snapshot *catalog.CatalogSnapshot, descriptors []*model.Descriptor, maxBytes uint64) (bool, []model.ObjectID, error) {
	tables, err := declarationHierarchy(ctx, snapshot, descriptors, maxBytes)
	if err != nil {
		return false, nil, err
	}
	schemaSet := make(map[model.ObjectID]struct{})
	inventory := false
	for _, descriptor := range tables {
		table, ok := descriptor.Table()
		if !ok {
			continue
		}
		link := false
		for _, constraint := range table.Constraints {
			relational := constraint.Enforced && (constraint.Kind == model.ConstraintForeignKey || constraint.Kind == model.ConstraintExclude)
			deferredKey := constraint.Enforced && constraint.Deferrable && (constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique)
			link = link || relational || deferredKey
		}
		if link {
			schemaSet[descriptor.Name().Parent] = struct{}{}
		}
		inventory = inventory || schema.DeclarationRelationalInventoryRequired(descriptor)
	}
	schemas := make([]model.ObjectID, 0, len(schemaSet))
	for schemaID := range schemaSet {
		schemas = append(schemas, schemaID)
	}
	slices.Sort(schemas)
	return inventory, schemas, ctx.Err()
}

func inspectDeclarationCatalog(ctx context.Context, transaction *txn.Transaction, store *catalog.Store, dependencies []txn.ReadDependency, inspect func(*catalog.CatalogSnapshot) error) (err error) {
	view, err := transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	if inspect == nil {
		err = model.ErrInvalidDescriptor
	} else {
		err = inspect(snapshot)
	}
	return errors.Join(err, snapshot.Close(), view.Close())
}

func declarationRelationalCatalogDependencies(ctx context.Context, transaction *txn.Transaction, store *catalog.Store, base []txn.ReadDependency, maxBytes uint64) ([]txn.ReadDependency, error) {
	if transaction == nil || store == nil || maxBytes == 0 {
		return nil, model.ErrValueBudget
	}
	remaining := maxBytes
	charge := func(bytes uint64) error {
		if bytes > remaining {
			return model.ErrValueBudget
		}
		remaining -= bytes
		return nil
	}
	baseBytes := uint64(len(base)) * uint64(unsafe.Sizeof(txn.ReadDependency{}))
	if len(base) != 0 && baseBytes/uint64(len(base)) != uint64(unsafe.Sizeof(txn.ReadDependency{})) {
		return nil, model.ErrValueBudget
	}
	if err := charge(baseBytes); err != nil {
		return nil, err
	}
	dependencies := slices.Clone(base)
	const dependencyBytes = uint64(512)
	appendDependency := func(dependency txn.ReadDependency) error {
		if err := charge(dependencyBytes); err != nil {
			return err
		}
		dependencies = append(dependencies, dependency)
		return nil
	}
	root, err := store.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return nil, err
	}
	if err := appendDependency(root); err != nil {
		return nil, err
	}
	referenceBytes := func(reference catalog.NamespaceReference) (uint64, error) {
		bytes := uint64(unsafe.Sizeof(reference))
		names := uint64(len(reference.Name.Name)) + uint64(len(reference.Name.Signature))
		if names > ^uint64(0)-bytes {
			return 0, model.ErrValueBudget
		}
		return bytes + names, nil
	}
	schemas := make([]catalog.NamespaceReference, 0)
	seenSchemas := make(map[model.ObjectID]struct{})
	err = inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		return snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference catalog.NamespaceReference) error {
			if reference.ID == 0 {
				return catalog.ErrCorruptCatalog
			}
			if _, duplicate := seenSchemas[reference.ID]; duplicate {
				return catalog.ErrCorruptCatalog
			}
			bytes, err := referenceBytes(reference)
			if err != nil {
				return err
			}
			if err := charge(bytes); err != nil {
				return err
			}
			seenSchemas[reference.ID] = struct{}{}
			schemas = append(schemas, reference)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	seenDescriptors := make(map[model.ObjectID]struct{}, len(schemas))
	appendDescriptor := func(id model.ObjectID) error {
		if id == 0 || id >= catalog.NamespaceObjectID {
			return catalog.ErrCorruptCatalog
		}
		if _, exists := seenDescriptors[id]; exists {
			return nil
		}
		if err := appendDependency(store.DescriptorDependency(id)); err != nil {
			return err
		}
		seenDescriptors[id] = struct{}{}
		return nil
	}
	for _, reference := range schemas {
		if err := appendDescriptor(reference.ID); err != nil {
			return nil, err
		}
		dependency, err := store.NamespaceScanDependency(ctx, reference.ID, model.ObjectTable)
		if err != nil {
			return nil, err
		}
		if err := appendDependency(dependency); err != nil {
			return nil, err
		}
	}
	tables := make([]catalog.NamespaceReference, 0)
	seenTables := make(map[model.ObjectID]struct{})
	err = inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		position := 0
		if err := snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference catalog.NamespaceReference) error {
			if position >= len(schemas) || schemas[position] != reference {
				return catalog.ErrDescriptorChanged
			}
			position++
			return nil
		}); err != nil {
			return err
		}
		if position != len(schemas) {
			return catalog.ErrDescriptorChanged
		}
		for _, schemaReference := range schemas {
			if err := snapshot.ScanNamespaceReferences(ctx, schemaReference.ID, model.ObjectTable, func(reference catalog.NamespaceReference) error {
				if reference.ID == 0 {
					return catalog.ErrCorruptCatalog
				}
				if _, duplicate := seenTables[reference.ID]; duplicate {
					return catalog.ErrCorruptCatalog
				}
				bytes, err := referenceBytes(reference)
				if err != nil {
					return err
				}
				if err := charge(bytes); err != nil {
					return err
				}
				seenTables[reference.ID] = struct{}{}
				tables = append(tables, reference)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, reference := range tables {
		if err := appendDescriptor(reference.ID); err != nil {
			return nil, err
		}
	}
	indexes := make([]model.ObjectID, 0)
	seenIndexes := make(map[model.ObjectID]struct{})
	err = inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		for _, reference := range tables {
			published, err := snapshot.Descriptor(ctx, reference.ID)
			if err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.ID() != reference.ID || descriptor.Version() != reference.Version || descriptor.Name() != reference.Name || descriptor.Kind() != model.ObjectTable {
				return catalog.ErrDescriptorChanged
			}
			if descriptor.State() != model.StatePublic {
				continue
			}
			table, ok := descriptor.Table()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			for _, id := range table.Indexes {
				if _, duplicate := seenIndexes[id]; duplicate {
					continue
				}
				if err := appendDescriptor(id); err != nil {
					return err
				}
				if err := charge(uint64(unsafe.Sizeof(id))); err != nil {
					return err
				}
				seenIndexes[id] = struct{}{}
				indexes = append(indexes, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		appendExpressionDependencies := func(definition *model.ExpressionDefinition) error {
			if definition == nil {
				return nil
			}
			for _, dependency := range definition.Dependencies {
				if err := appendDescriptor(dependency.Object); err != nil {
					return err
				}
			}
			return nil
		}
		for _, id := range indexes {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.Kind() != model.ObjectIndex {
				return model.ErrInvalidDescriptor
			}
			index, ok := descriptor.Index()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			for _, key := range index.Keys {
				if err := appendExpressionDependencies(key.Expression); err != nil {
					return err
				}
			}
			if err := appendExpressionDependencies(index.Predicate); err != nil {
				return err
			}
			for _, dependency := range index.PendingDependencies {
				if err := appendDescriptor(dependency.Object); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dependencies, ctx.Err()
}

func stageDeclarationMetadata(ctx context.Context, transaction *txn.Transaction, snapshot *catalog.CatalogSnapshot, descriptors []*model.Descriptor, updates []bind.ObjectMutation, dependencies []model.Dependency, store *catalog.Store) error {
	for _, dep := range dependencies {
		d, err := snapshot.Descriptor(ctx, dep.Object)
		if err != nil {
			return err
		}
		if d.Descriptor.Version() != dep.Version || d.Descriptor.State() != model.StatePublic {
			return catalog.ErrDescriptorChanged
		}
	}
	for _, d := range descriptors {
		change, err := store.Create(ctx, snapshot, d)
		if err != nil {
			return err
		}
		if err := transaction.ApplyMutations(ctx, change.Mutations); err != nil {
			return err
		}
	}
	for _, update := range updates {
		change, err := store.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return err
		}
		if err := transaction.ApplyMutations(ctx, change.Mutations); err != nil {
			return err
		}
	}
	return nil
}

func admitDeclaration(ctx context.Context, transaction *txn.Transaction, snapshot *catalog.CatalogSnapshot, descriptors []*model.Descriptor, options DeclarationOptions) (*DeclarationResult, error) {
	return declarationTargets(ctx, transaction, snapshot, descriptors, options, true)
}

// declarationTargets assembles the mutation targets of the supplied table
// descriptors. admitPolicies transfers each new table's policy and index layouts
// to the transaction, which the create path needs before it populates rows; a
// caller that only refreshes the target of an already-created relation passes
// false, because re-admitting a changed physical target is rejected.
func declarationTargets(ctx context.Context, transaction *txn.Transaction, snapshot *catalog.CatalogSnapshot, descriptors []*model.Descriptor, options DeclarationOptions, admitPolicies bool) (*DeclarationResult, error) {
	result := &DeclarationResult{}
	declared := make(map[model.ObjectID]*model.Descriptor, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor != nil {
			declared[descriptor.ID()] = descriptor
		}
	}
	tables, err := declarationHierarchy(ctx, snapshot, descriptors, options.Limits.MetadataBytes)
	if err != nil {
		return nil, err
	}
	columns, err := NewColumnExpressionCompiler(snapshot, options.Columns)
	if err != nil {
		return nil, err
	}
	indexes, err := NewIndexExpressionCompiler(snapshot, options.Indexes)
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		if table.Kind() != model.ObjectTable && table.Kind() != model.ObjectMaterializedView {
			continue
		}
		metadata, ok := table.Table()
		if !ok {
			continue
		}
		admit := admitPolicies && declared[table.ID()] != nil
		var retainedBytes uint64
		var policy *txn.RowNormalizer
		if admit {
			retainedBytes, err = admitDeclarationPolicyBytes(ctx, table, options)
			if err != nil {
				return nil, err
			}
			policy, err = schema.NewTablePolicy(ctx, schema.TablePolicyOptions{Descriptor: table, Encoder: options.Encoder, Expressions: columns, MetadataBytes: options.Limits.MetadataBytes, RowBytes: options.Limits.EntryBytes, MaxColumns: options.Limits.MaxColumns})
			if err != nil {
				return nil, err
			}
		}
		plans := make([]*schema.IndexPlan, 0, len(metadata.Indexes))
		for _, id := range metadata.Indexes {
			index := declared[id]
			if index == nil {
				published, descriptorErr := snapshot.Descriptor(ctx, id)
				if descriptorErr != nil {
					return nil, descriptorErr
				}
				index = published.Descriptor
			}
			if index == nil || index.Kind() != model.ObjectIndex {
				return nil, model.ErrInvalidDescriptor
			}
			plan, planErr := schema.NewIndexPlan(ctx, table, index, options.Encoder, indexes, options.Limits)
			if planErr != nil {
				return nil, planErr
			}
			plans = append(plans, plan)
		}
		expansion, err := schema.NewIndexSet(ctx, table, plans, options.Limits)
		if err != nil {
			return nil, err
		}
		if _, err := schema.LoadDeclarationRelationalConstraints(ctx, snapshot, expansion, options.Encoder, indexes, options.Limits); err != nil {
			return nil, err
		}
		if admit {
			if policy.RetainedBytes() > retainedBytes {
				return nil, model.ErrValueBudget
			}
			if err := transaction.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes()); err != nil {
				return nil, err
			}
			if result.rows == nil {
				result.rows = make(map[model.ObjectID]*txn.RowNormalizer)
			}
			result.rows[table.ID()] = policy
			for _, layout := range expansion.Layouts() {
				indexPolicy, err := expansion.ResolveMutation(ctx, layout.Target)
				if err != nil {
					return nil, err
				}
				if indexPolicy.RetainedBytes() > options.PolicyBytes {
					return nil, model.ErrValueBudget
				}
				if err := transaction.AdmitCreatedTarget(ctx, indexPolicy, indexPolicy.RetainedBytes()); err != nil {
					return nil, err
				}
				result.rows[layout.Target.ObjectID] = indexPolicy
			}
		}
		var partitionExpressions []txn.ValueExpression
		if metadata.PartitionStrategy != 0 {
			partitionExpressions, err = indexes.CompilePartitionExpressions(ctx, table)
			if err != nil {
				return nil, err
			}
		}
		result.targets = append(result.targets, execution.MutationTarget{Descriptor: table, Expansion: expansion, PartitionExpressions: partitionExpressions})
	}
	if err := result.linkHierarchy(); err != nil {
		return nil, err
	}
	return result, nil
}

// declarationNeedsHierarchy reports whether resolving this declaration's table
// hierarchy requires catalog namespace scans. It inspects only the supplied
// descriptors, so callers can decide which reads to declare before any snapshot
// traverses the catalog. An enforced foreign key counts because binding resolves
// each referenced table by name from the namespace collection.
func declarationNeedsHierarchy(descriptors []*model.Descriptor) bool {
	for _, descriptor := range descriptors {
		if descriptor == nil || descriptor.Kind() != model.ObjectTable && descriptor.Kind() != model.ObjectMaterializedView {
			continue
		}
		table, ok := descriptor.Table()
		if !ok {
			continue
		}
		if table.InheritanceChildren != 0 ||
			table.PartitionBound != nil && table.PartitionState == model.PartitionAttached {
			return true
		}
		for _, constraint := range table.Constraints {
			if constraint.Enforced && constraint.Kind == model.ConstraintForeignKey {
				return true
			}
		}
	}
	return false
}

func declarationHierarchy(ctx context.Context, snapshot *catalog.CatalogSnapshot, descriptors []*model.Descriptor, maxBytes uint64) ([]*model.Descriptor, error) {
	if snapshot == nil || maxBytes < uint64(unsafe.Sizeof(execution.MutationTarget{})) {
		return nil, model.ErrValueBudget
	}
	maximum := maxBytes / uint64(unsafe.Sizeof(execution.MutationTarget{}))
	all := make(map[model.ObjectID]*model.Descriptor)
	selected := make(map[model.ObjectID]bool)
	needsHierarchy := false
	for _, descriptor := range descriptors {
		if descriptor == nil {
			continue
		}
		all[descriptor.ID()] = descriptor
		if descriptor.Kind() == model.ObjectTable || descriptor.Kind() == model.ObjectMaterializedView {
			selected[descriptor.ID()] = true
			if table, ok := descriptor.Table(); ok && (table.InheritanceChildren != 0 ||
				table.PartitionBound != nil && table.PartitionState == model.PartitionAttached) {
				needsHierarchy = true
			}
		}
	}
	if !needsHierarchy {
		result := make([]*model.Descriptor, 0, len(selected))
		for id := range selected {
			result = append(result, all[id])
		}
		slices.SortFunc(result, func(left, right *model.Descriptor) int {
			if left.ID() < right.ID() {
				return -1
			}
			if left.ID() > right.ID() {
				return 1
			}
			return 0
		})
		return result, nil
	}
	var visited uint64
	var schemas []*model.Descriptor
	visit := func() error {
		visited++
		if visited > maximum {
			return model.ErrValueBudget
		}
		return ctx.Err()
	}
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		if err := visit(); err != nil {
			return err
		}
		if published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			schemas = append(schemas, published.Descriptor)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, namespace := range schemas {
		if err := snapshot.ScanNamespace(ctx, namespace.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if err := visit(); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor != nil && descriptor.State() == model.StatePublic && all[descriptor.ID()] == nil {
				all[descriptor.ID()] = descriptor
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for range len(all) {
		changed := false
		for id, descriptor := range all {
			if !selected[id] {
				continue
			}
			table, ok := descriptor.Table()
			if !ok || table.PartitionBound == nil || table.PartitionState != model.PartitionAttached {
				continue
			}
			for _, parentID := range table.Parents {
				parent := all[parentID]
				if parent == nil {
					return nil, model.ErrInvalidDescriptor
				}
				parentTable, ok := parent.Table()
				if !ok || parentTable.PartitionStrategy != table.PartitionBound.Strategy {
					continue
				}
				if !selected[parentID] {
					selected[parentID], changed = true, true
				}
			}
		}
		if !changed {
			break
		}
	}
	for range len(all) {
		changed := false
		for id, descriptor := range all {
			if selected[id] {
				continue
			}
			table, ok := descriptor.Table()
			if !ok || !slices.ContainsFunc(table.Parents, func(parent model.ObjectID) bool { return selected[parent] }) {
				continue
			}
			selected[id], changed = true, true
		}
		if !changed {
			break
		}
	}
	result := make([]*model.Descriptor, 0, len(selected))
	for id := range selected {
		result = append(result, all[id])
	}
	slices.SortFunc(result, func(left, right *model.Descriptor) int {
		if left.ID() < right.ID() {
			return -1
		}
		if left.ID() > right.ID() {
			return 1
		}
		return 0
	})
	return result, nil
}

func (r *DeclarationResult) linkHierarchy() error {
	if r == nil {
		return storage.ErrInvalidTarget
	}
	slices.SortFunc(r.targets, func(left, right execution.MutationTarget) int {
		if left.Descriptor.ID() < right.Descriptor.ID() {
			return -1
		}
		if left.Descriptor.ID() > right.Descriptor.ID() {
			return 1
		}
		return 0
	})
	byObject := make(map[model.ObjectID]int, len(r.targets))
	children := make([][]int, len(r.targets))
	for i, target := range r.targets {
		if target.Descriptor == nil || byObject[target.Descriptor.ID()] != 0 {
			return model.ErrInvalidDescriptor
		}
		byObject[target.Descriptor.ID()] = i + 1
	}
	for childIndex, target := range r.targets {
		table, ok := target.Descriptor.Table()
		if !ok || table.PartitionBound != nil && table.PartitionState != model.PartitionAttached {
			continue
		}
		for _, parentID := range table.Parents {
			parentIndex := byObject[parentID] - 1
			if parentIndex >= 0 {
				children[parentIndex] = append(children[parentIndex], childIndex)
			}
		}
	}
	visiting := make([]bool, len(r.targets))
	ready := make([]bool, len(r.targets))
	built := make([]execution.MutationTarget, len(r.targets))
	var build func(int) (execution.MutationTarget, error)
	build = func(index int) (execution.MutationTarget, error) {
		if index < 0 || index >= len(r.targets) || visiting[index] {
			return execution.MutationTarget{}, model.ErrInvalidDescriptor
		}
		if ready[index] {
			return built[index], nil
		}
		visiting[index] = true
		target := r.targets[index]
		target.PartitionChildren = make([]execution.MutationTarget, len(children[index]))
		for i, child := range children[index] {
			nested, err := build(child)
			if err != nil {
				return execution.MutationTarget{}, err
			}
			target.PartitionChildren[i] = nested
		}
		visiting[index] = false
		built[index], ready[index] = target, true
		return target, nil
	}
	linked := make([]execution.MutationTarget, len(r.targets))
	for i := range r.targets {
		target, err := build(i)
		if err != nil {
			return err
		}
		linked[i] = target
	}
	for i := range linked {
		root, err := partitionRoutingRoot(i, linked, byObject)
		if err != nil {
			return err
		}
		if root != i {
			linked[i].PartitionRouting = &linked[root]
		}
	}
	r.targets = linked
	return nil
}

func partitionRoutingRoot(index int, targets []execution.MutationTarget, byObject map[model.ObjectID]int) (int, error) {
	current := index
	for range len(targets) {
		if current < 0 || current >= len(targets) || targets[current].Descriptor == nil {
			return 0, model.ErrInvalidDescriptor
		}
		table, ok := targets[current].Descriptor.Table()
		if !ok {
			return 0, model.ErrInvalidDescriptor
		}
		if table.PartitionBound == nil || table.PartitionState != model.PartitionAttached {
			if current != index && table.PartitionStrategy == 0 {
				return 0, model.ErrInvalidDescriptor
			}
			return current, nil
		}
		if len(table.Parents) != 1 || table.PartitionBound.Strategy == 0 {
			return 0, model.ErrInvalidDescriptor
		}
		parent := byObject[table.Parents[0]] - 1
		if parent < 0 || parent >= len(targets) {
			return 0, model.ErrInvalidDescriptor
		}
		parentTable, ok := targets[parent].Descriptor.Table()
		if !ok || parentTable.PartitionStrategy != table.PartitionBound.Strategy {
			return 0, model.ErrInvalidDescriptor
		}
		current = parent
	}
	return 0, model.ErrInvalidDescriptor
}

func validateExistingDeclaration(ctx context.Context, transaction *txn.Transaction, existing *model.Descriptor, store *catalog.Store) (*DeclarationResult, error) {
	err := func() error {
		name, err := store.NamespaceDependency(ctx, existing.Name())
		if err != nil {
			return err
		}
		view, err := transaction.ReadView(ctx, []txn.ReadDependency{name, store.DescriptorDependency(existing.ID())})
		if err != nil {
			return err
		}
		snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		current, err := snapshot.Resolve(ctx, existing.Name())
		if err == nil && (current.Descriptor.ID() != existing.ID() || current.Descriptor.Version() != existing.Version()) {
			err = catalog.ErrDescriptorChanged
		}
		return errors.Join(err, snapshot.Close(), view.Close())
	}()
	if err != nil {
		return nil, err
	}
	return &DeclarationResult{}, nil
}

func admitDeclarationPolicyBytes(ctx context.Context, table *model.Descriptor, options DeclarationOptions) (uint64, error) {
	spec, err := table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: options.Limits.MetadataBytes, MaxItems: options.Limits.MaxColumns, MaxDepth: 64})
	if err != nil {
		return 0, err
	}
	remaining := options.PolicyBytes
	charge := func(bytes uint64) error {
		if bytes > remaining {
			return &model.Error{Code: "54000", Message: "declared policy retention exceeds budget"}
		}
		remaining -= bytes
		return nil
	}
	for range 2 {
		if err := charge(options.Limits.MetadataBytes); err != nil {
			return 0, err
		}
	}
	expressions := 0
	for _, column := range spec.Table.Columns {
		if column.Default != nil || column.IdentitySequence != 0 {
			expressions++
		}
		if column.Generated != nil {
			expressions++
		}
	}
	for _, constraint := range spec.Table.Constraints {
		if constraint.Kind == model.ConstraintCheck && constraint.Enforced {
			expressions++
		}
	}
	for range expressions {
		if err := charge(options.Columns.Evaluation.MaxBytes); err != nil {
			return 0, err
		}
	}
	return options.PolicyBytes - remaining, nil
}
