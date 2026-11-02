package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func rangePolymorphic(id model.TypeID) bool {
	return id == 3831 || id == 4537 || id == 5080 || id == 4538
}

type rangePolymorphicMode uint8

const (
	rangeExactOptional rangePolymorphicMode = iota
	rangeExactRequired
	rangeCompatibleOptional
	rangeCompatibleRequired
)

func rangePolymorphicMatches(family builtin.RangeFamilyInfo, declared, actual model.TypeID) bool {
	switch declared {
	case 3831, 5080:
		return actual == family.Range
	case 4537, 4538:
		return actual == family.Multirange
	default:
		return false
	}
}

// A subtype alone cannot identify a range type: several ranges may share it.
func (b *binder) polymorphicRangeFamily(signature builtin.Signature, args []expression, mode rangePolymorphicMode, pos int) (builtin.RangeFamilyInfo, error) {
	compatible := mode == rangeCompatibleOptional || mode == rangeCompatibleRequired
	requireKnown := mode == rangeExactRequired || mode == rangeCompatibleRequired
	var family builtin.RangeFamilyInfo
	required := rangePolymorphic(signature.Result) && compatiblePolymorphic(signature.Result) == compatible
	for i, arg := range args {
		declared := signature.Arguments[i]
		if !rangePolymorphic(declared) || compatiblePolymorphic(declared) != compatible {
			continue
		}
		required = true
		actual := b.argumentID(arg)
		if actual == 0 {
			continue
		}
		candidate, ok := b.lookupRangeFamily(actual)
		if !ok || !rangePolymorphicMatches(candidate, declared, actual) {
			return family, bindError("42804", "polymorphic range argument requires matching range type", pos)
		}
		if family.Range != 0 && family.Range != candidate.Range {
			return family, bindError("42804", "polymorphic range argument types differ", pos)
		}
		family = candidate
	}
	if required && requireKnown && family.Range == 0 {
		return family, bindError("42804", "could not determine polymorphic type because input has type unknown", pos)
	}
	return family, nil
}
func (b *binder) lookupRangeFamily(id model.TypeID) (builtin.RangeFamilyInfo, bool) {
	if family, ok := builtin.LookupRangeFamily(id); ok {
		return family, true
	}
	descriptor, err := b.findNamedType(id, 0)
	if err != nil || descriptor == nil {
		return builtin.RangeFamilyInfo{}, false
	}
	spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
	if err != nil || spec.Type == nil || spec.Type.Range == nil {
		return builtin.RangeFamilyInfo{}, false
	}
	metadata := spec.Type.Range
	rangeDescriptor, err := b.catalog.Descriptor(b.ctx, metadata.Range)
	if err != nil {
		return builtin.RangeFamilyInfo{}, false
	}
	multirangeDescriptor, err := b.catalog.Descriptor(b.ctx, metadata.Multirange)
	if err != nil {
		return builtin.RangeFamilyInfo{}, false
	}
	b.dependency(rangeDescriptor.Descriptor, 0)
	b.dependency(multirangeDescriptor.Descriptor, 0)
	return builtin.RangeFamilyInfo{
		Range:        model.TypeID(rangeDescriptor.Descriptor.OID()),
		Multirange:   model.TypeID(multirangeDescriptor.Descriptor.OID()),
		Subtype:      metadata.Subtype.Type.ID,
		SubtypeClass: metadata.OperatorClass,
		Collation:    metadata.Collation,
		Canonical:    metadata.CanonicalFunction,
		Difference:   metadata.DifferenceFunction,
	}, true
}

func (b *binder) declaredTypeConstructor(schema, name string, call callArguments, s *scope, pos int, variadicArray bool) (expression, bool, error) {
	if b.catalog == nil {
		return expression{}, false, nil
	}
	namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
	if isMissingObject(err) {
		return expression{}, false, nil
	}
	if err != nil {
		return expression{}, false, err
	}
	published, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectType, Name: name})
	if isMissingObject(err) {
		return expression{}, false, nil
	}
	if err != nil {
		return expression{}, false, err
	}
	spec, err := b.descriptorSpec(published.Descriptor)
	if err != nil {
		return expression{}, true, err
	}
	if spec.Type == nil {
		return expression{}, false, nil
	}
	kind := spec.Type.DeclaredKind()
	if kind != model.TypeKindComposite && kind != model.TypeKindRange && kind != model.TypeKindMultirange {
		return expression{}, false, nil
	}
	b.dependency(published.Descriptor, 0)
	if kind == model.TypeKindComposite {
		if variadicArray || call.names != nil || len(call.values) != len(spec.Type.Fields) {
			return expression{}, true, bindError("42883", "composite constructor arguments differ from its attributes", pos)
		}
		fields := make([]expression, len(call.values))
		for i, argument := range call.values {
			fields[i], err = b.coerce(s, argument, spec.Type.Fields[i].Layout, ir.ImplicitCast)
			if err != nil {
				return expression{}, true, err
			}
		}
		result, err := b.addScalar(s, expression{typ: spec.Type.Layout, position: pos}, ir.Scalar{Kind: ir.Row}, fields)
		return result, true, err
	}
	if call.names != nil {
		return expression{}, true, bindError("42883", "declared type constructor does not accept named arguments", pos)
	}
	if spec.Type.Range == nil {
		return expression{}, true, model.ErrInvalidDescriptor
	}
	if kind == model.TypeKindRange {
		if variadicArray || len(call.values) < 2 || len(call.values) > 3 || spec.Type.Range == nil {
			return expression{}, true, bindError("42883", "range constructor requires two bounds and optional bounds text", pos)
		}
		function := model.FunctionID(3840)
		if len(call.values) == 3 {
			function = 3841
		}
		signature, _ := builtin.LookupSignature(function)
		signature.Result = spec.Type.Layout.Type.ID
		targets := []model.TypeLayout{spec.Type.Range.Subtype, spec.Type.Range.Subtype}
		signature.Arguments[0], signature.Arguments[1] = targets[0].Type.ID, targets[1].Type.ID
		if len(call.values) == 3 {
			text := b.primitive(25)
			targets = append(targets, text)
			signature.Arguments[2] = 25
		}
		result, err := b.invokeArguments(s, signature, 0, call.values, ir.Call, 0, pos, spec.Type.Layout, targets, false)
		return result, true, err
	}
	rangeDescriptor, err := b.catalog.Descriptor(b.ctx, spec.Type.Range.Range)
	if err != nil {
		return expression{}, true, err
	}
	rangeSpec, err := b.descriptorSpec(rangeDescriptor.Descriptor)
	if err != nil || rangeSpec.Type == nil {
		if err != nil {
			return expression{}, true, err
		}
		return expression{}, true, model.ErrInvalidDescriptor
	}
	b.dependency(rangeDescriptor.Descriptor, 0)
	arrayDescriptor, err := b.catalog.Descriptor(b.ctx, rangeSpec.Type.Array)
	if err != nil {
		return expression{}, true, err
	}
	arraySpec, err := b.descriptorSpec(arrayDescriptor.Descriptor)
	if err != nil || arraySpec.Type == nil {
		if err != nil {
			return expression{}, true, err
		}
		return expression{}, true, model.ErrInvalidDescriptor
	}
	b.dependency(arrayDescriptor.Descriptor, 0)
	args := call.values
	function := model.FunctionID(4280)
	targets := []model.TypeLayout{}
	passVariadic := false
	switch {
	case variadicArray:
		if len(args) != 1 {
			return expression{}, true, bindError("42883", "VARIADIC multirange constructor requires one range array", pos)
		}
		function, targets, passVariadic = 4282, []model.TypeLayout{arraySpec.Type.Layout}, true
	case len(args) == 0:
	case len(args) == 1:
		function, targets = 4281, []model.TypeLayout{rangeSpec.Type.Layout}
	default:
		coerced := make([]expression, len(args))
		for i, argument := range args {
			coerced[i], err = b.coerce(s, argument, rangeSpec.Type.Layout, ir.ImplicitCast)
			if err != nil {
				return expression{}, true, err
			}
		}
		packed, packErr := b.addScalar(s, expression{typ: arraySpec.Type.Layout, position: pos}, ir.Scalar{Kind: ir.Array}, coerced)
		if packErr != nil {
			return expression{}, true, packErr
		}
		args, function, targets = []expression{packed}, 4282, []model.TypeLayout{arraySpec.Type.Layout}
	}
	signature, _ := builtin.LookupSignature(function)
	signature.Result = spec.Type.Layout.Type.ID
	if len(targets) != 0 {
		signature.Arguments[0] = targets[0].Type.ID
	}
	result, err := b.invokeArguments(s, signature, 0, args, ir.Call, 0, pos, spec.Type.Layout, targets, passVariadic)
	return result, true, err
}
