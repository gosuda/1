package bind

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errUnnestTypeFound = errors.New("unnest composite type found")

func (b *binder) findNamedType(id model.TypeID, pos int) (*model.Descriptor, error) {
	if b.catalog == nil {
		return nil, nil
	}
	if descriptor, cached := b.namedTypes[id]; cached {
		return descriptor, nil
	}
	var found, schema *model.Descriptor
	remaining := b.options.Limits.MaxOperands
	visit := func() error {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if remaining == 0 || b.nodes >= b.options.Limits.MaxNodes {
			return bindError("54000", "composite type catalog scan limit exceeded", pos)
		}
		remaining--
		b.nodes++
		return nil
	}
	err := b.catalog.ScanNamespace(b.ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		if err := visit(); err != nil {
			return err
		}
		if namespace.Descriptor.State() != model.StatePublic {
			return nil
		}
		return b.catalog.ScanNamespace(b.ctx, namespace.Descriptor.ID(), model.ObjectType, func(published catalog.PublishedDescriptor) error {
			if err := visit(); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor.State() != model.StatePublic || model.TypeID(descriptor.OID()) != id {
				return nil
			}
			found, schema = descriptor, namespace.Descriptor
			return errUnnestTypeFound
		})
	})
	if !unnestTypeFound(err) {
		if err != nil {
			return nil, err
		}
		if b.namedTypes == nil {
			b.namedTypes = make(map[model.TypeID]*model.Descriptor)
		}
		b.namedTypes[id] = nil
		return nil, nil
	}
	b.dependency(schema, 0)
	b.dependency(found, 0)
	if b.namedTypes == nil {
		b.namedTypes = make(map[model.TypeID]*model.Descriptor)
	}
	b.namedTypes[id] = found
	return found, nil
}

func unnestTypeFound(err error) bool {
	if err == errUnnestTypeFound {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !unnestTypeFound(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return unnestTypeFound(cause)
	}
	return false
}

func (b *binder) unnestNamedColumns(s *scope, element model.TypeLayout, pos int) ([]column, error) {
	fields, err := b.namedCompositeFields(element, pos)
	if err != nil {
		return nil, err
	}
	columns, err := b.unnestColumnBuffer(len(fields), pos)
	if err != nil {
		return nil, err
	}
	for i, field := range fields {
		slot, err := b.slot(s, field.Layout, true, 0, 0)
		if err != nil {
			return nil, err
		}
		columns[i].ResultColumn = ResultColumn{Name: field.Name, Layout: field.Layout, Nullable: true, Slot: slot}
	}
	return columns, nil
}

func (b *binder) namedCompositeFields(element model.TypeLayout, pos int) ([]model.ColumnDescriptor, error) {
	descriptor, err := b.findNamedType(element.Type.ID, pos)
	if err != nil {
		return nil, err
	}
	if descriptor == nil {
		return nil, bindError("42704", "composite type does not exist", pos)
	}
	return b.compositeFields(descriptor, element, pos)
}

func (b *binder) compositeFields(descriptor *model.Descriptor, element model.TypeLayout, pos int) ([]model.ColumnDescriptor, error) {
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return nil, err
	}
	if spec.Type == nil || spec.Type.Layout.Type.ID != element.Type.ID || spec.Type.Layout.Type.Storage != model.TypeComposite {
		return nil, bindError("XX000", "composite type descriptor has no matching layout", pos)
	}
	fields := spec.Type.Fields[:0]
	for _, field := range spec.Type.Fields {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		if field.State != model.StatePublic {
			continue
		}
		index := len(fields)
		// Physical children contain public fields only, in descriptor order.
		if index >= len(element.Children) || !sameType(field.Layout, element.Children[index]) {
			return nil, bindError("XX000", "composite fields differ from element layout", pos)
		}
		fields = append(fields, field)
		b.dependency(descriptor, field.ID)
	}
	if len(fields) != len(element.Children) {
		return nil, bindError("XX000", "composite fields differ from element layout", pos)
	}
	clear(spec.Type.Fields[len(fields):])
	return fields, nil
}
