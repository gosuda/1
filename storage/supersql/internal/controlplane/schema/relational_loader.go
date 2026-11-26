package schema

import (
	"context"
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type relationalCandidate struct {
	descriptor *model.Descriptor
	spec       model.DescriptorSpec
}

// LoadDeclarationRelationalConstraints links an unpublished table expansion to
// its snapshot-visible FK component. The returned support sets are read-only for
// this declaration; the seed remains the caller's foreground expansion.
func LoadDeclarationRelationalConstraints(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) ([]*IndexSet, error) {
	return loadRelationalIndexSets(ctx, snapshot, seed, encoder, expressions, limits)
}

// DeclarationRelationalInventoryRequired reports whether an admitted table must
// discover other tables to freeze its complete enforced foreign-key component.
func DeclarationRelationalInventoryRequired(descriptor *model.Descriptor) bool {
	table, ok := descriptor.Table()
	if !ok {
		return false
	}
	return slices.ContainsFunc(table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Enforced && constraint.Kind == model.ConstraintForeignKey
	})
}

// loadRelationalIndexSets discovers the complete enforced-FK component around
// seed from one catalog snapshot, then binds each component index against the
// same snapshot and codec.
func loadRelationalIndexSets(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) ([]*IndexSet, error) {
	if snapshot == nil || seed == nil || encoder == nil {
		return nil, errRelationalPolicy
	}
	seedSpec, _, err := seed.table.SpecWithinBytes(ctx, model.DescriptorLimits{
		MaxBytes: limits.MetadataBytes,
		MaxItems: max(limits.MaxColumns, len(seed.indexes)),
		MaxDepth: 64,
	})
	if err != nil || seedSpec.Table == nil {
		return nil, errors.Join(errRelationalPolicy, err)
	}
	needsLink := false
	for _, constraint := range seedSpec.Table.Constraints {
		relational := constraint.Enforced && (constraint.Kind == model.ConstraintForeignKey || constraint.Kind == model.ConstraintExclude)
		deferredKey := constraint.Enforced && constraint.Deferrable && (constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique)
		needsLink = needsLink || relational || deferredKey
	}
	if !needsLink {
		return []*IndexSet{seed}, ctx.Err()
	}
	if !DeclarationRelationalInventoryRequired(seed.table) {
		published, err := snapshot.Descriptor(ctx, seed.table.Name().Parent)
		if err != nil || published.Descriptor == nil || published.Descriptor.Kind() != model.ObjectSchema || published.Descriptor.State() != model.StatePublic {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		seed.schemaName = published.Descriptor.Name().Name
		if err := LinkRelationalConstraints(ctx, []*IndexSet{seed}, encoder, limits.MetadataBytes); err != nil {
			return nil, err
		}
		return []*IndexSet{seed}, ctx.Err()
	}
	remaining := limits.MetadataBytes
	fixed := uint64(unsafe.Sizeof(relationalCandidate{})) + uint64(unsafe.Sizeof((*IndexSet)(nil)))
	if fixed > remaining {
		return nil, errRelationalLimit
	}
	candidates := []relationalCandidate{{descriptor: seed.table, spec: seedSpec}}
	seen := map[model.ObjectID]struct{}{seed.target.ObjectID: {}}
	chargeDescriptor := func(descriptor *model.Descriptor) error {
		if descriptor == nil || descriptor.Kind() != model.ObjectTable {
			return errRelationalPolicy
		}
		if descriptor.State() != model.StatePublic {
			return nil
		}
		if remaining <= fixed {
			return errRelationalLimit
		}
		spec, used, err := descriptor.SpecWithinBytes(ctx, model.DescriptorLimits{MaxBytes: remaining - fixed, MaxItems: max(limits.MaxColumns, int((remaining-fixed)/uint64(unsafe.Sizeof(model.ConstraintDescriptor{}))+1)), MaxDepth: 64})
		if err != nil || spec.Table == nil || used > remaining-fixed {
			return errors.Join(errRelationalLimit, err)
		}
		remaining -= fixed + used
		candidates = append(candidates, relationalCandidate{descriptor: descriptor, spec: spec})
		seen[descriptor.ID()] = struct{}{}
		return nil
	}
	err = snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(table catalog.PublishedDescriptor) error {
			if _, exists := seen[table.Descriptor.ID()]; exists {
				return nil
			}
			return chargeDescriptor(table.Descriptor)
		})
	})
	if err != nil {
		return nil, err
	}
	included := map[model.ObjectID]bool{seed.target.ObjectID: true}
	for changed := true; changed; {
		changed = false
		for _, candidate := range candidates {
			for _, constraint := range candidate.spec.Table.Constraints {
				if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced {
					continue
				}
				if included[candidate.descriptor.ID()] && !included[constraint.ReferencedObject] {
					included[constraint.ReferencedObject] = true
					changed = true
				}
				if included[constraint.ReferencedObject] && !included[candidate.descriptor.ID()] {
					included[candidate.descriptor.ID()] = true
					changed = true
				}
			}
		}
	}
	sets := []*IndexSet{seed}
	for _, candidate := range candidates[1:] {
		if !included[candidate.descriptor.ID()] {
			continue
		}
		ids := slices.Clone(candidate.spec.Table.Indexes)
		slices.Sort(ids)
		plans := make([]*IndexPlan, len(ids))
		for i, id := range ids {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil || published.Descriptor == nil {
				return nil, errors.Join(errRelationalPolicy, err)
			}
			plans[i], err = NewIndexPlan(ctx, candidate.descriptor, published.Descriptor, encoder, expressions, limits)
			if err != nil {
				return nil, err
			}
		}
		set, err := NewIndexSet(ctx, candidate.descriptor, plans, limits)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set)
	}
	for _, set := range sets {
		schema, err := snapshot.Descriptor(ctx, set.table.Name().Parent)
		if err != nil || schema.Descriptor == nil || schema.Descriptor.Kind() != model.ObjectSchema || schema.Descriptor.State() != model.StatePublic {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		set.schemaName = schema.Descriptor.Name().Name
	}
	if err := LinkRelationalConstraints(ctx, sets, encoder, limits.MetadataBytes); err != nil {
		return nil, err
	}
	return sets, ctx.Err()
}
