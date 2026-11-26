package bind

import "github.com/gosuda/supersql-alpha/internal/model"

// declarationScope is private to expression compilation before publication.
// It never reports private objects as catalog publications.
type declarationScope struct {
	objects []*model.Descriptor
}

func (b *binder) declarationDescriptor(id model.ObjectID) (*model.Descriptor, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if scope := b.options.declaration; scope != nil {
		for _, descriptor := range scope.objects {
			if descriptor.ID() == id {
				return descriptor, nil
			}
		}
	}
	published, err := b.catalog.Descriptor(b.ctx, id)
	if err != nil {
		return nil, err
	}
	return published.Descriptor, nil
}

func (b *binder) declarationRelation(parts []string) (*model.Descriptor, *model.Descriptor, error) {
	if b.options.declaration == nil {
		return ResolveRelationName(b.ctx, b.catalog, b.options.SearchPath, parts)
	}
	if len(parts) == 0 || len(parts) > 2 {
		return nil, nil, bindError("42P01", "object does not exist", 0)
	}
	path := b.options.SearchPath
	if len(parts) == 2 {
		path = []string{parts[0]}
	}
	for _, schema := range path {
		namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for _, descriptor := range b.options.declaration.objects {
			name := descriptor.Name()
			if name.Parent == namespace.Descriptor.ID() && name.Name == parts[len(parts)-1] && defaultRelationKind(name.Kind) {
				return descriptor, namespace.Descriptor, nil
			}
		}
		descriptor, parent, err := ResolveRelationName(b.ctx, b.catalog, []string{schema}, parts[len(parts)-1:])
		if err == nil {
			return descriptor, parent, nil
		}
		if sql, ok := err.(*model.Error); !ok || sql.Code != "42P01" {
			return nil, nil, err
		}
	}
	return nil, nil, bindError("42P01", "object does not exist", 0)
}
