package schema

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type tablePolicyBinding struct {
	target     storage.ObjectRef
	normalizer *txn.RowNormalizer
	fallback   txn.MutationResolver
}

func (r tablePolicyBinding) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target == r.target {
		return r.normalizer, nil
	}
	return r.fallback.ResolveMutation(ctx, target)
}
func (p *IndexPolicyProvider) tableNormalizer(ctx context.Context, snapshot *catalog.CatalogSnapshot, descriptor *model.Descriptor) (*txn.RowNormalizer, error) {
	encoder, err := p.keyEncoder(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	var expressions ColumnExpressions
	if p.columns != nil {
		expressions, err = p.columns(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		if mutationPolicyNil(expressions) {
			return nil, errColumnExpressions
		}
	}
	return NewTablePolicy(ctx, TablePolicyOptions{Descriptor: descriptor, Encoder: encoder, Expressions: expressions, MetadataBytes: p.limits.MetadataBytes, RowBytes: p.limits.EntryBytes, MaxColumns: p.limits.MaxColumns})
}

// ResolveMutation compiles only the requested target. Transaction target caching
// owns its bounded retention; no scan-sized or unbounded global policy cache exists.
// The caller holds schema intent through transaction completion.
func (p *IndexPolicyProvider) ResolveMutation(ctx context.Context, target storage.ObjectRef) (normalizer *txn.RowNormalizer, err error) {
	if p == nil {
		return nil, errIndexPolicyOptions
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, errors.Join(errMutationDescriptorChanged, err)
	}
	for _, internal := range p.internal {
		if internal.ObjectID != target.ObjectID {
			continue
		}
		if internal != target {
			return nil, errMutationDescriptorChanged
		}
		return p.base.ResolveMutation(ctx, target)
	}
	view, err := p.views.CurrentReadView(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	snapshot, err := p.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	published, err := snapshot.Descriptor(ctx, target.ObjectID)
	if err != nil {
		return nil, errors.Join(errMutationDescriptorChanged, err)
	}
	descriptor := published.Descriptor
	if err := validateMutationDescriptor(descriptor, target); err != nil {
		return nil, err
	}
	if tableShapedKind(descriptor.Kind()) {
		return p.tableNormalizer(ctx, snapshot, descriptor)
	}
	spec, err := descriptor.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: p.limits.MaxColumns, MaxDepth: 64})
	if err != nil {
		return nil, err
	}
	table, err := snapshot.Descriptor(ctx, spec.Index.Table)
	if err != nil {
		return nil, err
	}
	if table.Descriptor.State() != model.StatePublic {
		return nil, errMutationDescriptorChanged
	}
	encoder, err := p.keyEncoder(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	var expressions IndexExpressions
	if p.expressions != nil {
		expressions, err = p.expressions(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		if mutationPolicyNil(expressions) {
			return nil, errIndexPolicyOptions
		}
	}
	plan, err := NewIndexPlan(ctx, table.Descriptor, descriptor, encoder, expressions, p.limits)
	if err != nil {
		return nil, err
	}
	budget := indexSetBudget{remaining: p.limits.MetadataBytes}
	binding, err := newIndexBinding(ctx, plan, p.limits, &budget)
	if err != nil {
		return nil, err
	}
	return binding.normalizer, nil
}
