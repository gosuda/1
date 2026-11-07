package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/partition"
)

func (b *binder) validatePartitionAttachment(ctx context.Context, parent, candidate *model.Descriptor, options CreateDeclarationOptions) error {
	if parent == nil || candidate == nil || options.Encoder == nil {
		return bindError("55000", "partition attachment requires key encoding", 0)
	}
	descendants, err := b.inheritanceDescriptors(parent, 0)
	if err != nil {
		return err
	}
	children := make([]*model.Descriptor, 0, len(descendants))
	for _, descriptor := range descendants {
		if descriptor.ID() == parent.ID() || descriptor.ID() == candidate.ID() {
			continue
		}
		b.dependency(descriptor, 0)
		table, ok := descriptor.Table()
		if !ok || table.PartitionBound == nil || table.PartitionState != model.PartitionAttached || !slices.Contains(table.Parents, parent.ID()) {
			continue
		}
		children = append(children, descriptor)
	}
	children = append(children, candidate)
	_, err = partition.NewRouter(ctx, partition.Options{Parent: parent, Children: children, Encoder: options.Encoder, Builtin: options.Bind.Builtin, Limits: partition.Limits{MaxChildren: options.Descriptors.MaxItems, MaxKeys: options.Descriptors.MaxItems, MaxBytes: options.Descriptors.MaxBytes}})
	return err
}
