package bind

import (
	"context"
	"errors"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func reindexPartitionHierarchy(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, selected model.ObjectID, versionReserve uint64, operation schema.SchemaJobOperation) ([]schema.JobDefinition, error) {
	if b == nil || snapshot == nil || table == nil || versionReserve == 0 || operation == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	metadata, ok := table.Table()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	if metadata.PartitionStrategy == 0 && metadata.PartitionBound == nil {
		return nil, nil
	}
	members := []*model.Descriptor{table}
	var err error
	if metadata.PartitionStrategy != 0 {
		members, err = b.inheritanceDescriptors(table, 0)
		if err != nil {
			return nil, err
		}
	}
	definitions := make([]schema.JobDefinition, 0, len(members))
	root := table.ID()
	for _, member := range members {
		spec, err := member.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		if member.ID() != root && (spec.Table.PartitionBound == nil || spec.Table.PartitionState != model.PartitionAttached) {
			return nil, model.ErrInvalidDescriptor
		}
		b.dependency(member, 0)
		if spec.Table.PartitionStrategy != 0 {
			continue
		}
		if member.State() != model.StatePublic || spec.Version > math.MaxUint64-versionReserve || spec.Physical.Generation == 0 || spec.Physical.Generation == math.MaxUint64 {
			return nil, model.ErrInvalidDescriptor
		}
		sourceIndexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
		if err != nil {
			return nil, err
		}
		selectedIndexes := make(map[model.ObjectID]bool)
		if selected != 0 {
			for _, index := range sourceIndexes {
				descends, err := partitionIndexDescendsFrom(ctx, snapshot, index, selected, options.Descriptors.MaxDepth, func(descriptor *model.Descriptor) {
					b.dependency(descriptor, 0)
				})
				if err != nil {
					return nil, err
				}
				if descends {
					selectedIndexes[index.ID()] = true
				}
			}
			if len(selectedIndexes) > 1 {
				return nil, model.ErrInvalidDescriptor
			}
			if len(selectedIndexes) == 0 {
				continue
			}
		}
		rebuildIndex := func(index *model.Descriptor) (bool, error) {
			metadata, ok := index.Index()
			if !ok {
				return false, model.ErrInvalidDescriptor
			}
			chosen := selectedIndexes[index.ID()]
			exclusion := slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
				return constraint.Kind == model.ConstraintExclude && constraint.Index == index.ID()
			})
			if operation == schema.SchemaReindex {
				if selected != 0 && chosen && exclusion {
					return false, bindError("0A000", "concurrent partition reindex does not support exclusion indexes", 0)
				}
				rebuild := selected != 0 && chosen || selected == 0 && metadata.Valid() && !exclusion
				if rebuild && (!metadata.Ready() || !metadata.Live()) {
					return false, bindError("55000", "partition index is not ready for reindex", 0)
				}
				return rebuild, nil
			}
			return selected == 0 || chosen, nil
		}
		rebuilds := 0
		for _, index := range sourceIndexes {
			rebuild, err := rebuildIndex(index)
			if err != nil {
				return nil, err
			}
			if rebuild {
				rebuilds++
			}
		}
		if rebuilds == 0 {
			continue
		}
		spec.Version += versionReserve
		spec.Physical.Generation++
		target, err := rebindTargetTableExpressions(ctx, member, &spec, snapshot, options)
		if err != nil {
			return nil, err
		}
		targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, sourceIndexes, target, 0)
		if err != nil {
			return nil, err
		}
		for i, sourceIndex := range sourceIndexes {
			rebuild, err := rebuildIndex(sourceIndex)
			if err != nil {
				return nil, err
			}
			if rebuild {
				continue
			}
			indexSpec, err := targetIndexes[i].SpecWithin(ctx, options.Descriptors)
			if err != nil || indexSpec.Index == nil {
				return nil, errors.Join(err, model.ErrInvalidDescriptor)
			}
			indexSpec.Physical = sourceIndex.Physical()
			targetIndexes[i], err = model.NewDescriptor(ctx, indexSpec, options.Descriptors)
			if err != nil {
				return nil, err
			}
		}
		for i, index := range targetIndexes {
			rebuild, err := rebuildIndex(sourceIndexes[i])
			if err != nil {
				return nil, err
			}
			if !rebuild {
				continue
			}
			indexSpec, err := index.SpecWithin(ctx, options.Descriptors)
			if err != nil || indexSpec.Index == nil {
				return nil, errors.Join(err, model.ErrInvalidDescriptor)
			}
			nextMetadata := *indexSpec.Index
			nextMetadata.Invalid, nextMetadata.NotReady, nextMetadata.NotLive = false, false, false
			indexSpec.Index = &nextMetadata
			targetIndexes[i], err = model.NewDescriptor(ctx, indexSpec, options.Descriptors)
			if err != nil {
				return nil, err
			}
		}
		definitions = append(definitions, schema.JobDefinition{
			Operation: operation, SourceTable: member, TargetTable: target,
			SourceIndexes: sourceIndexes, TargetIndexes: targetIndexes,
		})
	}
	return definitions, ctx.Err()
}

func partitionIndexDescendsFrom(ctx context.Context, snapshot CatalogSnapshot, index *model.Descriptor, ancestor model.ObjectID, maximumDepth int, visit func(*model.Descriptor)) (bool, error) {
	if index == nil || ancestor == 0 || maximumDepth <= 0 {
		return false, model.ErrInvalidDescriptor
	}
	current := index
	seen := make(map[model.ObjectID]bool)
	for depth := 0; depth <= maximumDepth; depth++ {
		if visit != nil {
			visit(current)
		}
		if current.ID() == ancestor {
			return true, nil
		}
		if seen[current.ID()] {
			return false, model.ErrInvalidDescriptor
		}
		seen[current.ID()] = true
		metadata, ok := current.Index()
		if !ok {
			return false, model.ErrInvalidDescriptor
		}
		if metadata.ParentIndex == 0 {
			return false, ctx.Err()
		}
		published, err := snapshot.Descriptor(ctx, metadata.ParentIndex)
		if err != nil {
			return false, err
		}
		current = published.Descriptor
		if current == nil || current.Kind() != model.ObjectIndex || current.State() != model.StatePublic {
			return false, model.ErrInvalidDescriptor
		}
	}
	return false, model.ErrDescriptorLimit
}
