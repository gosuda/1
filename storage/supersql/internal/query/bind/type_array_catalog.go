package bind

import (
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (b *binder) declaredArrayType(element model.TypeLayout, parent model.ObjectID, pos int) (model.TypeID, error) {
	if b.catalog == nil {
		return 0, bindError("42704", "could not find array type for input data type", pos)
	}
	var found *model.Descriptor
	remaining := b.options.Limits.MaxOperands
	err := b.catalog.ScanNamespace(b.ctx, parent, model.ObjectType, func(published catalog.PublishedDescriptor) error {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if remaining == 0 || b.nodes >= b.options.Limits.MaxNodes {
			return bindError("54000", "array type catalog scan limit exceeded", pos)
		}
		remaining--
		b.nodes++
		descriptor := published.Descriptor
		if descriptor.State() != model.StatePublic {
			return nil
		}
		spec, err := b.descriptorSpec(descriptor)
		if err != nil {
			return err
		}
		if spec.Type == nil || spec.Type.Layout.Type.Storage != model.TypeArray {
			return nil
		}
		layout := spec.Type.Layout
		if len(layout.Children) != 1 || layout.Children[0].Type.ID != element.Type.ID {
			return nil
		}
		if layout.Type.ID != model.TypeID(descriptor.OID()) || !sameType(layout.Children[0], element) {
			return bindError("XX000", "array descriptor differs from element layout", pos)
		}
		if found != nil && found.ID() != descriptor.ID() {
			return bindError("XX000", "multiple declared array types for input data type", pos)
		}
		found = descriptor
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if found == nil {
		return 0, bindError("42704", "could not find array type for input data type", pos)
	}
	b.dependency(found, 0)
	return model.TypeID(found.OID()), nil
}
