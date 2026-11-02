package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) canCoerceViaIO(source, target model.TypeLayout, context ir.CastContext) bool {
	from, sourceKnown := b.lookupTypeIO(source)
	to, targetKnown := b.lookupTypeIO(target)
	if !sourceKnown || !targetKnown || from.Output == 0 || to.Input == 0 {
		return false
	}
	sourceCategory := b.typeCategory(source)
	targetCategory := b.typeCategory(target)
	if context == ir.ExplicitCast {
		return sourceCategory == 'S' || targetCategory == 'S'
	}
	return context == ir.AssignmentCast && targetCategory == 'S'
}

func (b *binder) typeCategory(layout model.TypeLayout) byte {
	if typ, ok := builtin.LookupType(layout.Type.ID); ok {
		return typ.Category
	}
	if typ, ok := b.options.CatalogRelations.ResolveInformationSchemaType(layout.Type.ID); ok {
		switch typ.Kind {
		case model.TypeKindDomain:
			return b.typeCategory(typ.BaseLayout)
		case model.TypeKindArray:
			return 'A'
		}
	}
	descriptor, err := b.findNamedType(layout.Type.ID, 0)
	if err != nil || descriptor == nil {
		return 0
	}
	spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
	if err != nil || spec.Type == nil {
		return 0
	}
	switch spec.Type.DeclaredKind() {
	case model.TypeKindDomain:
		return b.typeCategory(spec.Type.BaseLayout)
	case model.TypeKindEnum:
		return 'E'
	case model.TypeKindComposite:
		return 'C'
	case model.TypeKindRange, model.TypeKindMultirange:
		return 'R'
	case model.TypeKindArray:
		return 'A'
	default:
		return 0
	}
}

func (b *binder) coerceViaIO(s *scope, value expression, target model.TypeLayout) (expression, error) {
	info, ok := b.lookupTypeIO(value.typ)
	if !ok || info.Output == 0 {
		return value, missing("resolved type output function", value.position)
	}
	if err := b.validateIOLayout(value.typ, value.position); err != nil {
		return value, err
	}
	signature, ok := builtin.LookupSignature(info.Output)
	if !ok || signature.ArgumentCount != 1 || signature.Result != 2275 {
		return value, missing("type output signature form", value.position)
	}
	text, err := b.invoke(s, signature, 0, []expression{value}, ir.Call, 0, value.position)
	if err != nil {
		return value, err
	}
	result, err := b.inputValue(s, text, target)
	if err != nil || !b.collatable(target.Type) {
		return result, err
	}
	result.collation = b.expressionCollation(value)
	return result, nil
}
func (b *binder) lookupTypeIO(layout model.TypeLayout) (builtin.TypeIO, bool) {
	if info, ok := builtin.LookupTypeIO(layout); ok {
		return info, true
	}
	if typ, ok := b.options.CatalogRelations.ResolveInformationSchemaType(layout.Type.ID); ok {
		return typ.TypeIO, true
	}
	descriptor, err := b.findNamedType(layout.Type.ID, 0)
	if err != nil || descriptor == nil {
		return builtin.TypeIO{}, false
	}
	spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
	if err != nil || spec.Type == nil {
		return builtin.TypeIO{}, false
	}
	switch spec.Type.DeclaredKind() {
	case model.TypeKindDomain:
		base, ok := b.lookupTypeIO(spec.Type.BaseLayout)
		if !ok {
			return builtin.TypeIO{}, false
		}
		return builtin.TypeIO{Input: 2597, Output: base.Output, Parameter: layout.Type.ID, Delimiter: base.Delimiter}, true
	case model.TypeKindEnum:
		return builtin.TypeIO{Input: 3506, Output: 3507, Parameter: layout.Type.ID, Delimiter: ','}, true
	case model.TypeKindComposite:
		return builtin.TypeIO{Input: 2290, Output: 2291, Parameter: layout.Type.ID, Delimiter: ','}, true
	case model.TypeKindRange:
		return builtin.TypeIO{Input: 3834, Output: 3835, Parameter: layout.Type.ID, Delimiter: ','}, true
	case model.TypeKindMultirange:
		return builtin.TypeIO{Input: 4231, Output: 4232, Parameter: layout.Type.ID, Delimiter: ','}, true
	case model.TypeKindArray:
		return builtin.TypeIO{Input: 750, Output: 751, Parameter: spec.Type.BaseLayout.Type.ID, Delimiter: ','}, true
	default:
		return builtin.TypeIO{}, false
	}
}

// domainBaseResult is one cached domainBase verdict: the domain's base layout
// when domain is true, or a negative result for non-domain and unknown types.
type domainBaseResult struct {
	layout model.TypeLayout
	domain bool
}

func (b *binder) domainBase(layout model.TypeLayout) (model.TypeLayout, bool, error) {
	id := layout.Type.ID
	if cached, ok := b.domainBases[id]; ok {
		return cached.layout, cached.domain, nil
	}
	base, domain, err := b.resolveDomainBase(layout)
	if err != nil {
		return model.TypeLayout{}, false, err
	}
	if b.domainBases == nil {
		b.domainBases = make(map[model.TypeID]domainBaseResult)
	}
	b.domainBases[id] = domainBaseResult{layout: base, domain: domain}
	return base, domain, nil
}

func (b *binder) resolveDomainBase(layout model.TypeLayout) (model.TypeLayout, bool, error) {
	if typ, ok := b.options.CatalogRelations.ResolveInformationSchemaType(layout.Type.ID); ok {
		return typ.BaseLayout, typ.Kind == model.TypeKindDomain, nil
	}
	if _, ok := builtin.LookupType(layout.Type.ID); ok {
		// Builtin type IDs can never name a declared domain. Checking before
		// the catalog scan keeps overload scoring off the storage path for the
		// overwhelmingly common builtin argument types.
		return model.TypeLayout{}, false, nil
	}
	descriptor, err := b.findNamedType(layout.Type.ID, 0)
	if err != nil {
		return model.TypeLayout{}, false, err
	}
	if descriptor == nil {
		return model.TypeLayout{}, false, nil
	}
	spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
	if err != nil {
		return model.TypeLayout{}, false, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain {
		return model.TypeLayout{}, false, nil
	}
	return spec.Type.BaseLayout, true, nil
}

func (b *binder) validateIOLayout(layout model.TypeLayout, pos int) error {
	if _, err := model.TypeLayoutBytes(b.ctx, layout, b.options.Limits.MaxBytes); err != nil {
		return err
	}
	return b.validateCompositeDeclarations(layout, pos)
}
func (b *binder) domainDefault(layout model.TypeLayout) (*model.ExpressionDefinition, bool, error) {
	remaining := b.options.Limits.MaxDepth
	for {
		if _, ok := b.options.CatalogRelations.InformationDomainByType(layout.Type.ID); ok {
			return nil, false, nil
		}
		if _, builtinType := builtin.LookupType(layout.Type.ID); builtinType {
			return nil, false, nil
		}
		descriptor, err := b.findNamedType(layout.Type.ID, 0)
		if err != nil || descriptor == nil {
			return nil, false, err
		}
		spec, err := b.descriptorSpec(descriptor)
		if err != nil {
			return nil, false, err
		}
		if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain {
			return nil, false, nil
		}
		if spec.Type.Default != nil {
			return spec.Type.Default, true, nil
		}
		if remaining == 0 {
			return nil, false, model.ErrDescriptorLimit
		}
		remaining--
		layout = spec.Type.BaseLayout
	}
}

func (b *binder) validateCompositeDeclarations(layout model.TypeLayout, pos int) error {
	if layout.Type.Storage == model.TypeComposite && layout.Type.ID != 2249 {
		if _, err := b.namedCompositeFields(layout, pos); err != nil {
			return err
		}
	}
	for _, child := range layout.Children {
		if err := b.validateCompositeDeclarations(child, pos); err != nil {
			return err
		}
	}
	return b.ctx.Err()
}
