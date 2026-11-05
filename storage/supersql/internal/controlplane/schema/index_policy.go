package schema

import (
	"context"
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errIndexPolicyOptions  = &model.Error{Code: "22023", Message: "invalid index policy provider options", Cause: storage.ErrInvalidLayout}
	errIndexPolicyResolver = &model.Error{Code: "55000", Message: "index job has no mutation policy for target", Cause: storage.ErrInvalidTarget}
)

type IndexWritePolicy struct {
	Table      storage.ObjectRef
	Rows       *txn.RowNormalizer
	Indexes    *IndexSet
	Generation *GenerationPlan
	Keys       KeyCodec
	Resolver   txn.MutationResolver
}

type IndexPolicies interface {
	Policy(context.Context, Job) (IndexWritePolicy, error)
}

// IndexExpressionFactory borrows snapshot only through policy construction.
// Compiled ValueExpressions must retain no snapshot or factory-owned state.
type IndexExpressionFactory func(context.Context, *catalog.CatalogSnapshot) (IndexExpressions, error)

type ColumnExpressionFactory func(context.Context, *catalog.CatalogSnapshot) (ColumnExpressions, error)

// KeyCodec provides symmetric key semantics for one immutable catalog snapshot.
type KeyCodec interface {
	storage.KeyEncoder
	storage.KeyDecoder
}

// KeyEncoderFactory returns an owned codec and must not retain the snapshot.
type KeyEncoderFactory func(context.Context, *catalog.CatalogSnapshot) (KeyCodec, error)

type IndexPolicyOptions struct {
	Catalog     *catalog.Store
	Views       catalog.CurrentReadViews
	Jobs        *JobStore
	Encoders    KeyEncoderFactory
	Expressions IndexExpressionFactory
	Columns     ColumnExpressionFactory
	Generations GenerationExpressionFactory
	Limits      IndexLimits
	Base        txn.MutationResolver
}

// IndexPolicyProvider borrows its collaborators. Returned policies own their
// metadata and retain no read view; bound programs obey IndexExpressions ownership.
// Callers must hold schema intents while using the returned mutation resolver.
type IndexPolicyProvider struct {
	catalog     *catalog.Store
	views       catalog.CurrentReadViews
	jobs        *JobStore
	encoders    KeyEncoderFactory
	expressions IndexExpressionFactory
	columns     ColumnExpressionFactory
	generations GenerationExpressionFactory
	limits      IndexLimits
	current     *catalog.CurrentDescriptors
	base        txn.MutationResolver
	internal    [4]storage.ObjectRef
}

func NewIndexPolicyProvider(ctx context.Context, options IndexPolicyOptions) (*IndexPolicyProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Catalog == nil || mutationPolicyNil(options.Views) || options.Jobs == nil || options.Jobs.normalizer == nil || options.Encoders == nil {
		return nil, errIndexPolicyOptions
	}
	if err := validateIndexLimits(options.Limits); err != nil {
		return nil, err
	}
	current, err := catalog.NewCurrentDescriptors(options.Catalog, options.Views)
	if err != nil {
		return nil, err
	}
	budget := indexSetBudget{remaining: options.Limits.MetadataBytes}
	if err := budget.charge(1, uint64(unsafe.Sizeof(IndexPolicyProvider{}))+uint64(unsafe.Sizeof(indexCatalogResolver{}))); err != nil {
		return nil, err
	}
	// Catalog collections have two bytea columns and one key. Admit their
	// declarations and normalizer copies before requesting owned layouts.
	catalogMetadata := uint64(unsafe.Sizeof(storage.ObjectLayout{})) +
		2*uint64(unsafe.Sizeof(storage.ColumnLayout{})) +
		uint64(unsafe.Sizeof(txn.RowNormalizer{})) +
		2*uint64(unsafe.Sizeof(txn.ColumnRule{})) +
		uint64(unsafe.Sizeof(storage.KeyColumn{}))
	if err := budget.charge(3, catalogMetadata); err != nil {
		return nil, err
	}
	layouts := options.Catalog.Layouts()
	if len(layouts) != 3 {
		return nil, errIndexPolicyOptions
	}
	metadata := &indexCatalogResolver{base: options.Base}
	if mutationPolicyNil(metadata.base) {
		metadata.base = nil
	}
	provider := &IndexPolicyProvider{
		catalog: options.Catalog, views: options.Views, jobs: options.Jobs,
		encoders: options.Encoders, expressions: options.Expressions, columns: options.Columns, generations: options.Generations, limits: options.Limits,
		current: current,
	}
	bytea := model.Type{ID: 17, Storage: model.TypeBytes}
	for i, layout := range layouts {
		if len(layout.Columns) != 2 || layout.Target.ObjectID != catalog.NamespaceObjectID+model.ObjectID(i) || layout.Target.ObjectID == options.Jobs.Target().ObjectID {
			return nil, errIndexPolicyOptions
		}
		var rules [2]txn.ColumnRule
		for column, declaration := range layout.Columns {
			if declaration.ID != model.ColumnID(column+1) || declaration.Type != bytea || declaration.Nullable || declaration.HasMissingValue || len(declaration.Children) != 0 {
				return nil, errIndexPolicyOptions
			}
			rules[column].Layout = declaration
		}
		rules[0].KeyPosition = 1
		normalizer, err := txn.NewRowNormalizer(ctx, layout.Target, rules[:], max(options.Limits.MetadataBytes, options.Limits.EntryBytes))
		if err != nil {
			return nil, err
		}
		metadata.bindings[i] = indexCatalogBinding{target: layout.Target, normalizer: normalizer}
		provider.internal[i] = layout.Target
	}
	provider.internal[len(layouts)] = options.Jobs.Target()
	provider.base = options.Jobs.WithBaseResolver(metadata)
	return provider, nil
}

// RequiresDeleteRangeExpansion prevents raw range tombstones from bypassing
// user-table index, constraint and trigger policies.
func (p *IndexPolicyProvider) RequiresDeleteRangeExpansion(ctx context.Context, target storage.ObjectRef) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p == nil {
		return false, errIndexPolicyOptions
	}
	for _, internal := range p.internal {
		if target.ObjectID == internal.ObjectID {
			return false, nil
		}
	}
	return true, nil
}

func (p *IndexPolicyProvider) keyEncoder(ctx context.Context, snapshot *catalog.CatalogSnapshot) (KeyCodec, error) {
	if p == nil || p.encoders == nil || snapshot == nil {
		return nil, errIndexPolicyOptions
	}
	encoder, err := p.encoders(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	if mutationPolicyNil(encoder) {
		return nil, errIndexPolicyOptions
	}
	return encoder, nil
}

func (p *IndexPolicyProvider) GenerationPlan(ctx context.Context, snapshot *catalog.CatalogSnapshot, definition JobDefinition) (*GenerationPlan, KeyCodec, error) {
	if p == nil || snapshot == nil {
		return nil, nil, errIndexPolicyOptions
	}
	encoder, err := p.keyEncoder(ctx, snapshot)
	if err != nil {
		return nil, nil, err
	}
	var indexExpressions IndexExpressions
	if p.expressions != nil {
		indexExpressions, err = p.expressions(ctx, snapshot)
		if err != nil {
			return nil, nil, err
		}
		if mutationPolicyNil(indexExpressions) {
			return nil, nil, errIndexPolicyOptions
		}
	}
	var generationExpressions GenerationExpressions
	if p.generations != nil {
		generationExpressions, err = p.generations(ctx, snapshot, definition)
		if err != nil {
			return nil, nil, err
		}
		if mutationPolicyNil(generationExpressions) {
			return nil, nil, errIndexPolicyOptions
		}
	}
	plan, err := NewGenerationPlan(ctx, definition, encoder, generationExpressions, indexExpressions, p.limits)
	if err != nil {
		return nil, nil, err
	}
	return plan, encoder, nil
}

func (p *IndexPolicyProvider) Policy(ctx context.Context, job Job) (IndexWritePolicy, error) {
	return p.policy(ctx, job.TableID, storage.ObjectRef{}, &job, nil)
}

// TablePolicy prepares foreground maintenance for exactly the bound table
// identity. A policy transition requires rebinding, not silently adopting a new
// descriptor. The returned resolver rechecks current policy under schema intent.
func (p *IndexPolicyProvider) TablePolicy(ctx context.Context, target storage.ObjectRef) (IndexWritePolicy, error) {
	if err := target.Validate(); err != nil {
		return IndexWritePolicy{}, err
	}
	return p.policy(ctx, target.ObjectID, target, nil, nil)
}

func (p *IndexPolicyProvider) policy(ctx context.Context, tableID model.ObjectID, expected storage.ObjectRef, job *Job, intent *model.Descriptor) (policy IndexWritePolicy, err error) {
	if err := ctx.Err(); err != nil {
		return IndexWritePolicy{}, err
	}
	if p == nil || p.catalog == nil || p.jobs == nil {
		return IndexWritePolicy{}, errIndexPolicyOptions
	}
	if job != nil {
		if err := validJob(*job, p.jobs.options.RecordBytes); err != nil {
			return IndexWritePolicy{}, err
		}
		if job.TableID != tableID {
			return IndexWritePolicy{}, ErrJobTransition
		}
	}
	for _, target := range p.internal {
		if target.ObjectID == tableID || job != nil && job.Kind == AddIndexJob && target.ObjectID == job.MutationID {
			return IndexWritePolicy{}, errIndexPolicyOptions
		}
	}
	view, err := p.views.CurrentReadView(ctx)
	if mutationPolicyNil(view) {
		if err != nil {
			return IndexWritePolicy{}, err
		}
		return IndexWritePolicy{}, storage.ErrSnapshotNotClosed
	}
	defer func() {
		err = errors.Join(err, view.Close())
		if err != nil {
			policy = IndexWritePolicy{}
		}
	}()
	if err != nil {
		return IndexWritePolicy{}, err
	}
	snapshot, err := p.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		return IndexWritePolicy{}, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	encoder, err := p.keyEncoder(ctx, snapshot)
	if err != nil {
		return IndexWritePolicy{}, err
	}
	if job == nil {
		active, activeErr := p.jobs.Active(ctx, view, tableID)
		if activeErr == nil {
			job = &active
		} else if !indexRowAbsent(activeErr) {
			return IndexWritePolicy{}, activeErr
		}
	}
	var expectedState model.SchemaState
	if job != nil && job.Kind == AddIndexJob {
		expectedState, err = indexJobPolicyState(job.State)
		if err != nil {
			return IndexWritePolicy{}, err
		}
	}
	var expressions IndexExpressions
	if p.expressions != nil {
		expressions, err = p.expressions(ctx, snapshot)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		if mutationPolicyNil(expressions) {
			return IndexWritePolicy{}, errIndexPolicyOptions
		}
	}
	table, err := snapshot.Descriptor(ctx, tableID)
	if err != nil {
		return IndexWritePolicy{}, err
	}
	if table.Descriptor == nil || !tableShapedKind(table.Descriptor.Kind()) || table.Descriptor.State() != model.StatePublic {
		return IndexWritePolicy{}, errMutationDescriptorChanged
	}
	physical := table.Descriptor.Physical()
	current := storage.ObjectRef{ObjectID: table.Descriptor.ID(), Generation: physical.Generation, DescriptorVersion: table.Descriptor.Version(), EncodingVersion: physical.EncodingVersion}
	if expected.ObjectID != 0 && current != expected {
		return IndexWritePolicy{}, errMutationDescriptorChanged
	}
	if intent != nil {
		if _, err := NewIndexPlan(ctx, table.Descriptor, intent, encoder, expressions, p.limits); err != nil {
			return IndexWritePolicy{}, err
		}
	}
	descriptorLimits := model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64}
	spec, err := table.Descriptor.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return IndexWritePolicy{}, indexPlanLimitError(err)
	}
	if spec.Table == nil {
		return IndexWritePolicy{}, invalidIndexPlan()
	}
	if len(spec.Table.Columns) > p.limits.MaxColumns || len(spec.Table.PrimaryKey) > p.limits.MaxColumns {
		return IndexWritePolicy{}, errIndexSetLimit
	}
	ids := spec.Table.Indexes
	slices.Sort(ids)
	for i, id := range ids {
		reserved := id == tableID || id == p.jobs.Target().ObjectID || id >= catalog.NamespaceObjectID
		if reserved || (i > 0 && id == ids[i-1]) {
			return IndexWritePolicy{}, invalidIndexPlan()
		}
	}
	linked := true
	if job != nil && job.Kind == AddIndexJob {
		_, linked = slices.BinarySearch(ids, job.MutationID)
	}
	if !linked {
		if job.State != JobPending {
			return IndexWritePolicy{}, errMutationDescriptorChanged
		}
		// An unlinked intent is a real StateAbsent descriptor, not a missing
		// catalog row. It becomes a maintenance plan only after publication.
		intent, err := snapshot.Descriptor(ctx, job.MutationID)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		if intent.Descriptor == nil || intent.Descriptor.Kind() != model.ObjectIndex || intent.Descriptor.State() != expectedState {
			return IndexWritePolicy{}, errMutationDescriptorChanged
		}
		intentSpec, err := intent.Descriptor.SpecWithin(ctx, descriptorLimits)
		if err != nil {
			return IndexWritePolicy{}, indexPlanLimitError(err)
		}
		if intentSpec.Index == nil || intentSpec.Index.Table != tableID {
			return IndexWritePolicy{}, invalidIndexPlan()
		}
	}
	budget := indexSetBudget{remaining: p.limits.MetadataBytes}
	if err := budget.charge(uint64(len(ids)), uint64(unsafe.Sizeof((*IndexPlan)(nil)))); err != nil {
		return IndexWritePolicy{}, err
	}
	plans := make([]*IndexPlan, len(ids))
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return IndexWritePolicy{}, err
		}
		index, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		if job != nil && job.Kind == AddIndexJob && id == job.MutationID {
			if index.Descriptor == nil || index.Descriptor.State() != expectedState {
				return IndexWritePolicy{}, errMutationDescriptorChanged
			}
		}
		limits := p.limits
		limits.MetadataBytes = budget.remaining
		plan, err := NewIndexPlan(ctx, table.Descriptor, index.Descriptor, encoder, expressions, limits)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		if err := budget.charge(1, plan.metadataBytes()); err != nil {
			return IndexWritePolicy{}, err
		}
		plans[i] = plan
	}
	indexes, err := NewIndexSet(ctx, table.Descriptor, plans, p.limits)
	if err != nil {
		return IndexWritePolicy{}, err
	}
	rowPolicy, err := p.tableNormalizer(ctx, snapshot, table.Descriptor)
	if err != nil {
		return IndexWritePolicy{}, err
	}
	base := tablePolicyBinding{target: current, normalizer: rowPolicy, fallback: p.base}
	checked, err := NewCheckedMutationResolver(p.current, indexes.WithBaseResolver(base), p.internal[:], len(p.internal))
	if err != nil {
		return IndexWritePolicy{}, err
	}
	var resolver txn.MutationResolver = checked
	var generation *GenerationPlan
	if job != nil && job.Kind == RewriteGenerationJob && (job.State == JobWriteOnly || job.State == JobBackfilling || job.State == JobValidating) {
		definition, err := p.jobs.Definition(ctx, view, *job)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		var generationExpressions GenerationExpressions
		if p.generations != nil {
			generationExpressions, err = p.generations(ctx, snapshot, definition)
			if err != nil {
				return IndexWritePolicy{}, err
			}
		} else if p.columns != nil {
			columnExpressions, columnErr := p.columns(ctx, snapshot)
			if columnErr != nil {
				return IndexWritePolicy{}, columnErr
			}
			if mutationPolicyNil(columnExpressions) {
				return IndexWritePolicy{}, errIndexPolicyOptions
			}
			generationExpressions = generationColumnsOnly{ColumnExpressions: columnExpressions}
		}
		if mutationPolicyNil(generationExpressions) {
			return IndexWritePolicy{}, errIndexPolicyOptions
		}
		generation, err = NewGenerationPlan(ctx, definition, encoder, generationExpressions, expressions, p.limits)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		indexes, err = indexes.withGeneration(generation)
		if err != nil {
			return IndexWritePolicy{}, err
		}
		resolver = generation.WithBaseResolver(checked)
	}
	sets, relationalErr := loadRelationalIndexSets(ctx, snapshot, indexes, encoder, expressions, p.limits)
	if relationalErr != nil {
		return IndexWritePolicy{}, relationalErr
	}
	var composed txn.MutationResolver = p.base
	first := 0
	if job != nil {
		composed = resolver
		first = 1
	}
	for _, set := range sets[first:] {
		rows := rowPolicy
		if set.target.ObjectID != indexes.target.ObjectID {
			rows, relationalErr = p.tableNormalizer(ctx, snapshot, set.table)
			if relationalErr != nil {
				return IndexWritePolicy{}, relationalErr
			}
		}
		composed = set.WithBaseResolver(tablePolicyBinding{target: set.target, normalizer: rows, fallback: composed})
	}
	checked, relationalErr = NewCheckedMutationResolver(p.current, composed, p.internal[:], len(p.internal))
	if relationalErr != nil {
		return IndexWritePolicy{}, relationalErr
	}
	resolver = checked
	return IndexWritePolicy{Table: indexes.target, Rows: rowPolicy, Indexes: indexes, Generation: generation, Keys: encoder, Resolver: resolver}, nil
}

func indexJobPolicyState(state JobState) (model.SchemaState, error) {
	switch state {
	case JobPending:
		return model.StateAbsent, nil
	case JobDeleteOnly, JobReverting:
		return model.StateDeleteOnly, nil
	case JobWriteOnly, JobBackfilling, JobValidating:
		return model.StateWriteOnly, nil
	case JobPublic:
		return model.StatePublic, nil
	default:
		return 0, ErrJobTransition
	}
}

type indexCatalogBinding struct {
	target     storage.ObjectRef
	normalizer *txn.RowNormalizer
}

type indexCatalogResolver struct {
	bindings [3]indexCatalogBinding
	base     txn.MutationResolver
}

func (r *indexCatalogResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, binding := range r.bindings {
		if target.ObjectID != binding.target.ObjectID {
			continue
		}
		if target != binding.target {
			return nil, storage.ErrInvalidTarget
		}
		return binding.normalizer, nil
	}
	if r.base == nil {
		return nil, errIndexPolicyResolver
	}
	return r.base.ResolveMutation(ctx, target)
}
