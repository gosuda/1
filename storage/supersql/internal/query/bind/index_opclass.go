package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func indexOperatorClass(typ model.Type, nodes []*zpgparse.Node) (model.OID, error) {
	if len(nodes) == 0 || len(nodes) > 2 {
		return 0, bindError("42601", "operator class requires an optionally qualified name", 0)
	}
	parts, err := names(nodes)
	if err != nil {
		return 0, err
	}
	if len(parts) == 2 && parts[0] != "pg_catalog" {
		return 0, bindError("42704", "operator class does not exist for access method btree", 0)
	}
	class, found := builtin.LookupOperatorClassName("btree", parts[len(parts)-1])
	if !found {
		return 0, bindError("42704", "operator class does not exist for access method btree", 0)
	}
	if err := builtin.ValidateOperatorClassInput(typ, class.ID); err != nil {
		return 0, err
	}
	return class.ID, nil
}

func (b *binder) indexOperatorClassType(layout model.TypeLayout) (model.Type, error) {
	for depth := 0; depth <= int(b.options.Limits.MaxDepth); depth++ {
		if _, known := builtin.LookupType(layout.Type.ID); known {
			return layout.Type, nil
		}
		if informationType, known := b.options.CatalogRelations.ResolveInformationSchemaType(layout.Type.ID); known {
			switch informationType.Kind {
			case model.TypeKindDomain:
				layout = informationType.BaseLayout
				continue
			case model.TypeKindArray:
				layout.Type.ID = 2277
				return layout.Type, nil
			default:
				return layout.Type, nil
			}
		}
		descriptor, err := b.findNamedType(layout.Type.ID, 0)
		if err != nil || descriptor == nil {
			if err != nil {
				return model.Type{}, err
			}
			return layout.Type, nil
		}
		spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
		if err != nil {
			return model.Type{}, err
		}
		if spec.Type == nil {
			return model.Type{}, model.ErrInvalidDescriptor
		}
		switch spec.Type.DeclaredKind() {
		case model.TypeKindDomain:
			layout = spec.Type.BaseLayout
		case model.TypeKindEnum:
			layout.Type.ID = 3500
			return layout.Type, nil
		case model.TypeKindComposite:
			layout.Type.ID = 2249
			return layout.Type, nil
		case model.TypeKindArray:
			layout.Type.ID = 2277
			return layout.Type, nil
		case model.TypeKindRange:
			layout.Type.ID = 3831
			return layout.Type, nil
		case model.TypeKindMultirange:
			layout.Type.ID = 4537
			return layout.Type, nil
		default:
			return layout.Type, nil
		}
	}
	return model.Type{}, bindError("54000", "domain base chain exceeds limit", 0)
}
