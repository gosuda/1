package session

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/partition"
)

func declarationPartitionAttachment(declaration *bind.BoundDeclaration) (parent, child *model.Descriptor, ok bool, err error) {
	if declaration == nil || !declaration.RequiresPartitionValidation() {
		return nil, nil, false, nil
	}
	for _, descriptor := range declaration.Descriptors() {
		if descriptor == nil {
			return nil, nil, false, model.ErrInvalidDescriptor
		}
		if metadata, table := descriptor.Table(); table && metadata.PartitionBound != nil && !metadata.PartitionBound.Default {
			if child != nil {
				return nil, nil, false, model.ErrInvalidDescriptor
			}
			child = descriptor
		}
	}
	if child == nil {
		return nil, nil, false, model.ErrInvalidDescriptor
	}
	childTable, _ := child.Table()
	for _, update := range declaration.Updates() {
		if update.Base == nil || update.Next == nil || !slices.Contains(childTable.Parents, update.Next.ID()) {
			continue
		}
		before, beforeOK := update.Base.Table()
		after, afterOK := update.Next.Table()
		if beforeOK && afterOK && before.PartitionDefault != 0 && after.PartitionDefault == before.PartitionDefault {
			parent = update.Next
			break
		}
	}
	if parent == nil {
		return nil, nil, false, model.ErrInvalidDescriptor
	}
	return parent, child, true, nil
}

func (transactions *Transactions) populateTableDeclaration(ctx context.Context, declaration *bind.BoundDeclaration, invocation StatementExecution) (affected uint64, err error) {
	environment := &invocation.Execution.Environment
	defer func() {
		err = rejectPreparedEnvironment(*environment, err)
	}()
	parent, child, required, err := declarationPartitionAttachment(declaration)
	if err != nil || !required {
		return 0, err
	}
	if environment.Query == nil || invocation.Planning == nil || invocation.Execution.Catalog == nil || invocation.Execution.ObjectDDL == nil || environment.Reader != nil {
		return 0, errPreparedInvocation
	}
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
	return transactions.populatePartitionAttachment(ctx, parent, child, nil, invocation)
}

func hierarchyAttachment(ctx context.Context, command *bind.BoundObjectCommand) (parent, child *model.Descriptor, indexes []*model.Descriptor, partition, ok bool, err error) {
	if command == nil {
		return nil, nil, nil, false, false, nil
	}
	updates := command.Updates()
	for _, update := range updates {
		before, beforeOK := update.Base.Table()
		after, afterOK := update.Next.Table()
		if !beforeOK || !afterOK || len(after.Parents) <= len(before.Parents) {
			continue
		}
		parentID := model.ObjectID(0)
		for _, candidate := range after.Parents {
			if !slices.Contains(before.Parents, candidate) {
				parentID = candidate
				break
			}
		}
		if parentID == 0 {
			continue
		}
		child = update.Next
		for _, candidate := range updates {
			if candidate.Next.ID() == parentID {
				parent = candidate.Next
				break
			}
		}
		partition = before.PartitionBound == nil && after.PartitionBound != nil
		break
	}
	if parent == nil || child == nil {
		return nil, nil, nil, false, false, nil
	}
	if partition {
		for _, descriptor := range command.Creates() {
			if descriptor.Kind() != model.ObjectIndex {
				continue
			}
			spec, specErr := descriptor.Spec(ctx)
			if specErr != nil {
				return nil, nil, nil, false, false, specErr
			}
			if spec.Index != nil {
				indexes = append(indexes, descriptor)
			}
		}
	}
	return parent, child, indexes, partition, true, nil
}

type partitionAttachmentScan struct {
	children        []*model.Descriptor
	candidateLeaves []*model.Descriptor
	fallback        *model.Descriptor
	fallbackLeaves  []*model.Descriptor
}

func (transactions *Transactions) openAttachmentSnapshot(ctx context.Context, dependencies []txn.ReadDependency) (txn.ReadView, *catalog.CatalogSnapshot, error) {
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return nil, nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	return view, snapshot, nil
}

// inspectAttachmentReferences opens a protected view over dependencies, builds a
// snapshot, and hands it to inspect. The view and snapshot are always released.
func inspectAttachmentReferences(ctx context.Context, transactions *Transactions, dependencies []txn.ReadDependency, inspect func(*catalog.CatalogSnapshot) error) (err error) {
	view, snapshot, err := transactions.openAttachmentSnapshot(ctx, dependencies)
	if err != nil {
		return err
	}
	if inspect == nil {
		err = model.ErrInvalidDescriptor
	} else {
		err = inspect(snapshot)
	}
	return errors.Join(err, snapshot.Close(), view.Close())
}

// attachmentIndexDependencies declares the descriptors of every index already
// attached to the supplied tables. Planning a table's index set reads those rows to
// rebuild each index plan, so they are part of the attachment's read set.
func attachmentIndexDependencies(ctx context.Context, store *catalog.Store, descriptors []*model.Descriptor, maximum uint32) ([]txn.ReadDependency, error) {
	if store == nil || maximum == 0 {
		return nil, model.ErrDescriptorLimit
	}
	seen := make(map[model.ObjectID]bool)
	dependencies := make([]txn.ReadDependency, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor == nil {
			return nil, model.ErrInvalidDescriptor
		}
		metadata, ok := descriptor.Table()
		if !ok {
			return nil, model.ErrInvalidDescriptor
		}
		for _, id := range metadata.Indexes {
			if id == 0 || id >= catalog.NamespaceObjectID {
				return nil, model.ErrInvalidDescriptor
			}
			if seen[id] {
				continue
			}
			if uint32(len(seen)) == maximum {
				return nil, model.ErrDescriptorLimit
			}
			seen[id] = true
			dependencies = append(dependencies, store.DescriptorDependency(id))
		}
	}
	return dependencies, ctx.Err()
}

func (transactions *Transactions) attachmentCatalogDependencies(ctx context.Context, original []txn.ReadDependency, maximum uint32) ([]txn.ReadDependency, error) {
	if maximum == 0 {
		return nil, model.ErrDescriptorLimit
	}
	store := transactions.sequences.config.Catalog
	root, err := store.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return nil, err
	}
	dependencies := append(slices.Clone(original), root)
	view, snapshot, err := transactions.openAttachmentSnapshot(ctx, dependencies)
	if err != nil {
		return nil, err
	}
	// Discovery must not resolve descriptors: the view declares only the schema
	// namespace span, and ScanNamespace would read each schema's descriptor row.
	// ScanNamespaceReferences reads the namespace collection alone, so the schemas
	// it reports can then be declared before anything resolves them.
	var references []catalog.NamespaceReference
	err = snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference catalog.NamespaceReference) error {
		if reference.ID == 0 || reference.ID >= catalog.NamespaceObjectID {
			return catalog.ErrCorruptCatalog
		}
		if uint32(len(references)) == maximum {
			return model.ErrDescriptorLimit
		}
		references = append(references, reference)
		return nil
	})
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		return nil, err
	}
	seen := make(map[model.ObjectID]bool, len(references))
	for _, reference := range references {
		if seen[reference.ID] {
			continue
		}
		seen[reference.ID] = true
		dependencies = append(dependencies, store.DescriptorDependency(reference.ID))
		dependency, err := store.NamespaceScanDependency(ctx, reference.ID, model.ObjectTable)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	// Discovering the child set re-scans every schema's table namespace through
	// ScanNamespace, which resolves each visited table's descriptor. Declare those
	// descriptor rows too, so the discovery and confirmation passes read only what
	// this declaration covers.
	var tables []catalog.NamespaceReference
	err = inspectAttachmentReferences(ctx, transactions, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		for _, reference := range references {
			if err := snapshot.ScanNamespaceReferences(ctx, reference.ID, model.ObjectTable, func(table catalog.NamespaceReference) error {
				if table.ID == 0 || table.ID >= catalog.NamespaceObjectID {
					return catalog.ErrCorruptCatalog
				}
				if uint32(len(tables)) == maximum {
					return model.ErrDescriptorLimit
				}
				tables = append(tables, table)
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
	for _, table := range tables {
		if seen[table.ID] {
			continue
		}
		seen[table.ID] = true
		dependencies = append(dependencies, store.DescriptorDependency(table.ID))
	}
	return dependencies, ctx.Err()
}

func attachmentDataDependencies(ctx context.Context, store *catalog.Store, descriptors []*model.Descriptor, limits model.DescriptorLimits, maximum uint32) ([]txn.ReadDependency, error) {
	if store == nil || maximum == 0 {
		return nil, model.ErrDescriptorLimit
	}
	seen := make(map[model.ObjectID]bool, len(descriptors))
	dependencies := make([]txn.ReadDependency, 0, len(descriptors)*2)
	full := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	for _, descriptor := range descriptors {
		if descriptor == nil {
			return nil, model.ErrInvalidDescriptor
		}
		if seen[descriptor.ID()] {
			continue
		}
		if uint32(len(seen)) == maximum {
			return nil, model.ErrDescriptorLimit
		}
		seen[descriptor.ID()] = true
		target, keys, _, err := analyzeTableLayout(ctx, descriptor, limits)
		if err != nil || len(keys) == 0 {
			return nil, errors.Join(err, model.ErrInvalidDescriptor)
		}
		dependencies = append(dependencies,
			store.DescriptorDependency(descriptor.ID()),
			txn.ReadDependency{Target: target, KeyLayout: keys, Spans: []storage.KeySpan{full}},
		)
	}
	return dependencies, ctx.Err()
}

func sameAttachmentDescriptors(expected, actual []*model.Descriptor) bool {
	if len(expected) != len(actual) {
		return false
	}
	targets := make(map[model.ObjectID]storage.ObjectRef, len(expected))
	for _, descriptor := range expected {
		if descriptor == nil || targets[descriptor.ID()] != (storage.ObjectRef{}) {
			return false
		}
		physical := descriptor.Physical()
		targets[descriptor.ID()] = storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
	}
	for _, descriptor := range actual {
		if descriptor == nil {
			return false
		}
		physical := descriptor.Physical()
		target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
		if targets[descriptor.ID()] != target {
			return false
		}
		delete(targets, descriptor.ID())
	}
	return len(targets) == 0
}

func partitionAttachmentDescriptors(ctx context.Context, snapshot *catalog.CatalogSnapshot, parent, child *model.Descriptor, maximum uint32) (partitionAttachmentScan, error) {
	var result partitionAttachmentScan
	children, err := directPartitionChildren(ctx, snapshot, parent, maximum)
	if err != nil {
		return result, err
	}
	foundChild := false
	for i, candidate := range children {
		if candidate.ID() == child.ID() {
			children[i], foundChild = child, true
			break
		}
	}
	if !foundChild {
		children = append(children, child)
		slices.SortFunc(children, func(left, right *model.Descriptor) int {
			if left.ID() < right.ID() {
				return -1
			}
			if left.ID() > right.ID() {
				return 1
			}
			return 0
		})
	}
	result.children = children
	result.candidateLeaves = []*model.Descriptor{child}
	childTable, ok := child.Table()
	if !ok {
		return result, model.ErrInvalidDescriptor
	}
	if childTable.PartitionStrategy != 0 {
		result.candidateLeaves, err = partitionPhysicalLeaves(ctx, snapshot, child, maximum)
		if err != nil {
			return result, err
		}
	}
	for _, candidate := range children {
		metadata, ok := candidate.Table()
		if ok && metadata.PartitionBound != nil && metadata.PartitionBound.Default {
			result.fallback = candidate
			break
		}
	}
	if result.fallback != nil && result.fallback.ID() != child.ID() {
		result.fallbackLeaves, err = partitionPhysicalLeaves(ctx, snapshot, result.fallback, maximum)
		if err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}

func (transactions *Transactions) populatePartitionAttachment(ctx context.Context, parent, child *model.Descriptor, createdIndexes []*model.Descriptor, invocation StatementExecution) (affected uint64, err error) {
	environment := &invocation.Execution.Environment
	if environment.Query == nil || invocation.Execution.ObjectDDL == nil || environment.Reader != nil {
		return 0, errPreparedInvocation
	}
	maximumDescriptors := invocation.Execution.ObjectDDL.MaxDescriptors
	catalogDependencies, err := transactions.attachmentCatalogDependencies(ctx, invocation.Execution.Dependencies, maximumDescriptors)
	if err != nil {
		return 0, err
	}
	discoveryView, discoverySnapshot, err := transactions.openAttachmentSnapshot(ctx, catalogDependencies)
	if err != nil {
		return 0, err
	}
	discovered, err := partitionAttachmentDescriptors(ctx, discoverySnapshot, parent, child, maximumDescriptors)
	plannedLeaves := append(slices.Clone(discovered.candidateLeaves), discovered.fallbackLeaves...)
	if err == nil {
		var dependencies []txn.ReadDependency
		dependencies, err = attachmentDataDependencies(ctx, transactions.sequences.config.Catalog, plannedLeaves, invocation.Execution.ObjectDDL.Binding.Descriptors, maximumDescriptors)
		catalogDependencies = append(catalogDependencies, dependencies...)
	}
	if err == nil {
		var indexDependencies []txn.ReadDependency
		indexDependencies, err = attachmentIndexDependencies(ctx, transactions.sequences.config.Catalog, plannedLeaves, maximumDescriptors)
		catalogDependencies = append(catalogDependencies, indexDependencies...)
	}
	err = errors.Join(err, discoverySnapshot.Close(), discoveryView.Close())
	if err != nil {
		return 0, err
	}
	view, snapshot, err := transactions.openAttachmentSnapshot(ctx, catalogDependencies)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	scan, err := partitionAttachmentDescriptors(ctx, snapshot, parent, child, maximumDescriptors)
	if err != nil {
		return 0, err
	}
	confirmedLeaves := append(slices.Clone(scan.candidateLeaves), scan.fallbackLeaves...)
	if !sameAttachmentDescriptors(discovered.children, scan.children) || !sameAttachmentDescriptors(plannedLeaves, confirmedLeaves) {
		return 0, catalog.ErrDescriptorChanged
	}
	router, err := partition.NewRouter(ctx, partition.Options{
		Parent: parent, Children: scan.children, Encoder: transactions.options.KeyEncoder,
		Builtin: transactions.settings.BuiltinOptions(environment.Builtin.Collation, environment.Builtin.DefaultCollation),
		Limits:  partition.Limits{MaxChildren: int(maximumDescriptors), MaxKeys: invocation.Execution.ObjectDDL.Binding.Descriptors.MaxItems, MaxBytes: invocation.Execution.ObjectDDL.Binding.Descriptors.MaxBytes},
	})
	if err != nil {
		return 0, err
	}
	compiler, err := NewIndexExpressionCompiler(snapshot, invocation.Execution.ObjectDDL.Indexes)
	if err != nil {
		return 0, err
	}
	columnCompiler, err := NewColumnExpressionCompiler(snapshot, invocation.Execution.ObjectDDL.Columns)
	if err != nil {
		return 0, err
	}
	expressions, err := compiler.CompilePartitionExpressions(ctx, parent)
	if err != nil {
		return 0, err
	}
	createdByTable := make(map[model.ObjectID][]*model.Descriptor)
	for _, index := range createdIndexes {
		if index == nil {
			return 0, model.ErrInvalidDescriptor
		}
		metadata, ok := index.Index()
		if !ok {
			return 0, model.ErrInvalidDescriptor
		}
		createdByTable[metadata.Table] = append(createdByTable[metadata.Table], index)
	}
	for _, leaf := range scan.candidateLeaves {
		leafCreated := createdByTable[leaf.ID()]
		normalizer, err := schema.NewTablePolicy(ctx, schema.TablePolicyOptions{
			Descriptor: leaf, Encoder: transactions.options.KeyEncoder, Expressions: columnCompiler,
			MetadataBytes: invocation.Execution.ObjectDDL.Limits.MetadataBytes,
			RowBytes:      invocation.Execution.ObjectDDL.Limits.EntryBytes,
			MaxColumns:    invocation.Execution.ObjectDDL.Limits.MaxColumns,
		})
		if err != nil {
			return 0, err
		}
		indexSet, err := partitionAttachmentIndexSet(ctx, snapshot, leaf, leafCreated, compiler, transactions.options.KeyEncoder, invocation.Execution.ObjectDDL.Limits)
		if err != nil {
			return 0, err
		}
		if err := admitCreatedIndexTargets(ctx, transactions.transaction, indexSet, leafCreated, invocation.Execution.ObjectDDL.PolicyBytes); err != nil {
			return 0, err
		}
		if _, err := schema.LoadDeclarationRelationalConstraints(ctx, snapshot, indexSet, transactions.options.KeyEncoder, compiler, invocation.Execution.ObjectDDL.Limits); err != nil {
			return 0, err
		}
		selected := make([]model.ObjectID, len(leafCreated))
		for i, index := range leafCreated {
			selected[i] = index.ID()
		}
		slices.Sort(selected)
		count, err := transactions.scanPartitionAttachment(ctx, view, router, expressions, columnCompiler, normalizer, indexSet, selected, true, parent, leaf, child.ID(), invocation)
		if err != nil {
			return 0, err
		}
		maximum := invocation.Execution.Environment.Modify.MaxRows
		if affected > maximum || count > maximum-affected {
			return 0, &model.Error{Code: "54000", Message: "partition attachment row limit exceeded"}
		}
		affected += count
	}
	validated := affected
	for _, leaf := range scan.fallbackLeaves {
		fallbackNormalizer, err := schema.NewTablePolicy(ctx, schema.TablePolicyOptions{
			Descriptor: leaf, Encoder: transactions.options.KeyEncoder, Expressions: columnCompiler,
			MetadataBytes: invocation.Execution.ObjectDDL.Limits.MetadataBytes,
			RowBytes:      invocation.Execution.ObjectDDL.Limits.EntryBytes,
			MaxColumns:    invocation.Execution.ObjectDDL.Limits.MaxColumns,
		})
		if err != nil {
			return 0, err
		}
		count, err := transactions.scanPartitionAttachment(ctx, view, router, expressions, columnCompiler, fallbackNormalizer, nil, nil, false, parent, leaf, scan.fallback.ID(), invocation)
		if err != nil {
			return 0, err
		}
		maximum := invocation.Execution.Environment.Modify.MaxRows
		if validated > maximum || count > maximum-validated {
			return 0, &model.Error{Code: "54000", Message: "partition attachment row limit exceeded"}
		}
		validated += count
	}
	return affected, nil
}
func (transactions *Transactions) populateInheritanceAttachment(ctx context.Context, child *model.Descriptor, invocation StatementExecution) (affected uint64, err error) {
	environment := &invocation.Execution.Environment
	if child == nil || environment.Query == nil || invocation.Execution.ObjectDDL == nil || environment.Reader != nil {
		return 0, errPreparedInvocation
	}
	maximumDescriptors := invocation.Execution.ObjectDDL.MaxDescriptors
	catalogDependencies, err := transactions.attachmentCatalogDependencies(ctx, invocation.Execution.Dependencies, maximumDescriptors)
	if err != nil {
		return 0, err
	}
	discoveryView, discoverySnapshot, err := transactions.openAttachmentSnapshot(ctx, catalogDependencies)
	if err != nil {
		return 0, err
	}
	discovered, err := inheritancePhysicalMembers(ctx, discoverySnapshot, child, maximumDescriptors)
	if err == nil {
		var dependencies []txn.ReadDependency
		dependencies, err = attachmentDataDependencies(ctx, transactions.sequences.config.Catalog, discovered, invocation.Execution.ObjectDDL.Binding.Descriptors, maximumDescriptors)
		catalogDependencies = append(catalogDependencies, dependencies...)
	}
	err = errors.Join(err, discoverySnapshot.Close(), discoveryView.Close())
	if err != nil {
		return 0, err
	}
	view, snapshot, err := transactions.openAttachmentSnapshot(ctx, catalogDependencies)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	members, err := inheritancePhysicalMembers(ctx, snapshot, child, maximumDescriptors)
	if err != nil {
		return 0, err
	}
	if !sameAttachmentDescriptors(discovered, members) {
		return 0, catalog.ErrDescriptorChanged
	}
	compiler, err := NewColumnExpressionCompiler(snapshot, invocation.Execution.ObjectDDL.Columns)
	if err != nil {
		return 0, err
	}
	for _, member := range members {
		normalizer, err := schema.NewTablePolicy(ctx, schema.TablePolicyOptions{
			Descriptor: member, Encoder: transactions.options.KeyEncoder, Expressions: compiler,
			MetadataBytes: invocation.Execution.ObjectDDL.Limits.MetadataBytes,
			RowBytes:      invocation.Execution.ObjectDDL.Limits.EntryBytes,
			MaxColumns:    invocation.Execution.ObjectDDL.Limits.MaxColumns,
		})
		if err != nil {
			return 0, err
		}
		count, err := transactions.scanPartitionAttachment(ctx, view, nil, nil, compiler, normalizer, nil, nil, false, member, member, 0, invocation)
		if err != nil {
			return 0, err
		}
		maximum := invocation.Execution.Environment.Modify.MaxRows
		if affected > maximum || count > maximum-affected {
			return 0, &model.Error{Code: "54000", Message: "inheritance attachment row limit exceeded"}
		}
		affected += count
	}
	return affected, nil
}

func directPartitionChildren(ctx context.Context, snapshot *catalog.CatalogSnapshot, parent *model.Descriptor, maximum uint32) ([]*model.Descriptor, error) {
	var schemas []*model.Descriptor
	var scanned uint32
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		scanned++
		if scanned > maximum {
			return model.ErrDescriptorLimit
		}
		if published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			schemas = append(schemas, published.Descriptor)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var children []*model.Descriptor
	for _, namespace := range schemas {
		if err := snapshot.ScanNamespace(ctx, namespace.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			scanned++
			if scanned > maximum {
				return model.ErrDescriptorLimit
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil
			}
			table, ok := descriptor.Table()
			if !ok || table.PartitionBound == nil || table.PartitionState != model.PartitionAttached || !slices.Contains(table.Parents, parent.ID()) {
				return nil
			}
			children = append(children, descriptor)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(children, func(left, right *model.Descriptor) int {
		if left.ID() < right.ID() {
			return -1
		}
		if left.ID() > right.ID() {
			return 1
		}
		return 0
	})
	return children, nil
}

func inheritancePhysicalMembers(ctx context.Context, snapshot *catalog.CatalogSnapshot, root *model.Descriptor, maximum uint32) ([]*model.Descriptor, error) {
	if snapshot == nil || root == nil || maximum == 0 {
		return nil, model.ErrDescriptorLimit
	}
	var schemas []*model.Descriptor
	var scanned uint32
	visit := func() error {
		scanned++
		if scanned > maximum {
			return model.ErrDescriptorLimit
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
	children := make(map[model.ObjectID][]*model.Descriptor)
	for _, namespace := range schemas {
		if err := snapshot.ScanNamespace(ctx, namespace.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if err := visit(); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil
			}
			table, ok := descriptor.Table()
			if !ok || table.PartitionBound != nil {
				return nil
			}
			for _, parent := range table.Parents {
				children[parent] = append(children[parent], descriptor)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *model.Descriptor) int {
			if left.ID() < right.ID() {
				return -1
			}
			if left.ID() > right.ID() {
				return 1
			}
			return 0
		})
	}
	var members []*model.Descriptor
	visiting := make(map[model.ObjectID]bool)
	visited := make(map[model.ObjectID]bool)
	var descend func(*model.Descriptor) error
	descend = func(descriptor *model.Descriptor) error {
		if visiting[descriptor.ID()] {
			return model.ErrInvalidDescriptor
		}
		if visited[descriptor.ID()] {
			return nil
		}
		visiting[descriptor.ID()] = true
		members = append(members, descriptor)
		visited[descriptor.ID()] = true
		if uint32(len(visited)) > maximum {
			return model.ErrDescriptorLimit
		}
		for _, child := range children[descriptor.ID()] {
			if err := descend(child); err != nil {
				return err
			}
		}
		delete(visiting, descriptor.ID())
		return ctx.Err()
	}
	if err := descend(root); err != nil {
		return nil, err
	}
	return members, ctx.Err()
}

func partitionPhysicalLeaves(ctx context.Context, snapshot *catalog.CatalogSnapshot, root *model.Descriptor, maximum uint32) ([]*model.Descriptor, error) {
	if snapshot == nil || root == nil || maximum == 0 {
		return nil, model.ErrDescriptorLimit
	}
	var schemas []*model.Descriptor
	var scanned uint32
	visit := func() error {
		scanned++
		if scanned > maximum {
			return model.ErrDescriptorLimit
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
	children := make(map[model.ObjectID][]*model.Descriptor)
	for _, namespace := range schemas {
		if err := snapshot.ScanNamespace(ctx, namespace.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if err := visit(); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil
			}
			table, ok := descriptor.Table()
			if !ok || table.PartitionBound == nil || table.PartitionState != model.PartitionAttached || len(table.Parents) != 1 {
				return nil
			}
			children[table.Parents[0]] = append(children[table.Parents[0]], descriptor)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *model.Descriptor) int {
			if left.ID() < right.ID() {
				return -1
			}
			if left.ID() > right.ID() {
				return 1
			}
			return 0
		})
	}
	var leaves []*model.Descriptor
	visiting := make(map[model.ObjectID]bool)
	visited := make(map[model.ObjectID]bool)
	var descend func(*model.Descriptor) error
	descend = func(descriptor *model.Descriptor) error {
		if visiting[descriptor.ID()] || visited[descriptor.ID()] {
			return model.ErrInvalidDescriptor
		}
		visiting[descriptor.ID()] = true
		table, ok := descriptor.Table()
		if !ok {
			return model.ErrInvalidDescriptor
		}
		if table.PartitionStrategy == 0 {
			leaves = append(leaves, descriptor)
		} else {
			for _, child := range children[descriptor.ID()] {
				if err := descend(child); err != nil {
					return err
				}
			}
		}
		delete(visiting, descriptor.ID())
		visited[descriptor.ID()] = true
		if uint32(len(visited)) > maximum {
			return model.ErrDescriptorLimit
		}
		return ctx.Err()
	}
	if err := descend(root); err != nil {
		return nil, err
	}
	return leaves, ctx.Err()
}

func partitionAttachmentIndexSet(ctx context.Context, snapshot *catalog.CatalogSnapshot, table *model.Descriptor, created []*model.Descriptor, compiler *IndexExpressionCompiler, encoder storage.KeyEncoder, limits schema.IndexLimits) (*schema.IndexSet, error) {
	metadata, ok := table.Table()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	byID := make(map[model.ObjectID]*model.Descriptor, len(created))
	for _, descriptor := range created {
		byID[descriptor.ID()] = descriptor
	}
	plans := make([]*schema.IndexPlan, 0, len(metadata.Indexes))
	for _, id := range metadata.Indexes {
		descriptor := byID[id]
		if descriptor == nil {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil {
				return nil, err
			}
			descriptor = published.Descriptor
		}
		plan, err := schema.NewIndexPlan(ctx, table, descriptor, encoder, compiler, limits)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return schema.NewIndexSet(ctx, table, plans, limits)
}

func admitCreatedIndexTargets(ctx context.Context, transaction *txn.Transaction, indexes *schema.IndexSet, created []*model.Descriptor, maximum uint64) error {
	if transaction == nil || indexes == nil || maximum == 0 {
		return errPreparedInvocation
	}
	wanted := make(map[model.ObjectID]bool, len(created))
	for _, descriptor := range created {
		if descriptor == nil || descriptor.Kind() != model.ObjectIndex {
			return model.ErrInvalidDescriptor
		}
		wanted[descriptor.ID()] = true
	}
	for _, layout := range indexes.Layouts() {
		if !wanted[layout.Target.ObjectID] {
			continue
		}
		policy, err := indexes.ResolveMutation(ctx, layout.Target)
		if err != nil {
			return err
		}
		if policy.RetainedBytes() > maximum {
			return model.ErrValueBudget
		}
		if err := transaction.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes()); err != nil {
			return err
		}
		delete(wanted, layout.Target.ObjectID)
	}
	if len(wanted) != 0 {
		return model.ErrInvalidDescriptor
	}
	return ctx.Err()
}
func (transactions *Transactions) populateHierarchyIndexes(ctx context.Context, command *bind.BoundObjectCommand, invocation StatementExecution) (affected uint64, err error) {
	environment := &invocation.Execution.Environment

	if command == nil || environment.Query == nil || invocation.Execution.ObjectDDL == nil || environment.Reader != nil {
		return 0, errPreparedInvocation
	}
	tables := make(map[model.ObjectID]*model.Descriptor)
	for _, update := range command.Updates() {
		if update.Next != nil && update.Next.Kind() == model.ObjectTable {
			tables[update.Next.ID()] = update.Next
		}
	}
	created := make(map[model.ObjectID][]*model.Descriptor)
	for _, descriptor := range command.Creates() {
		metadata, ok := descriptor.Index()
		table := tables[metadata.Table]
		if !ok || metadata.ParentIndex == 0 || table == nil {
			continue
		}
		tableMetadata, ok := table.Table()
		if !ok {
			return 0, model.ErrInvalidDescriptor
		}
		if tableMetadata.PartitionStrategy == 0 {
			created[table.ID()] = append(created[table.ID()], descriptor)
		}
	}
	if len(created) == 0 {
		return 0, nil
	}
	view, err := transactions.transaction.ReadView(ctx, invocation.Execution.Dependencies)
	if err != nil {
		return 0, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return 0, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
		return 0, errors.Join(err, snapshot.Close(), view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	indexCompiler, err := NewIndexExpressionCompiler(snapshot, invocation.Execution.ObjectDDL.Indexes)
	if err != nil {
		return 0, err
	}
	columnCompiler, err := NewColumnExpressionCompiler(snapshot, invocation.Execution.ObjectDDL.Columns)
	if err != nil {
		return 0, err
	}
	ids := make([]model.ObjectID, 0, len(created))
	for id := range created {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		table := tables[id]
		normalizer, err := schema.NewTablePolicy(ctx, schema.TablePolicyOptions{
			Descriptor: table, Encoder: transactions.options.KeyEncoder, Expressions: columnCompiler,
			MetadataBytes: invocation.Execution.ObjectDDL.Limits.MetadataBytes,
			RowBytes:      invocation.Execution.ObjectDDL.Limits.EntryBytes,
			MaxColumns:    invocation.Execution.ObjectDDL.Limits.MaxColumns,
		})
		if err != nil {
			return 0, err
		}
		indexSet, err := partitionAttachmentIndexSet(ctx, snapshot, table, created[id], indexCompiler, transactions.options.KeyEncoder, invocation.Execution.ObjectDDL.Limits)
		if err != nil {
			return 0, err
		}
		selected := make([]model.ObjectID, len(created[id]))
		for i, index := range created[id] {
			selected[i] = index.ID()
		}
		if err := admitCreatedIndexTargets(ctx, transactions.transaction, indexSet, created[id], invocation.Execution.ObjectDDL.PolicyBytes); err != nil {
			return 0, err
		}
		slices.Sort(selected)
		count, err := transactions.scanPartitionAttachment(ctx, view, nil, nil, columnCompiler, normalizer, indexSet, selected, false, table, table, 0, invocation)
		if err != nil {
			return 0, err
		}
		maximum := invocation.Execution.Environment.Modify.MaxRows
		if affected > maximum || count > maximum-affected {
			return 0, &model.Error{Code: "54000", Message: "partition index population row limit exceeded"}
		}
		affected += count
	}
	return affected, ctx.Err()
}

func (transactions *Transactions) scanPartitionAttachment(ctx context.Context, reader storage.SnapshotReader, router *partition.Router, expressions []txn.ValueExpression, compiler *ColumnExpressionCompiler, normalizer *txn.RowNormalizer, indexes *schema.IndexSet, selectedIndexes []model.ObjectID, relational bool, parent, child *model.Descriptor, expectedTarget model.ObjectID, invocation StatementExecution) (affected uint64, err error) {
	parentTable, parentOK := parent.Table()
	childTable, childOK := child.Table()
	options := invocation.Execution.Environment.Modify
	if !parentOK || !childOK || reader == nil || compiler == nil || normalizer == nil || options.Query == nil || options.BatchRows <= 0 || options.MaxRows == 0 || options.MaxRecordBytes == 0 ||
		(router != nil) != (expectedTarget != 0) {
		return 0, errPreparedInvocation
	}
	childColumns := slices.DeleteFunc(slices.Clone(childTable.Columns), func(column model.ColumnDescriptor) bool {
		return column.State != model.StatePublic && column.State != model.StateWriteOnly
	})
	slices.SortFunc(childColumns, func(left, right model.ColumnDescriptor) int {
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	allLayouts := make([]storage.ColumnLayout, len(childColumns))
	for i, column := range childColumns {
		allLayouts[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
	}
	virtual := make([]txn.ValueExpression, len(childColumns))
	for i, column := range childColumns {
		if column.Generated == nil || column.GeneratedStored {
			continue
		}
		virtual[i], err = compiler.CompileStoredGenerated(ctx, column.Generated, column.ID, allLayouts, column.Layout)
		if err != nil {
			return 0, err
		}
	}
	layout := normalizer.Layout().Columns
	slices.SortFunc(layout, func(left, right storage.ColumnLayout) int {
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	ids := make([]model.ColumnID, len(layout))
	positions := make(map[model.ColumnID]int, len(layout))
	for i, column := range layout {
		ids[i], positions[column.ID] = column.ID, i
	}
	logicalPositions := make(map[model.ColumnID]int, len(childColumns))
	for i, column := range childColumns {
		logicalPositions[column.ID] = i
	}
	physicalToLogical := make([]int, len(layout))
	for i, column := range layout {
		position, ok := logicalPositions[column.ID]
		if !ok {
			return 0, model.ErrInvalidDescriptor
		}
		physicalToLogical[i] = position
	}
	keyLayout := make([]storage.KeyColumn, len(childTable.PrimaryKey))
	keyPositions := make([]int, len(keyLayout))
	for i, key := range childTable.PrimaryKey {
		position, ok := positions[key.Column]
		if !ok {
			return 0, model.ErrInvalidDescriptor
		}
		keyPositions[i] = position
		keyLayout[i] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: key.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	if len(keyLayout) == 0 {
		return 0, model.ErrInvalidDescriptor
	}
	request := storage.PushdownRequest{
		Target: storage.ObjectRef{ObjectID: child.ID(), DescriptorVersion: child.Version(), Generation: child.Physical().Generation, EncodingVersion: child.Physical().EncodingVersion},
		Layout: layout, KeyLayout: keyLayout, ColumnIDs: ids,
		Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}
	expected, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request}, invocation.Execution.ObjectDDL.Binding.Descriptors.MaxBytes)
	if err != nil {
		return 0, err
	}
	plan, err := reader.PlanScan(ctx, request)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, reader.ReleasePlan(plan)) }()
	actual, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: plan.Request}, invocation.Execution.ObjectDDL.Binding.Descriptors.MaxBytes)
	if err != nil || actual != expected || !plan.Accepted.Empty() || !plan.Residual.Empty() || plan.AppliedLimit.Enabled {
		return 0, errors.Join(err, storage.ErrInvalidScanPlan)
	}
	cursor, err := reader.VectorizedScan(ctx, plan)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	var parentPositions []int
	if router != nil {
		parentPositions = make([]int, len(parentTable.Columns))
		for i := range parentPositions {
			parentPositions[i] = -1
		}
		for i, column := range parentTable.Columns {
			if column.State != model.StatePublic && column.State != model.StateWriteOnly {
				continue
			}
			childIndex := slices.IndexFunc(childColumns, func(candidate model.ColumnDescriptor) bool {
				if candidate.SystemIdentity || column.SystemIdentity {
					return candidate.SystemIdentity && column.SystemIdentity
				}
				return candidate.Name == column.Name
			})
			if childIndex < 0 {
				return 0, model.ErrInvalidDescriptor
			}
			parentPositions[i] = childIndex
		}
	}
	keyValues := make([]model.Value, len(keyLayout))
	logical := make([]model.Value, len(parentTable.Columns))
	partitionKeys := make([]model.Value, len(parentTable.PartitionKeys))
	values := make([]model.Value, len(layout))
	childLogical := make([]model.Value, len(childColumns))
	expressionRuntime := model.ExpressionRuntime{Sequences: options.Sequences, Relations: options.Relations, Temporal: options.Temporal, Settings: options.Settings, TextSearch: options.TextSearch, Advisory: options.Advisory, Native: options.Native}
	batch := columnar.Batch{MaxRows: options.BatchRows}
	defer batch.Reset()
	defer clear(values)
	defer clear(logical)
	defer clear(childLogical)
	for {
		batch.Reset()
		if err := cursor.Next(ctx, &batch); err != nil {
			if err == io.EOF {
				return affected, nil
			}
			return 0, err
		}
		if err := batch.Validate(max(batch.MaxRows, batch.Length)); err != nil || len(batch.Columns) != len(layout) {
			return 0, errors.Join(err, storage.ErrInvalidScanPlan)
		}
		for ordinal := range batch.Rows() {
			if affected == options.MaxRows {
				return 0, &model.Error{Code: "54000", Message: "inheritance attachment row limit exceeded"}
			}
			clear(values)
			remaining := options.MaxRecordBytes * 8
			for i := range values {
				value, used, copyErr := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(ordinal), remaining)
				if copyErr != nil {
					return 0, copyErr
				}
				values[i], remaining = value, remaining-used
			}
			for i, column := range childColumns {
				childLogical[i] = model.NullVal(column.Layout.Type.Storage)
			}
			for i, position := range physicalToLogical {
				childLogical[position] = values[i]
			}
			for i, evaluator := range virtual {
				if evaluator == nil {
					continue
				}
				childLogical[i], err = evaluator.Evaluate(ctx, childLogical, expressionRuntime)
				if err != nil {
					return 0, err
				}
			}
			row := model.Row{Values: childLogical}
			if err := normalizer.ValidateRow(ctx, &row); err != nil {
				return 0, err
			}
			for i, position := range parentPositions {
				if position < 0 {
					logical[i] = model.NullVal(parentTable.Columns[i].Layout.Type.Storage)
				} else {
					logical[i] = childLogical[position]
				}
			}
			if router != nil {
				expression := 0
				for i, key := range parentTable.PartitionKeys {
					if key.Column != 0 {
						position := slices.IndexFunc(parentTable.Columns, func(column model.ColumnDescriptor) bool { return column.ID == key.Column })
						if position < 0 {
							return 0, model.ErrInvalidDescriptor
						}
						partitionKeys[i] = logical[position]
						continue
					}
					if expression >= len(expressions) {
						return 0, model.ErrInvalidDescriptor
					}
					partitionKeys[i], err = expressions[expression].Evaluate(ctx, logical, expressionRuntime)
					if err != nil {
						return 0, err
					}
					expression++
				}
				target, routeErr := router.Route(ctx, partitionKeys)
				if routeErr != nil || target.Ref.ObjectID != expectedTarget {
					return 0, &model.Error{Code: "23514", Message: "row violates partition constraint", Table: child.Name().Name, Cause: routeErr}
				}
			}
			for i, position := range keyPositions {
				keyValues[i] = values[position]
			}
			key, err := transactions.options.KeyEncoder.EncodePrefix(ctx, keyLayout, keyValues, options.MaxRecordBytes)
			if err != nil {
				return 0, err
			}
			row = model.Row{Key: key, Values: childLogical}
			if indexes != nil {
				if selectedIndexes == nil {
					err = indexes.Expand(ctx, transactions.transaction, nil, &row)
				} else if len(selectedIndexes) != 0 {
					err = indexes.ExpandSelected(ctx, transactions.transaction, &row, selectedIndexes)
				}
				if err != nil {
					return 0, err
				}
				if relational {
					if err := indexes.ExpandRelational(ctx, transactions.transaction, options.Deferral, options.Triggers, expressionRuntime, options.Query.Allocator(runtime.WorkMemory), options.MaxRecordBytes*8, nil, &row); err != nil {
						return 0, err
					}
				}
			}
			affected++
		}
	}
}
