package modify

import (
	"context"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	logicalpartition "github.com/gosuda/supersql-alpha/internal/query/partition"
)

// PartitionChild supplies one physical descendant and its nested hierarchy.
type PartitionChild struct {
	Descriptor  *model.Descriptor
	Expansion   Expansion
	Children    []PartitionChild
	Expressions []txn.ValueExpression
}

// PartitionPlanOptions supplies an admitted declarative partition hierarchy.
type PartitionPlanOptions struct {
	Root *model.Descriptor
	// Input is the directly named SQL target. Nil selects Root.
	Input       *model.Descriptor
	Children    []PartitionChild
	Expressions []txn.ValueExpression
	Encoder     storage.KeyEncoder
	Builtin     builtin.ResolveOptions
	Limits      logicalpartition.Limits
}

type partitionChild struct {
	descriptor     *model.Descriptor
	parentObject   model.ObjectID
	expansion      Expansion
	target         storage.ObjectRef
	columns        []model.ColumnDescriptor
	columnIDs      []model.ColumnID
	layout         []storage.ColumnLayout
	rootToChild    []model.ColumnID
	childToRoot    []int
	visibleColumns []int
	keyLayout      []storage.KeyColumn
	router         *logicalpartition.Router
	keyColumns     []int
	expressions    []txn.ValueExpression
	keyValues      []model.Value
}

// PartitionPlan owns immutable hierarchy routing and row projection metadata.
type PartitionPlan struct {
	root         *model.Descriptor
	inputTarget  storage.ObjectRef
	router       *logicalpartition.Router
	children     []partitionChild
	byObject     map[model.ObjectID]int
	rootColumns  []model.ColumnDescriptor
	inputColumns []model.ColumnDescriptor
	keyColumns   []int
	expressions  []txn.ValueExpression
	keyValues    []model.Value
	sourceImage  bool
}

// NewPartitionPlan validates and compiles a declarative partition hierarchy.
func NewPartitionPlan(ctx context.Context, options PartitionPlanOptions) (*PartitionPlan, error) {
	if options.Root == nil || options.Encoder == nil {
		return nil, storage.ErrInvalidLayout
	}
	root, ok := options.Root.Table()
	if !ok || root.PartitionStrategy == 0 || len(root.PartitionKeys) == 0 {
		return nil, storage.ErrInvalidLayout
	}
	input := options.Input
	if input == nil {
		input = options.Root
	}
	inputTable, ok := input.Table()
	if !ok || len(inputTable.Columns) != len(root.Columns) {
		return nil, storage.ErrInvalidLayout
	}
	for i := range root.Columns {
		left, right := root.Columns[i], inputTable.Columns[i]
		if left.SystemIdentity != right.SystemIdentity || !model.TypeLayoutsEqual(left.Layout, right.Layout) ||
			!left.SystemIdentity && left.Name != right.Name {
			return nil, storage.ErrInvalidLayout
		}
	}
	plan := &PartitionPlan{root: options.Root, rootColumns: slices.Clone(root.Columns), inputColumns: slices.Clone(inputTable.Columns), byObject: make(map[model.ObjectID]int)}
	inputPhysical := input.Physical()
	plan.inputTarget = storage.ObjectRef{ObjectID: input.ID(), DescriptorVersion: input.Version(), Generation: inputPhysical.Generation, EncodingVersion: inputPhysical.EncodingVersion}
	if err := plan.inputTarget.Validate(); err != nil {
		return nil, err
	}
	keyColumns, expressions, err := plan.routingKeys(root, options.Expressions)
	if err != nil {
		return nil, err
	}
	plan.keyColumns, plan.expressions = keyColumns, expressions
	plan.keyValues = make([]model.Value, len(keyColumns))
	plan.router, err = plan.addLevel(ctx, options.Root, options.Children, options)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// NewInheritancePlan compiles source-relation selection for inherited writes.
func NewInheritancePlan(ctx context.Context, root PartitionChild) (*PartitionPlan, error) {
	if root.Descriptor == nil || len(root.Children) == 0 {
		return nil, storage.ErrInvalidLayout
	}
	table, ok := root.Descriptor.Table()
	if !ok || table.PartitionStrategy != 0 {
		return nil, storage.ErrInvalidLayout
	}
	plan := &PartitionPlan{root: root.Descriptor, rootColumns: slices.Clone(table.Columns), inputColumns: slices.Clone(table.Columns), byObject: make(map[model.ObjectID]int)}
	var add func(PartitionChild) error
	add = func(spec PartitionChild) error {
		if spec.Descriptor == nil {
			return storage.ErrInvalidLayout
		}
		metadata, ok := spec.Descriptor.Table()
		if !ok || len(metadata.PrimaryKey) != len(table.PrimaryKey) {
			return storage.ErrInvalidLayout
		}
		child := partitionChild{descriptor: spec.Descriptor, expansion: spec.Expansion, columns: slices.Clone(metadata.Columns)}
		physical := spec.Descriptor.Physical()
		child.target = storage.ObjectRef{ObjectID: spec.Descriptor.ID(), DescriptorVersion: spec.Descriptor.Version(), Generation: physical.Generation, EncodingVersion: physical.EncodingVersion}
		if err := child.target.Validate(); err != nil {
			return err
		}
		child.rootToChild = make([]model.ColumnID, len(plan.rootColumns))
		child.childToRoot = make([]int, len(metadata.Columns))
		for i := range child.childToRoot {
			child.childToRoot[i] = -1
		}
		child.columnIDs = make([]model.ColumnID, len(metadata.Columns))
		child.layout = make([]storage.ColumnLayout, len(metadata.Columns))
		for i, column := range metadata.Columns {
			rootIndex := slices.IndexFunc(plan.rootColumns, func(rootColumn model.ColumnDescriptor) bool {
				if rootColumn.SystemIdentity || column.SystemIdentity {
					return rootColumn.SystemIdentity && column.SystemIdentity && model.TypeLayoutsEqual(rootColumn.Layout, column.Layout)
				}
				return rootColumn.Name == column.Name && model.TypeLayoutsEqual(rootColumn.Layout, column.Layout)
			})
			if rootIndex >= 0 {
				child.rootToChild[rootIndex], child.childToRoot[i] = column.ID, rootIndex
				if rootIndex != i {
					plan.sourceImage = true
				}
			} else {
				plan.sourceImage = true
			}
			child.columnIDs[i] = column.ID
			child.layout[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		}
		for _, id := range child.rootToChild {
			if id == 0 {
				return storage.ErrInvalidLayout
			}
		}
		for rootIndex, rootColumn := range plan.rootColumns {
			if rootColumn.SystemIdentity {
				continue
			}
			childColumn := slices.IndexFunc(child.childToRoot, func(candidate int) bool { return candidate == rootIndex })
			if childColumn < 0 {
				return storage.ErrInvalidLayout
			}
			child.visibleColumns = append(child.visibleColumns, childColumn)
		}
		child.keyLayout = make([]storage.KeyColumn, len(metadata.PrimaryKey))
		for i, key := range metadata.PrimaryKey {
			rootKey := table.PrimaryKey[i]
			column := slices.IndexFunc(metadata.Columns, func(candidate model.ColumnDescriptor) bool { return candidate.ID == key.Column })
			if column < 0 || child.childToRoot[column] < 0 || plan.rootColumns[child.childToRoot[column]].ID != rootKey.Column || key.Type != rootKey.Type || key.Descending != rootKey.Descending || key.NullsFirst != rootKey.NullsFirst {
				return storage.ErrInvalidLayout
			}
			child.keyLayout[i] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: key.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		}
		if spec.Expansion != nil {
			if err := spec.Expansion.ValidateDescriptor(ctx, spec.Descriptor); err != nil {
				return err
			}
		}
		if _, exists := plan.byObject[spec.Descriptor.ID()]; exists {
			return nil
		}
		plan.children = append(plan.children, child)
		plan.byObject[spec.Descriptor.ID()] = len(plan.children)
		for _, nested := range spec.Children {
			if err := add(nested); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(root); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *PartitionPlan) routingKeys(table model.TableMetadata, expressions []txn.ValueExpression) ([]int, []txn.ValueExpression, error) {
	result := make([]int, 0, len(table.PartitionKeys))
	expression := 0
	for _, key := range table.PartitionKeys {
		if key.Column == 0 {
			if expression >= len(expressions) || expressions[expression] == nil {
				return nil, nil, ErrUnsupported
			}
			result = append(result, -1-expression)
			expression++
			continue
		}
		parentColumn := slices.IndexFunc(table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == key.Column })
		if parentColumn < 0 {
			return nil, nil, storage.ErrInvalidLayout
		}
		column := table.Columns[parentColumn]
		rootColumn := slices.IndexFunc(p.rootColumns, func(candidate model.ColumnDescriptor) bool {
			if candidate.SystemIdentity || column.SystemIdentity {
				return candidate.SystemIdentity && column.SystemIdentity && model.TypeLayoutsEqual(candidate.Layout, column.Layout)
			}
			return candidate.Name == column.Name && model.TypeLayoutsEqual(candidate.Layout, column.Layout)
		})
		if rootColumn < 0 {
			return nil, nil, storage.ErrInvalidLayout
		}
		result = append(result, rootColumn)
	}
	if expression != len(expressions) {
		return nil, nil, storage.ErrInvalidLayout
	}
	return result, slices.Clone(expressions), nil
}

func (p *PartitionPlan) addLevel(ctx context.Context, parent *model.Descriptor, supplied []PartitionChild, options PartitionPlanOptions) (*logicalpartition.Router, error) {
	descriptors := make([]*model.Descriptor, len(supplied))
	indices := make([]int, len(supplied))
	for i, spec := range supplied {
		if spec.Descriptor == nil {
			return nil, storage.ErrInvalidLayout
		}
		if _, exists := p.byObject[spec.Descriptor.ID()]; exists {
			return nil, storage.ErrInvalidLayout
		}
		table, ok := spec.Descriptor.Table()
		if !ok || table.PartitionBound == nil || table.PartitionState != model.PartitionAttached {
			return nil, storage.ErrInvalidLayout
		}
		child := partitionChild{descriptor: spec.Descriptor, expansion: spec.Expansion, parentObject: parent.ID(), columns: slices.Clone(table.Columns)}
		physical := spec.Descriptor.Physical()
		child.target = storage.ObjectRef{ObjectID: spec.Descriptor.ID(), DescriptorVersion: spec.Descriptor.Version(), Generation: physical.Generation, EncodingVersion: physical.EncodingVersion}
		if err := child.target.Validate(); err != nil {
			return nil, err
		}
		child.rootToChild = make([]model.ColumnID, len(p.rootColumns))
		child.childToRoot = make([]int, len(table.Columns))
		child.columnIDs = make([]model.ColumnID, len(table.Columns))
		child.layout = make([]storage.ColumnLayout, len(table.Columns))
		for j, column := range table.Columns {
			child.layout[j] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
			child.columnIDs[j] = column.ID
			rootIndex := slices.IndexFunc(p.rootColumns, func(candidate model.ColumnDescriptor) bool {
				if candidate.SystemIdentity || column.SystemIdentity {
					return candidate.SystemIdentity && column.SystemIdentity && model.TypeLayoutsEqual(candidate.Layout, column.Layout)
				}
				return candidate.Name == column.Name && model.TypeLayoutsEqual(candidate.Layout, column.Layout)
			})
			if rootIndex < 0 || rootIndex != j {
				return nil, storage.ErrInvalidLayout
			}
			child.childToRoot[j], child.rootToChild[rootIndex] = rootIndex, column.ID
		}
		for _, id := range child.rootToChild {
			if id == 0 {
				return nil, storage.ErrInvalidLayout
			}
		}
		for rootIndex, rootColumn := range p.rootColumns {
			if rootColumn.SystemIdentity {
				continue
			}
			childColumn := slices.IndexFunc(child.childToRoot, func(candidate int) bool { return candidate == rootIndex })
			if childColumn < 0 {
				return nil, storage.ErrInvalidLayout
			}
			child.visibleColumns = append(child.visibleColumns, childColumn)
		}
		child.keyLayout = make([]storage.KeyColumn, len(table.PrimaryKey))
		for j, key := range table.PrimaryKey {
			child.keyLayout[j] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: key.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		}
		if len(child.keyLayout) != len(rootPrimaryKey(p.root)) {
			return nil, storage.ErrInvalidLayout
		}
		for j, key := range child.keyLayout {
			column := slices.IndexFunc(child.columns, func(candidate model.ColumnDescriptor) bool { return candidate.ID == key.ColumnID })
			rootKey := rootPrimaryKey(p.root)[j]
			if column < 0 || child.childToRoot[column] < 0 || p.rootColumns[child.childToRoot[column]].ID != rootKey.Column || key.Type != rootKey.Type || key.Descending != rootKey.Descending || key.NullsFirst != rootKey.NullsFirst {
				return nil, storage.ErrInvalidLayout
			}
		}
		if len(child.keyLayout) == 0 {
			return nil, storage.ErrInvalidLayout
		}
		if spec.Expansion != nil {
			if err := spec.Expansion.ValidateDescriptor(ctx, spec.Descriptor); err != nil {
				return nil, err
			}
		}
		indices[i] = len(p.children)
		p.children = append(p.children, child)
		p.byObject[spec.Descriptor.ID()] = indices[i] + 1
		descriptors[i] = spec.Descriptor
	}
	router, err := logicalpartition.NewRouter(ctx, logicalpartition.Options{Parent: parent, Children: descriptors, Encoder: options.Encoder, Builtin: options.Builtin, Limits: options.Limits})
	if err != nil {
		return nil, err
	}
	for i, spec := range supplied {
		table, _ := spec.Descriptor.Table()
		partitioned := table.PartitionStrategy != 0
		if !partitioned && len(spec.Children) != 0 {
			return nil, storage.ErrInvalidLayout
		}
		if !partitioned {
			continue
		}
		keys, expressions, err := p.routingKeys(table, spec.Expressions)
		if err != nil {
			return nil, err
		}
		nested, err := p.addLevel(ctx, spec.Descriptor, spec.Children, options)
		if err != nil {
			return nil, err
		}
		child := &p.children[indices[i]]
		child.keyValues = make([]model.Value, len(keys))
		child.router, child.keyColumns, child.expressions = nested, keys, expressions
	}
	return router, nil
}

func (p *PartitionPlan) child(object model.ObjectID) (*partitionChild, error) {
	index := p.byObject[object] - 1
	if index < 0 || index >= len(p.children) {
		return nil, storage.ErrInvalidTarget
	}
	return &p.children[index], nil
}

func rootPrimaryKey(descriptor *model.Descriptor) []model.KeyElement {
	table, _ := descriptor.Table()
	return table.PrimaryKey
}

// OwnedBytes reports immutable routing metadata retained for execution.
func (p *PartitionPlan) OwnedBytes() uint64 {
	if p == nil {
		return 0
	}
	total := uint64(unsafe.Sizeof(*p))
	add := func(count, size uint64) {
		if size != 0 && count > (^uint64(0)-total)/size {
			total = ^uint64(0)
			return
		}
		total += count * size
	}
	add(uint64(cap(p.rootColumns)), uint64(unsafe.Sizeof(model.ColumnDescriptor{})))
	add(uint64(cap(p.keyColumns)), uint64(unsafe.Sizeof(int(0))))
	add(uint64(cap(p.expressions)), uint64(unsafe.Sizeof(txn.ValueExpression(nil))))
	add(uint64(cap(p.keyValues)), uint64(unsafe.Sizeof(model.Value{})))
	add(uint64(cap(p.inputColumns)), uint64(unsafe.Sizeof(model.ColumnDescriptor{})))
	add(uint64(len(p.byObject)), 64)
	add(uint64(cap(p.children)), uint64(unsafe.Sizeof(partitionChild{})))
	if p.router != nil {
		add(1, p.router.OwnedBytes())
	}
	for i := range p.children {
		child := &p.children[i]
		add(uint64(cap(child.columns)), uint64(unsafe.Sizeof(model.ColumnDescriptor{})))
		add(uint64(cap(child.columnIDs)+cap(child.rootToChild)), uint64(unsafe.Sizeof(model.ColumnID(0))))
		add(uint64(cap(child.layout)), uint64(unsafe.Sizeof(storage.ColumnLayout{})))
		add(uint64(cap(child.childToRoot)+cap(child.visibleColumns)+cap(child.keyColumns)), uint64(unsafe.Sizeof(int(0))))
		add(uint64(cap(child.keyLayout)), uint64(unsafe.Sizeof(storage.KeyColumn{})))
		add(uint64(cap(child.expressions)), uint64(unsafe.Sizeof(txn.ValueExpression(nil))))
		add(uint64(cap(child.keyValues)), uint64(unsafe.Sizeof(model.Value{})))
		if child.router != nil {
			add(1, child.router.OwnedBytes())
		}
	}
	return total
}

// WorkspaceBytes reports the expansion workspace needed across descendants.
func (p *PartitionPlan) WorkspaceBytes() uint64 {
	var total uint64
	for _, child := range p.children {
		if child.descriptor.ID() == p.root.ID() || child.expansion == nil {
			continue
		}
		bytes := child.expansion.WorkspaceBytes()
		if total > ^uint64(0)-bytes {
			return ^uint64(0)
		}
		total += bytes
	}
	return total
}

func (p *PartitionPlan) mapColumns(child *partitionChild, columns []model.ColumnID) ([]model.ColumnID, error) {
	mapped := make([]model.ColumnID, len(columns))
	for i, id := range columns {
		root := slices.IndexFunc(p.inputColumns, func(column model.ColumnDescriptor) bool { return column.ID == id })
		if root < 0 || root >= len(child.rootToChild) {
			return nil, storage.ErrInvalidLayout
		}
		mapped[i] = child.rootToChild[root]
	}
	return mapped, nil
}

// Route selects the leaf physical relation for a logical root row.
func (p *PartitionPlan) Route(ctx context.Context, row []model.Value, expressions model.ExpressionRuntime) (*partitionChild, error) {
	if len(row) != len(p.rootColumns) {
		return nil, storage.ErrInvalidLayout
	}
	return p.routeLevel(ctx, p.router, p.keyColumns, p.expressions, p.keyValues, row, expressions)
}

func (p *PartitionPlan) routeLevel(ctx context.Context, router *logicalpartition.Router, columns []int, evaluators []txn.ValueExpression, keys, row []model.Value, expressions model.ExpressionRuntime) (*partitionChild, error) {
	for i, column := range columns {
		if column >= 0 {
			keys[i] = row[column]
			continue
		}
		value, err := evaluators[-1-column].Evaluate(ctx, row, expressions)
		if err != nil {
			return nil, err
		}
		keys[i] = value
	}
	target, err := router.Route(ctx, keys)
	if err != nil {
		return nil, err
	}
	child, err := p.child(target.Ref.ObjectID)
	if err != nil || child.router == nil {
		return child, err
	}
	return p.routeLevel(ctx, child.router, child.keyColumns, child.expressions, child.keyValues, row, expressions)
}

func (p *PartitionPlan) source(object model.ObjectID) (*partitionChild, error) {
	return p.child(object)
}
func (p *PartitionPlan) contains(route *partitionChild, ancestor model.ObjectID) bool {
	if route == nil || ancestor == 0 {
		return false
	}
	current := route
	for range len(p.children) + 1 {
		if current.descriptor.ID() == ancestor {
			return true
		}
		if current.parentObject == p.root.ID() {
			return ancestor == p.root.ID()
		}
		parent, err := p.child(current.parentObject)
		if err != nil {
			return false
		}
		current = parent
	}
	return false
}

func (p *PartitionPlan) conflict(route *partitionChild, arbiters []model.ConflictArbiter) (ConflictLookup, []model.ConflictArbiter, error) {
	return p.conflictFrom(route, p.root.ID(), arbiters)
}

func (p *PartitionPlan) conflictFrom(route *partitionChild, ancestor model.ObjectID, arbiters []model.ConflictArbiter) (ConflictLookup, []model.ConflictArbiter, error) {
	if route == nil || ancestor == 0 || !p.contains(route, ancestor) {
		return nil, arbiters, storage.ErrInvalidTarget
	}
	var lineage []*partitionChild
	current := route
	for current.descriptor.ID() != ancestor {
		lineage = append(lineage, current)
		if current.parentObject == ancestor {
			break
		}
		parent, err := p.child(current.parentObject)
		if err != nil {
			return nil, nil, err
		}
		current = parent
	}
	mapped := slices.Clone(arbiters)
	needsMapping := slices.ContainsFunc(mapped, func(arbiter model.ConflictArbiter) bool { return !arbiter.Primary })
	for i := len(lineage) - 1; i >= 0; i-- {
		if !needsMapping {
			break
		}
		mapper, ok := lineage[i].expansion.(interface {
			MapParentArbiters([]model.ConflictArbiter) ([]model.ConflictArbiter, error)
		})
		if !ok {
			return nil, nil, ErrUnsupported
		}
		var err error
		mapped, err = mapper.MapParentArbiters(mapped)
		if err != nil {
			return nil, nil, err
		}
	}
	lookup, _ := route.expansion.(ConflictLookup)
	return lookup, mapped, nil
}

// Project maps a physical descendant row into root-column order.
func (p *PartitionPlan) Project(child *partitionChild, values []model.Value, destination []model.Value) ([]model.Value, error) {
	if child == nil || len(values) != len(child.childToRoot) || len(destination) != len(p.rootColumns) {
		return nil, storage.ErrInvalidLayout
	}
	for i, root := range child.childToRoot {
		if root < 0 {
			continue
		}
		if root >= len(destination) {
			return nil, storage.ErrInvalidLayout
		}
		destination[root] = values[i]
	}
	return destination, nil
}

// PrepareInsert normalizes against the named SQL target before routing the
// completed row through the top-level partition hierarchy.
func (p *PartitionPlan) PrepareInsert(ctx context.Context, transaction Transaction, columnIDs []model.ColumnID, values []model.Value, allocator columnar.Allocator, maximum uint64, expressions model.ExpressionRuntime) (*txn.InsertCandidate, *partitionChild, model.Row, error) {
	if transaction == nil {
		return nil, nil, model.Row{}, storage.ErrInvalidMutation
	}
	normalized, err := transaction.PrepareInsert(ctx, txn.InsertPreparation{Target: p.inputTarget, ColumnIDs: columnIDs, Values: values, Allocator: allocator, MaxBytes: maximum, Expressions: expressions})
	if err != nil {
		return nil, nil, model.Row{}, err
	}
	logical, _, err := normalized.CopyRow(ctx, maximum)
	if err != nil {
		_ = normalized.Close()
		return nil, nil, model.Row{}, err
	}
	selected, err := p.Route(ctx, logical.Values, expressions)
	if err != nil {
		clear(logical.Values)
		_ = normalized.Close()
		return nil, nil, model.Row{}, err
	}
	if err := normalized.Close(); err != nil {
		clear(logical.Values)
		return nil, nil, model.Row{}, err
	}
	candidate, err := transaction.PrepareInsert(ctx, txn.InsertPreparation{Target: selected.target, ColumnIDs: selected.rootToChild, Values: logical.Values, Allocator: allocator, MaxBytes: maximum, Expressions: expressions})
	if err != nil {
		clear(logical.Values)
		return nil, nil, model.Row{}, err
	}
	physical, _, err := candidate.CopyRow(ctx, maximum)
	if err != nil {
		clear(logical.Values)
		_ = candidate.Close()
		return nil, nil, model.Row{}, err
	}
	if _, err := p.Project(selected, physical.Values, logical.Values); err != nil {
		clear(physical.Values)
		clear(logical.Values)
		_ = candidate.Close()
		return nil, nil, model.Row{}, err
	}
	routed, routeErr := p.Route(ctx, logical.Values, expressions)
	if routeErr != nil || routed != selected {
		clear(physical.Values)
		clear(logical.Values)
		_ = candidate.Close()
		if routeErr != nil {
			return nil, nil, model.Row{}, routeErr
		}
		return nil, nil, model.Row{}, logicalpartition.ErrNoPartition
	}
	logical.Key, physical.Key = physical.Key, nil
	clear(physical.Values)
	return candidate, selected, logical, nil
}

func partitionExpressions(options Options) model.ExpressionRuntime {
	return model.ExpressionRuntime{Sequences: options.Sequences, Relations: options.Relations, Temporal: options.Temporal, Settings: options.Settings, TextSearch: options.TextSearch, Advisory: options.Advisory, Native: options.Native}
}
