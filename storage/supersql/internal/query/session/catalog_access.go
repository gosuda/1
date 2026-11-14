package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	logicalpartition "github.com/gosuda/supersql-alpha/internal/query/partition"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// Limits are positive; request limits may only narrow them. CapabilityEpoch is
// supplied by the owner of the reader's capability configuration, not a fence.
type CatalogAccessOptions struct {
	MaxPaths         int
	MaxBytes         uint64
	CapabilityEpoch  uint64
	IndexLimits      schema.IndexLimits
	IndexExpressions IndexExpressionOptions
	Builtin          builtin.ResolveOptions
	PartitionLimits  logicalpartition.Limits
}

// CatalogAccess borrows a pinned catalog snapshot, its matching data reader and
// a key codec implementing both storage.KeyEncoder and storage.KeyDecoder. The
// caller serializes calls and keeps them alive. Returned metadata is detached;
// this collaborator neither opens nor closes snapshots.
type CatalogAccess struct {
	catalog *catalog.CatalogSnapshot
	reader  storage.SnapshotReader
	encoder storage.KeyEncoder
	options CatalogAccessOptions
}

func NewCatalogAccess(snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, encoder storage.KeyEncoder, options CatalogAccessOptions) (*CatalogAccess, error) {
	limits := options.IndexLimits
	_, decoder := encoder.(storage.KeyDecoder)
	missingCollaborator := snapshot == nil || reader == nil || encoder == nil || !decoder
	if missingCollaborator || snapshot.Timestamp() <= 0 || options.CapabilityEpoch == 0 {
		return nil, fmt.Errorf("%w: catalog access requires pinned collaborators, key decoding, and a capability epoch", opt.ErrInvalid)
	}
	if options.IndexExpressions == (IndexExpressionOptions{}) {
		return nil, fmt.Errorf("%w: catalog access requires index expression limits", opt.ErrInvalid)
	}
	if _, err := NewIndexExpressionCompiler(snapshot, options.IndexExpressions); err != nil {
		return nil, err
	}
	invalidPaths := options.MaxPaths <= 0 || options.MaxBytes == 0 || options.MaxBytes > math.MaxInt
	invalidMetadata := limits.MetadataBytes == 0 || limits.MetadataBytes > math.MaxInt
	invalidEntries := limits.EntryBytes == 0 || limits.EntryBytes > math.MaxInt
	invalidColumns := limits.MaxColumns <= 0 || uint64(limits.MaxColumns) > math.MaxUint32
	if invalidPaths || invalidMetadata || invalidEntries || invalidColumns {
		return nil, opt.ErrLimit
	}
	if timestamped, ok := reader.(interface{ Timestamp() model.Timestamp }); ok && timestamped.Timestamp() != snapshot.Timestamp() {
		return nil, fmt.Errorf("%w: catalog and reader snapshots differ", opt.ErrInvalid)
	}
	partitionLimits := options.PartitionLimits
	if partitionLimits.MaxChildren == 0 && partitionLimits.MaxKeys == 0 && partitionLimits.MaxBytes == 0 {
		partitionLimits = logicalpartition.DefaultLimits()
	}
	if partitionLimits.MaxChildren <= 0 || uint64(partitionLimits.MaxChildren) > math.MaxUint32 || partitionLimits.MaxKeys <= 0 || partitionLimits.MaxBytes == 0 {
		return nil, opt.ErrLimit
	}
	partitionLimits.MaxBytes = min(partitionLimits.MaxBytes, options.MaxBytes)
	options.PartitionLimits = partitionLimits
	return &CatalogAccess{catalog: snapshot, reader: reader, encoder: encoder, options: options}, nil
}

func (a *CatalogAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	if err := ctx.Err(); err != nil {
		return opt.Access{}, err
	}
	if request.MaxPaths <= 0 || request.MaxBytes == 0 {
		return opt.Access{}, opt.ErrLimit
	}
	if request.Predicate != 0 {
		if request.Graph == nil {
			return opt.Access{}, opt.ErrInvalid
		}
		if _, ok := request.Graph.Scalar(request.Predicate); !ok {
			return opt.Access{}, opt.ErrInvalid
		}
	}
	budget := catalogAccessBudget{remaining: min(request.MaxBytes, a.options.MaxBytes)}
	if err := budget.charge(1, uint64(unsafe.Sizeof(opt.Access{}))); err != nil {
		return opt.Access{}, err
	}
	published, err := a.catalog.Descriptor(ctx, request.Object)
	if err != nil {
		return opt.Access{}, err
	}
	table := published.Descriptor
	if table == nil {
		return opt.Access{}, catalog.ErrCorruptCatalog
	}
	wrongSource := table.Kind() != model.ObjectTable && table.Kind() != model.ObjectMaterializedView || table.State() != model.StatePublic
	staleSource := table.ID() != request.Object || table.Version() != request.DescriptorVersion || table.Physical().Generation != request.Generation
	if wrongSource || staleSource {
		return opt.Access{}, fmt.Errorf("%w: stale or nonpublic access source", opt.ErrInvalid)
	}
	descriptorLimits := model.DescriptorLimits{MaxBytes: budget.remaining, MaxItems: int(budget.remaining), MaxDepth: 64}
	spec, err := table.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return opt.Access{}, err
	}
	if spec.Table == nil || len(spec.Table.Columns) > a.options.IndexLimits.MaxColumns {
		return opt.Access{}, opt.ErrLimit
	}
	primary, err := a.primary(ctx, table, spec.Table, &budget)
	if err != nil {
		return opt.Access{}, err
	}
	for _, id := range request.Columns {
		if err := ctx.Err(); err != nil {
			return opt.Access{}, err
		}
		if !catalogAccessCovers(primary.ColumnMap, id) {
			return opt.Access{}, storage.ErrInvalidProjection
		}
	}
	base, err := budget.copyRequest(ctx, storage.PushdownRequest{Layout: primary.Layout})
	if err != nil {
		return opt.Access{}, err
	}
	pathLimit := min(request.MaxPaths, a.options.MaxPaths)
	capacity := min(pathLimit, len(spec.Table.Indexes)+1)
	if err := budget.charge(uint64(capacity), uint64(unsafe.Sizeof(opt.AccessPath{}))); err != nil {
		return opt.Access{}, err
	}
	result := opt.Access{Target: primary.Target, Layout: base.Layout, Paths: make([]opt.AccessPath, 1, capacity)}
	result.Paths[0] = primary
	slices.Sort(spec.Table.Indexes)
	for _, id := range spec.Table.Indexes {
		if err := ctx.Err(); err != nil {
			return opt.Access{}, err
		}
		if len(result.Paths) == pathLimit {
			break
		}
		published, err := a.catalog.Descriptor(ctx, id)
		if err != nil {
			return opt.Access{}, err
		}
		index := published.Descriptor
		if index == nil || index.ID() != id || index.Kind() != model.ObjectIndex || id == table.ID() {
			return opt.Access{}, catalog.ErrCorruptCatalog
		}
		indexSpec, err := index.SpecWithin(ctx, descriptorLimits)
		if err != nil {
			return opt.Access{}, err
		}
		if indexSpec.Index == nil || indexSpec.Index.Table != table.ID() {
			return opt.Access{}, catalog.ErrCorruptCatalog
		}
		if index.State() != model.StatePublic || !indexSpec.Index.Valid() || len(indexSpec.Index.Keys) == 0 {
			continue
		}
		complex := indexSpec.Index.Predicate != nil || slices.ContainsFunc(indexSpec.Index.Keys, func(key model.KeyElement) bool { return key.Expression != nil })
		var compiler *IndexExpressionCompiler
		if complex {
			compiler, err = NewIndexExpressionCompiler(a.catalog, a.options.IndexExpressions)
			if err != nil {
				return opt.Access{}, err
			}
		}
		// A different key collation cannot stand in for the bound base column.
		if slices.ContainsFunc(indexSpec.Index.Keys, func(key model.KeyElement) bool {
			if key.Column == 0 {
				return false
			}
			return slices.ContainsFunc(primary.Layout, func(column storage.ColumnLayout) bool {
				return column.ID == key.Column && !storage.KeyLayoutsEqual([]storage.KeyColumn{{ColumnID: column.ID, Type: column.Type, Children: column.Children}}, []storage.KeyColumn{{ColumnID: key.Column, Type: key.Type, Children: key.Children}})
			})
		}) {
			continue
		}
		path, err := a.secondary(ctx, table, index, indexSpec.Index, primary, request.Columns, compiler, &budget)
		if err != nil {
			return opt.Access{}, err
		}
		if path.Target.ObjectID == 0 {
			continue
		}
		result.Paths = append(result.Paths, path)
	}
	constraints, err := a.partitionConstraints(ctx, table, spec.Table, &budget)
	if err != nil {
		return opt.Access{}, err
	}
	for i := range result.Paths {
		result.Paths[i].Partitions, err = mapPartitionConstraints(ctx, constraints, result.Paths[i], &budget)
		if err != nil {
			return opt.Access{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return opt.Access{}, err
	}
	return result, nil
}
func (a *CatalogAccess) partitionConstraints(ctx context.Context, leaf *model.Descriptor, leafTable *model.TableMetadata, budget *catalogAccessBudget) ([]opt.PartitionAccess, error) {
	current, metadata := leaf, leafTable
	var result []opt.PartitionAccess
	for range 64 {
		if metadata.PartitionBound == nil || metadata.PartitionState != model.PartitionAttached {
			return result, nil
		}
		var parent *model.Descriptor
		for _, parentID := range metadata.Parents {
			published, err := a.catalog.Descriptor(ctx, parentID)
			if err != nil {
				return nil, err
			}
			candidate := published.Descriptor
			if candidate == nil {
				return nil, catalog.ErrCorruptCatalog
			}
			candidateTable, ok := candidate.Table()
			if !ok || candidate.State() != model.StatePublic || candidateTable.PartitionStrategy != metadata.PartitionBound.Strategy {
				continue
			}
			if parent != nil {
				return nil, catalog.ErrCorruptCatalog
			}
			parent = candidate
		}
		if parent == nil {
			return nil, catalog.ErrCorruptCatalog
		}
		parentTable, ok := parent.Table()
		if !ok {
			return nil, catalog.ErrCorruptCatalog
		}
		keys, direct, err := partitionConstraintKeys(ctx, &parentTable, leafTable, budget)
		if err != nil {
			return nil, err
		}
		if direct {
			children, err := directPartitionChildren(ctx, a.catalog, parent, uint32(a.options.PartitionLimits.MaxChildren))
			if err != nil {
				return nil, err
			}
			limits := a.options.PartitionLimits
			limits.MaxBytes = min(limits.MaxBytes, budget.remaining)
			router, err := logicalpartition.NewRouter(ctx, logicalpartition.Options{
				Parent: parent, Children: children, Encoder: a.encoder, Builtin: a.options.Builtin, Limits: limits,
			})
			if err != nil {
				return nil, err
			}
			if err := budget.charge(1, router.OwnedBytes()); err != nil {
				return nil, err
			}
			if result == nil {
				if err := budget.charge(uint64(64), uint64(unsafe.Sizeof(opt.PartitionAccess{}))); err != nil {
					return nil, err
				}
				result = make([]opt.PartitionAccess, 0, 64)
			}
			result = append(result, opt.PartitionAccess{Router: router, Child: current.ID(), Keys: keys})
		}
		current, metadata = parent, &parentTable
	}
	return nil, opt.ErrLimit
}

func partitionConstraintKeys(ctx context.Context, parent, leaf *model.TableMetadata, budget *catalogAccessBudget) ([]storage.KeyColumn, bool, error) {
	if len(parent.PartitionKeys) == 0 {
		return nil, false, catalog.ErrCorruptCatalog
	}
	keys := make([]storage.KeyColumn, len(parent.PartitionKeys))
	for i, key := range parent.PartitionKeys {
		if key.Column == 0 || key.Expression != nil {
			return nil, false, nil
		}
		parentIndex := slices.IndexFunc(parent.Columns, func(column model.ColumnDescriptor) bool {
			return column.ID == key.Column && column.State == model.StatePublic
		})
		if parentIndex < 0 {
			return nil, false, catalog.ErrCorruptCatalog
		}
		parentColumn := parent.Columns[parentIndex]
		leafIndex := slices.IndexFunc(leaf.Columns, func(column model.ColumnDescriptor) bool {
			if column.State != model.StatePublic {
				return false
			}
			if parentColumn.SystemIdentity || column.SystemIdentity {
				return parentColumn.SystemIdentity && column.SystemIdentity
			}
			return column.Name == parentColumn.Name
		})
		if leafIndex < 0 {
			return nil, false, catalog.ErrCorruptCatalog
		}
		leafColumn := leaf.Columns[leafIndex]
		expected := storage.KeyColumn{ColumnID: leafColumn.ID, Type: key.Type, Children: key.Children}
		actual := storage.KeyColumn{ColumnID: leafColumn.ID, Type: leafColumn.Layout.Type, Children: leafColumn.Layout.Children}
		if !storage.KeyLayoutsEqual([]storage.KeyColumn{expected}, []storage.KeyColumn{actual}) {
			return nil, false, catalog.ErrCorruptCatalog
		}
		keys[i] = storage.KeyColumn{
			ColumnID: leafColumn.ID, Type: key.Type, Children: key.Children,
			Descending: key.Descending, NullsFirst: key.NullsFirst,
		}
	}
	owned, used, err := storage.CopyKeyLayout(ctx, keys, budget.remaining)
	if err != nil {
		return nil, false, err
	}
	budget.remaining -= used
	return owned, true, nil
}

func mapPartitionConstraints(ctx context.Context, constraints []opt.PartitionAccess, path opt.AccessPath, budget *catalogAccessBudget) ([]opt.PartitionAccess, error) {
	if len(constraints) == 0 {
		return nil, nil
	}
	if err := budget.charge(uint64(len(constraints)), uint64(unsafe.Sizeof(opt.PartitionAccess{}))); err != nil {
		return nil, err
	}
	result := make([]opt.PartitionAccess, 0, len(constraints))
	scan := physical.ScanSpec{ColumnMap: path.ColumnMap}
	for _, constraint := range constraints {
		keys := make([]storage.KeyColumn, len(constraint.Keys))
		copy(keys, constraint.Keys)
		for i := range keys {
			keys[i].ColumnID = scan.PhysicalColumn(keys[i].ColumnID)
			if keys[i].ColumnID == 0 {
				keys = nil
				break
			}
		}
		if keys == nil {
			continue
		}
		owned, used, err := storage.CopyKeyLayout(ctx, keys, budget.remaining)
		if err != nil {
			return nil, err
		}
		budget.remaining -= used
		result = append(result, opt.PartitionAccess{Router: constraint.Router, Child: constraint.Child, Keys: owned})
	}
	return result, nil
}

func (a *CatalogAccess) primary(ctx context.Context, descriptor *model.Descriptor, table *model.TableMetadata, budget *catalogAccessBudget) (opt.AccessPath, error) {
	identity := descriptor.Physical()
	target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: identity.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: identity.EncodingVersion}
	if err := target.Validate(); err != nil {
		return opt.AccessPath{}, err
	}
	if err := budget.charge(uint64(len(table.Columns)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))+uint64(unsafe.Sizeof(physical.ScanColumn{}))); err != nil {
		return opt.AccessPath{}, err
	}
	path := opt.AccessPath{Algorithm: physical.TableScan, Target: target, Layout: make([]storage.ColumnLayout, 0, len(table.Columns)), ColumnMap: make([]physical.ScanColumn, 0, len(table.Columns)), Direction: storage.ScanForward}
	for _, column := range table.Columns {
		if err := ctx.Err(); err != nil {
			return opt.AccessPath{}, err
		}
		if column.State != model.StatePublic {
			continue
		}
		layout, used, err := model.CopyTypeLayout(ctx, column.Layout, budget.remaining)
		if err != nil {
			return opt.AccessPath{}, err
		}
		budget.remaining -= used
		stored := storage.ColumnLayout{ID: column.ID, Type: layout.Type, Children: layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			value, used, err := model.CopyValue(ctx, *column.ConstantDefault, budget.remaining)
			if err != nil {
				return opt.AccessPath{}, err
			}
			budget.remaining -= used
			stored.HasMissingValue, stored.MissingValue = true, value
		}
		path.Layout = append(path.Layout, stored)
		path.ColumnMap = append(path.ColumnMap, physical.ScanColumn{Source: column.ID, Target: column.ID})
	}
	slices.SortFunc(path.Layout, func(left, right storage.ColumnLayout) int {
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	if err := budget.charge(uint64(len(table.PrimaryKey)), uint64(unsafe.Sizeof(storage.KeyColumn{}))); err != nil {
		return opt.AccessPath{}, err
	}
	path.Keys = make([]storage.KeyColumn, len(table.PrimaryKey))
	for i, key := range table.PrimaryKey {
		if key.Column == 0 || key.Expression != nil || key.Collation != key.Type.Collation {
			return opt.AccessPath{}, schema.ErrInvalidIndexPlan
		}
		var layout model.TypeLayout
		for _, column := range path.Layout {
			if column.ID == key.Column {
				layout = model.TypeLayout{Type: column.Type, Children: column.Children}
				break
			}
		}
		if !storage.KeyLayoutsEqual([]storage.KeyColumn{{ColumnID: key.Column, Type: layout.Type, Children: layout.Children}}, []storage.KeyColumn{{ColumnID: key.Column, Type: key.Type, Children: key.Children}}) {
			return opt.AccessPath{}, schema.ErrInvalidIndexPlan
		}
		if key.OperatorClass != 0 {
			encoder, ok := a.encoder.(storage.IndexKeyEncoder)
			if !ok {
				return opt.AccessPath{}, &schema.IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass}
			}
			if err := encoder.ValidateOperatorClass(ctx, layout, key.OperatorClass); err != nil {
				return opt.AccessPath{}, err
			}
		}
		path.Keys[i] = storage.KeyColumn{ColumnID: key.Column, Type: layout.Type, Children: layout.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	keys, used, err := storage.CopyKeyLayout(ctx, path.Keys, budget.remaining)
	if err != nil {
		return opt.AccessPath{}, err
	}
	budget.remaining -= used
	path.Keys = keys
	if err := budget.charge(uint64(len(table.PrimaryKey)), uint64(unsafe.Sizeof(model.OperatorID(0)))); err != nil {
		return opt.AccessPath{}, err
	}
	path.Operators = make([]model.OperatorID, 0, len(table.PrimaryKey))
	for _, key := range table.PrimaryKey {
		operator := catalogAccessEquality(key)
		if operator == 0 {
			break
		}
		path.Operators = append(path.Operators, operator)
	}
	if len(path.Keys) != 0 {
		path.Algorithm = physical.PrimaryKeyScan
	}
	if err := (storage.PushdownRequest{Target: target, Layout: path.Layout, KeyLayout: path.Keys, Direction: path.Direction}).Validate(ctx); err != nil {
		return opt.AccessPath{}, err
	}
	return path, nil
}

func catalogAccessEquality(key model.KeyElement) model.OperatorID {
	class, ok := builtin.LookupOperatorClass(key.OperatorClass)
	if !ok || class.Method != "btree" {
		return 0
	}
	for operator := range builtin.OperatorsNamed("=") {
		if operator.Left != key.Type.ID || operator.Right != key.Type.ID {
			continue
		}
		for interpretation := range builtin.RowComparisonInterpretations(operator.ID) {
			if interpretation.Family == class.Family && interpretation.Strategy == 3 {
				return operator.ID
			}
		}
	}
	return 0
}

func catalogAccessExpression(ctx context.Context, binding *bind.BoundIndexExpression, budget *catalogAccessBudget) (opt.AccessExpression, error) {
	if binding == nil || binding.Graph == nil || binding.Root == 0 {
		return opt.AccessExpression{}, opt.ErrInvalid
	}
	if err := budget.charge(1, uint64(unsafe.Sizeof(*binding.Graph))+binding.Graph.OwnedBytes()); err != nil {
		return opt.AccessExpression{}, err
	}
	if err := budget.charge(uint64(len(binding.Parameters)), uint64(unsafe.Sizeof(opt.AccessExpressionParameter{}))); err != nil {
		return opt.AccessExpression{}, err
	}
	result := opt.AccessExpression{Graph: binding.Graph, Root: binding.Root, Parameters: make([]opt.AccessExpressionParameter, len(binding.Parameters)), Builtin: binding.Builtin}
	for i, parameter := range binding.Parameters {
		layout, used, err := model.CopyTypeLayout(ctx, parameter.Layout, budget.remaining)
		if err != nil {
			return opt.AccessExpression{}, err
		}
		budget.remaining -= used
		result.Parameters[i] = opt.AccessExpressionParameter{Column: parameter.Column, Layout: layout}
	}
	return result, nil
}

func (a *CatalogAccess) secondary(ctx context.Context, table, index *model.Descriptor, metadata *model.IndexMetadata, primary opt.AccessPath, columns []model.ColumnID, compiler *IndexExpressionCompiler, budget *catalogAccessBudget) (opt.AccessPath, error) {
	limits := a.options.IndexLimits
	limits.MetadataBytes = min(limits.MetadataBytes, budget.remaining)
	plan, err := schema.NewIndexPlan(ctx, table, index, a.encoder, compiler, limits)
	if err != nil {
		return opt.AccessPath{}, err
	}
	layout := plan.Layout()
	keys := plan.KeyLayout()
	owned, err := budget.copyRequest(ctx, storage.PushdownRequest{Target: plan.Target(), Layout: layout.Columns, KeyLayout: keys})
	if err != nil {
		return opt.AccessPath{}, err
	}
	if err := budget.charge(uint64(len(metadata.Keys)+len(metadata.Include)), uint64(unsafe.Sizeof(physical.ScanColumn{}))); err != nil {
		return opt.AccessPath{}, err
	}
	mapping := make([]physical.ScanColumn, 0, len(metadata.Keys)+len(metadata.Include))
	if err := budget.charge(uint64(len(metadata.Keys)), uint64(unsafe.Sizeof(opt.AccessExpression{}))+uint64(unsafe.Sizeof(model.OperatorID(0)))); err != nil {
		return opt.AccessPath{}, err
	}
	expressions := make([]opt.AccessExpression, len(metadata.Keys))
	operators := make([]model.OperatorID, len(metadata.Keys))
	for i, key := range metadata.Keys {
		if key.Column != 0 {
			if !catalogAccessCovers(mapping, key.Column) {
				mapping = append(mapping, physical.ScanColumn{Source: key.Column, Target: keys[i].ColumnID})
			}
		} else {
			if compiler == nil || key.Expression == nil {
				return opt.AccessPath{}, opt.ErrInvalid
			}
			binding, err := compiler.BindIndexExpression(ctx, key.Expression, primary.Layout, model.TypeLayout{Type: key.Type, Children: key.Children})
			if err != nil {
				return opt.AccessPath{}, err
			}
			expressions[i], err = catalogAccessExpression(ctx, binding, budget)
			if err != nil {
				return opt.AccessPath{}, err
			}
		}
		operators[i] = catalogAccessEquality(key)
	}
	include := 0
	for _, column := range layout.Columns {
		if column.ID <= plan.OwnerColumn() {
			continue
		}
		id := metadata.Include[include]
		include++
		if !catalogAccessCovers(mapping, id) {
			mapping = append(mapping, physical.ScanColumn{Source: id, Target: column.ID})
		}
	}
	path := opt.AccessPath{Algorithm: physical.SecondaryScan, Index: model.IndexID(plan.Target().ObjectID), Target: owned.Target, Layout: owned.Layout, Keys: owned.KeyLayout, KeyExpressions: expressions, Operators: operators, ColumnMap: mapping, Direction: storage.ScanForward}
	if metadata.Predicate != nil {
		if compiler == nil {
			return opt.AccessPath{}, opt.ErrInvalid
		}
		boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
		binding, err := compiler.BindIndexExpression(ctx, metadata.Predicate, primary.Layout, boolean)
		if err != nil {
			return opt.AccessPath{}, err
		}
		path.Predicate, err = catalogAccessExpression(ctx, binding, budget)
		if err != nil {
			return opt.AccessPath{}, err
		}
	}
	covering := true
	for _, column := range columns {
		covering = covering && catalogAccessCovers(mapping, column)
	}
	if covering {
		return path, nil
	}
	base, err := budget.copyRequest(ctx, storage.PushdownRequest{Target: primary.Target, Layout: primary.Layout, KeyLayout: primary.Keys, Direction: storage.ScanForward})
	if err != nil {
		return opt.AccessPath{}, err
	}
	if err := budget.charge(uint64(len(primary.ColumnMap)), uint64(unsafe.Sizeof(physical.ScanColumn{}))); err != nil {
		return opt.AccessPath{}, err
	}
	baseMap := make([]physical.ScanColumn, len(primary.ColumnMap))
	copy(baseMap, primary.ColumnMap)
	path.Lookup = opt.LookupAccess{Enabled: true, OwnerColumn: plan.OwnerColumn(), Target: base.Target, Layout: base.Layout, ColumnMap: baseMap, Keys: base.KeyLayout}
	return path, nil
}

func catalogAccessCovers(mapping []physical.ScanColumn, id model.ColumnID) bool {
	return slices.ContainsFunc(mapping, func(column physical.ScanColumn) bool { return column.Source == id })
}

type catalogAccessBudget struct{ remaining uint64 }

func (b *catalogAccessBudget) charge(count, size uint64) error {
	if size != 0 && count > b.remaining/size {
		return opt.ErrLimit
	}
	b.remaining -= count * size
	return nil
}

func (b *catalogAccessBudget) copyRequest(ctx context.Context, request storage.PushdownRequest) (storage.PushdownRequest, error) {
	owned, used, err := storage.CopyPushdownRequest(ctx, request, b.remaining)
	if err != nil {
		return storage.PushdownRequest{}, err
	}
	b.remaining -= used
	return owned, nil
}

// Negotiate retires the backend binding before returning only capability data.
// The optimizer supplies Source and ColumnMap from the selected AccessPath.
func (a *CatalogAccess) Negotiate(ctx context.Context, request storage.PushdownRequest, maxBytes uint64) (result physical.ScanSpec, err error) {
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	budget := catalogAccessBudget{remaining: min(maxBytes, a.options.MaxBytes)}
	if err := budget.charge(1, uint64(unsafe.Sizeof(physical.ScanSpec{}))); err != nil {
		return physical.ScanSpec{}, err
	}
	var plan storage.PushdownPlan
	if negotiator, ok := a.reader.(interface {
		NegotiateScan(context.Context, storage.PushdownRequest) (storage.PushdownPlan, error)
	}); ok {
		plan, err = negotiator.NegotiateScan(ctx, request)
	} else {
		plan, err = a.reader.PlanScan(ctx, request)
		if err == nil || plan.Binding != ([32]byte{}) {
			defer func() {
				err = errors.Join(err, a.reader.ReleasePlan(plan))
				if err != nil {
					result = physical.ScanSpec{}
				}
			}()
		}
	}
	if err != nil {
		return physical.ScanSpec{}, err
	}
	if plan.SnapshotTS != a.catalog.Timestamp() {
		return physical.ScanSpec{}, storage.ErrInvalidScanPlan
	}
	owned, err := budget.copyRequest(ctx, plan.Request)
	if err != nil {
		return physical.ScanSpec{}, err
	}
	accepted, err := budget.copyRequest(ctx, storage.PushdownRequest{Predicate: plan.Accepted, ColumnIDs: plan.Projection})
	if err != nil {
		return physical.ScanSpec{}, err
	}
	residual, err := budget.copyRequest(ctx, storage.PushdownRequest{Predicate: plan.Residual})
	if err != nil {
		return physical.ScanSpec{}, err
	}
	if err := budget.charge(uint64(len(plan.Ordering)), uint64(unsafe.Sizeof(storage.ScanOrdering{}))); err != nil {
		return physical.ScanSpec{}, err
	}
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	ordering := make([]storage.ScanOrdering, len(plan.Ordering))
	copy(ordering, plan.Ordering)
	return physical.ScanSpec{CapabilityEpoch: a.options.CapabilityEpoch, Request: owned, Accepted: accepted.Predicate, Residual: residual.Predicate, Projection: accepted.ColumnIDs, Ordering: ordering, Estimate: plan.Estimate, AppliedLimit: plan.AppliedLimit}, nil
}

var _ opt.AccessPlanner = (*CatalogAccess)(nil)
