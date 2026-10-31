package schema

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errIndexSetDescriptor = &model.Error{Code: "55000", Message: "index maintenance does not match table descriptor", Cause: storage.ErrInvalidTarget}
	errIndexSetLimit      = &model.Error{Code: "54000", Message: "index maintenance exceeds resource limit", Cause: errors.Join(ErrIndexLimit, model.ErrValueBudget)}
)

// IndexSet borrows immutable plans and owns the exact mutation policies for one
// table descriptor. It is safe to share while its expression capabilities are.
type IndexSet struct {
	table          *model.Descriptor
	target         storage.ObjectRef
	indexes        []indexBinding
	limits         IndexLimits
	workspaceBytes uint64
	shadow         *GenerationPlan
	relations      *relationalSet
	schemaName     string
}

type indexBinding struct {
	plan        *IndexPlan
	target      storage.ObjectRef
	normalizer  *txn.RowNormalizer
	ownerLayout [1]storage.ColumnLayout
	ownerIDs    [1]model.ColumnID
}

// tableShapedKind reports whether a descriptor kind owns a table-shaped
// physical relation whose rows index maintenance can bind to: an ordinary
// table, or the backing table of a materialized view.
func tableShapedKind(kind model.ObjectKind) bool {
	return kind == model.ObjectTable || kind == model.ObjectMaterializedView
}

func NewIndexSet(ctx context.Context, table *model.Descriptor, plans []*IndexPlan, limits IndexLimits) (*IndexSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.MetadataBytes == 0 || limits.EntryBytes == 0 || limits.MaxColumns <= 0 {
		return nil, errIndexSetLimit
	}
	if table == nil || !tableShapedKind(table.Kind()) || table.State() != model.StatePublic {
		return nil, errIndexSetDescriptor
	}
	physical := table.Physical()
	target := storage.ObjectRef{ObjectID: table.ID(), Generation: physical.Generation, DescriptorVersion: table.Version(), EncodingVersion: physical.EncodingVersion}
	if err := target.Validate(); err != nil {
		return nil, errIndexSetDescriptor
	}
	budget := indexSetBudget{remaining: limits.MetadataBytes}
	if err := budget.charge(1, uint64(unsafe.Sizeof(IndexSet{}))); err != nil {
		return nil, err
	}
	if err := budget.charge(uint64(len(plans)), uint64(unsafe.Sizeof(indexBinding{}))); err != nil {
		return nil, err
	}
	set := &IndexSet{table: table, target: target, limits: limits, indexes: make([]indexBinding, len(plans))}
	for i, plan := range plans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if plan == nil {
			return nil, errIndexSetDescriptor
		}
		owned := *plan
		plan = &owned
		id, version := plan.Table()
		if id != table.ID() || version != table.Version() || plan.Target().ObjectID == table.ID() {
			return nil, errIndexSetDescriptor
		}
		if plan.entryBytes() == 0 || plan.entryBytes() > limits.EntryBytes {
			return nil, errIndexSetLimit
		}
		switch plan.State() {
		case model.StateAbsent, model.StateDeleteOnly, model.StateWriteOnly, model.StatePublic, model.StateDropWriteOnly, model.StateDropDeleteOnly:
		default:
			return nil, errIndexSetDescriptor
		}
		binding, err := newIndexBinding(ctx, plan, limits, &budget)
		if err != nil {
			return nil, err
		}
		set.indexes[i] = binding
	}
	slices.SortFunc(set.indexes, func(a, b indexBinding) int {
		return cmp.Compare(a.target.ObjectID, b.target.ObjectID)
	})
	for i := 1; i < len(set.indexes); i++ {
		if set.indexes[i-1].target.ObjectID == set.indexes[i].target.ObjectID {
			return nil, errIndexSetDescriptor
		}
	}
	if err := set.configureConstraintIndexes(ctx, table); err != nil {
		return nil, err
	}
	if err := set.validateDeclaredIndexes(ctx, table); err != nil {
		return nil, err
	}
	if len(set.indexes) != 0 {
		// Entry construction can overlap one prior entry; owner reads project
		// only bytea keys. Program-internal allocations follow their own policy.
		fixed := 2*uint64(unsafe.Sizeof(storage.Mutation{})) + uint64(unsafe.Sizeof(storage.ReadRequest{})) + uint64(unsafe.Sizeof(model.Row{})) + uint64(unsafe.Sizeof(model.Value{})) + 2*uint64(unsafe.Sizeof(IndexEntry{}))
		if limits.EntryBytes > (^uint64(0)-fixed)/4 {
			return nil, errIndexSetLimit
		}
		set.workspaceBytes = 4*limits.EntryBytes + fixed
	}
	return set, nil
}

type indexSetBudget struct {
	remaining uint64
}

func (b *indexSetBudget) charge(count, width uint64) error {
	if width != 0 && count > b.remaining/width {
		return errIndexSetLimit
	}
	b.remaining -= count * width
	return nil
}

func newIndexBinding(ctx context.Context, plan *IndexPlan, limits IndexLimits, budget *indexSetBudget) (indexBinding, error) {
	// Admit both layout snapshots before invoking accessors on a plan that may
	// have been constructed under a larger metadata allowance.
	if err := budget.charge(2, plan.metadataBytes()); err != nil {
		return indexBinding{}, err
	}
	layout, keys := plan.Layout(), plan.KeyLayout()
	if layout.Target != plan.Target() || len(layout.Columns) > limits.MaxColumns || len(keys) < 2 || len(keys) >= len(layout.Columns) {
		return indexBinding{}, errIndexSetDescriptor
	}
	if err := layout.Validate(ctx); err != nil {
		return indexBinding{}, err
	}
	if err := budget.charge(1, uint64(unsafe.Sizeof(txn.RowNormalizer{}))); err != nil {
		return indexBinding{}, err
	}
	if err := budget.charge(uint64(len(layout.Columns)), uint64(unsafe.Sizeof(txn.ColumnRule{}))); err != nil {
		return indexBinding{}, err
	}
	keyBytes, err := storage.KeyLayoutBytes(ctx, keys, budget.remaining)
	if err != nil {
		return indexBinding{}, err
	}
	if err := budget.charge(1, keyBytes); err != nil {
		return indexBinding{}, err
	}
	rules := make([]txn.ColumnRule, len(layout.Columns))
	for i, column := range layout.Columns {
		if column.ID != model.ColumnID(i+1) || column.HasMissingValue {
			return indexBinding{}, errIndexSetDescriptor
		}
		rules[i].Layout = column
		if i >= len(keys) {
			continue
		}
		key := keys[i]
		expected := storage.KeyColumn{ColumnID: column.ID, Type: column.Type, Children: column.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		if !storage.KeyLayoutsEqual([]storage.KeyColumn{key}, []storage.KeyColumn{expected}) {
			return indexBinding{}, errIndexSetDescriptor
		}
		rules[i].KeyPosition = uint32(i + 1)
		rules[i].KeyDescending = key.Descending
		rules[i].KeyNullsFirst = key.NullsFirst
	}
	owner := layout.Columns[len(keys)]
	if owner.ID != plan.OwnerColumn() || owner.Type.Storage != model.TypeBytes || owner.Nullable || len(owner.Children) != 0 {
		return indexBinding{}, errIndexSetDescriptor
	}
	normalizer, err := txn.NewRowNormalizer(ctx, layout.Target, rules, max(limits.MetadataBytes, limits.EntryBytes))
	if err != nil {
		return indexBinding{}, err
	}
	return indexBinding{plan: plan, target: layout.Target, normalizer: normalizer, ownerLayout: [1]storage.ColumnLayout{owner}, ownerIDs: [1]model.ColumnID{owner.ID}}, nil
}

func (s *IndexSet) configureConstraintIndexes(ctx context.Context, table *model.Descriptor) error {
	spec, err := table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: s.limits.MetadataBytes, MaxItems: max(s.limits.MaxColumns, len(s.indexes)), MaxDepth: 64})
	if err != nil {
		return err
	}
	if spec.Table == nil {
		return errIndexSetDescriptor
	}
	for _, constraint := range spec.Table.Constraints {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch constraint.Kind {
		case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
		default:
			continue
		}
		binding, found := s.index(constraint.Index)
		if !found || !constraint.Mutation && !binding.plan.Valid() || constraint.Mutation && !binding.plan.WriteMaintained() ||
			binding.plan.State() != model.StatePublic && (binding.plan.State() != model.StateWriteOnly || !constraint.Mutation) {
			return errIndexSetDescriptor
		}
		if constraint.Kind != model.ConstraintExclude && !binding.plan.isUnique() {
			return errIndexSetDescriptor
		}
		binding.plan.deferred = constraint.Deferrable
		binding.plan.exclusion = constraint.Kind == model.ConstraintExclude
	}
	return ctx.Err()
}

func (s *IndexSet) ValidateDescriptor(ctx context.Context, table *model.Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || table == nil || !tableShapedKind(table.Kind()) || table.State() != model.StatePublic {
		return errIndexSetDescriptor
	}
	physical := table.Physical()
	if table.ID() != s.target.ObjectID || table.Version() != s.target.DescriptorVersion || physical.Generation != s.target.Generation || physical.EncodingVersion != s.target.EncodingVersion {
		return errIndexSetDescriptor
	}
	if table != s.table {
		if err := s.validateDeclaredIndexes(ctx, table); err != nil {
			return err
		}
	}
	if s.shadow != nil {
		return s.shadow.ValidateDescriptor(ctx, table)
	}
	return nil
}

func (s *IndexSet) validateDeclaredIndexes(ctx context.Context, table *model.Descriptor) error {
	spec, err := table.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: s.limits.MetadataBytes, MaxItems: max(s.limits.MaxColumns, len(s.indexes)), MaxDepth: 64})
	if err != nil {
		return err
	}
	if spec.Table == nil || len(spec.Table.Indexes) != len(s.indexes) || len(spec.Table.Columns) > s.limits.MaxColumns {
		return errIndexSetDescriptor
	}
	slices.Sort(spec.Table.Indexes)
	for i, id := range spec.Table.Indexes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id != s.indexes[i].target.ObjectID {
			return errIndexSetDescriptor
		}
	}
	for _, constraint := range spec.Table.Constraints {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch constraint.Kind {
		case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
		default:
			continue
		}
		binding, found := s.index(constraint.Index)
		if !found || !constraint.Mutation && !binding.plan.Valid() || constraint.Mutation && !binding.plan.WriteMaintained() ||
			binding.plan.State() != model.StatePublic && (binding.plan.State() != model.StateWriteOnly || !constraint.Mutation) {
			return errIndexSetDescriptor
		}
		plan := binding.plan
		if constraint.Kind != model.ConstraintExclude && !plan.isUnique() || plan.deferred != constraint.Deferrable ||
			plan.predicate != nil && constraint.Kind != model.ConstraintExclude || len(constraint.Columns) == 0 || len(constraint.Columns) != len(plan.keys) {
			return errIndexSetDescriptor
		}
		for i, key := range plan.keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if key.expression != nil || key.column < 0 || key.column >= len(plan.baseColumns) ||
				plan.baseColumns[key.column].ID != constraint.Columns[i] {
				return errIndexSetDescriptor
			}
		}
	}
	return nil
}

func (s *IndexSet) WorkspaceBytes() uint64 {
	if s == nil {
		return 0
	}
	bytes := s.workspaceBytes
	if s.shadow != nil {
		bytes += s.shadow.WorkspaceBytes()
	}
	if s.relations != nil {
		bytes = max(bytes, s.relations.maxBytes)
	}
	return bytes
}
func (s *IndexSet) Layouts() []storage.ObjectLayout {
	if s == nil {
		return nil
	}
	layouts := make([]storage.ObjectLayout, len(s.indexes))
	for i := range s.indexes {
		layouts[i] = s.indexes[i].normalizer.Layout()
	}
	if s.shadow != nil {
		layouts = append(layouts, s.shadow.Layouts()...)
	}
	return layouts
}

func (s *IndexSet) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding, found := s.index(target.ObjectID)
	if !found || binding.target != target {
		return nil, storage.ErrInvalidTarget
	}
	return binding.normalizer, nil
}

func (s *IndexSet) index(id model.ObjectID) (*indexBinding, bool) {
	if s == nil {
		return nil, false
	}
	position, found := slices.BinarySearchFunc(s.indexes, id, func(binding indexBinding, id model.ObjectID) int {
		return cmp.Compare(binding.target.ObjectID, id)
	})
	if !found {
		return nil, false
	}
	return &s.indexes[position], true
}

func (s *IndexSet) WithBaseResolver(base txn.MutationResolver) txn.MutationResolver {
	if s != nil && s.shadow != nil {
		base = s.shadow.WithBaseResolver(base)
	}
	return indexMutationResolver{indexes: s, base: base}
}

func (s *IndexSet) withGeneration(plan *GenerationPlan) (*IndexSet, error) {
	if s == nil || plan == nil || plan.source.ObjectID != s.target.ObjectID || plan.source.Generation != s.target.Generation || plan.WorkspaceBytes() > math.MaxUint64-s.workspaceBytes {
		return nil, errIndexSetDescriptor
	}
	copy := *s
	copy.shadow = plan
	return &copy, nil
}

type indexMutationResolver struct {
	indexes *IndexSet
	base    txn.MutationResolver
}

func (r indexMutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.indexes == nil {
		return nil, storage.ErrInvalidTarget
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if binding, found := r.indexes.index(target.ObjectID); found && binding.target == target {
		return binding.normalizer, nil
	}
	if r.base == nil {
		return nil, storage.ErrInvalidTarget
	}
	return r.base.ResolveMutation(ctx, target)
}
