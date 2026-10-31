package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func addInheritanceDropUpdates(ctx context.Context, command *BoundObjectCommand, snapshot CatalogSnapshot, limits model.DescriptorLimits) error {
	if command == nil {
		return model.ErrInvalidDescriptor
	}
	dropped := make(map[model.ObjectID]bool, len(command.drops))
	for _, descriptor := range command.drops {
		dropped[descriptor.ID()] = true
	}
	type parentChange struct {
		base     *model.Descriptor
		children map[model.ObjectID]bool
	}
	changes := make(map[model.ObjectID]*parentChange)
	for _, child := range command.drops {
		table, ok := child.Table()
		if !ok || len(table.Parents) == 0 || table.PartitionState == model.PartitionDetachPending {
			continue
		}
		for _, parentID := range table.Parents {
			if dropped[parentID] {
				continue
			}
			change := changes[parentID]
			if change == nil {
				published, err := snapshot.Descriptor(ctx, parentID)
				if err != nil {
					return err
				}
				change = &parentChange{base: published.Descriptor, children: make(map[model.ObjectID]bool)}
				changes[parentID] = change
			}
			change.children[child.ID()] = true
		}
	}
	ids := make([]model.ObjectID, 0, len(changes))
	for id := range changes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		change := changes[id]
		spec, err := change.base.SpecWithin(ctx, limits)
		if err != nil {
			return err
		}
		if spec.Table == nil || uint32(len(change.children)) > spec.Table.InheritanceChildren {
			return model.ErrInvalidDescriptor
		}
		spec.Table = cloneTableMetadata(spec.Table)
		spec.Table.InheritanceChildren -= uint32(len(change.children))
		if spec.Table.PartitionDefault != 0 && change.children[spec.Table.PartitionDefault] {
			spec.Table.PartitionDefault = 0
		}
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, limits)
		if err != nil {
			return err
		}
		if err := change.base.ValidateSuccessor(ctx, next); err != nil {
			return err
		}
		command.updates = append(command.updates, ObjectMutation{Base: change.base, Next: next})
	}
	return nil
}
