package builtin

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type TypeScalarCall func(context.Context, []model.Value, CallBudget) (model.Value, error)

// TypeTextCodec is a snapshot-pinned unary SQL text codec.
type TypeTextCodec struct {
	Input      TypeScalarCall
	Output     TypeScalarCall
	OwnedBytes uint64
}

type TypeRuntimeCompiler interface {
	CompileDomainCheck(context.Context, TypeResolver, *model.Descriptor, model.ConstraintDescriptor, model.TypeLayout) (TypeScalarCall, error)
	ResolveTypeFunction(context.Context, TypeResolver, model.ObjectID, model.FunctionID, []model.TypeLayout, model.TypeLayout) (TypeScalarCall, error)
}
type typeRuntimeInformationSchema interface {
	InformationSchemaTypeResolver() InformationSchemaTypeResolver
}

type TypeDefinition struct {
	Descriptor *model.Descriptor
	Published  bool
}

type TypeResolver interface {
	ResolveType(model.TypeID) (*ResolvedType, bool)
	SemanticFingerprint() [sha256.Size]byte
}

type ResolvedType struct {
	resolver   *declaredTypeResolver
	descriptor *model.Descriptor
	object     model.ObjectID
	version    uint64
	published  bool
	metadata   model.TypeMetadata
	labelOIDs  map[string]model.OID
	labelOrder map[model.OID]int
	checks     []resolvedDomainCheck
	canonical  TypeScalarCall
	difference TypeScalarCall
}

type resolvedDomainCheck struct {
	name string
	id   model.ObjectID
	call TypeScalarCall
}

type declaredTypeResolver struct {
	types       map[model.TypeID]*ResolvedType
	objects     map[model.ObjectID]*ResolvedType
	information InformationSchemaTypeResolver
	owned       uint64
	fingerprint [sha256.Size]byte
}

func NewDeclaredTypeResolver(ctx context.Context, definitions []TypeDefinition, compiler TypeRuntimeCompiler, limits model.DescriptorLimits) (TypeResolver, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(definitions) > limits.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	resolver := &declaredTypeResolver{}
	if provider, ok := compiler.(typeRuntimeInformationSchema); ok {
		resolver.information = provider.InformationSchemaTypeResolver()
	}
	// DescriptorLimits.MaxBytes bounds one owned descriptor, not the catalog.
	// The resolver charges two owned descriptor copies per declared type (the
	// bounded copy it takes while resolving plus the retained resolved
	// metadata) and its own maps, so its aggregate allowance is derived from
	// the population it was handed. A fixed cap of one descriptor's MaxBytes
	// admitted only a few hundred small enums before every later statement
	// failed (TestDeclaredTypeResolverAdmitsCatalogSizedPopulation).
	mapEntryBytes := 6 * uint64(unsafe.Sizeof(uintptr(0)))
	if limits.MaxBytes > (math.MaxUint64-mapEntryBytes-uint64(unsafe.Sizeof(ResolvedType{})))/2 {
		return nil, model.ErrDescriptorLimit
	}
	perType := 2*limits.MaxBytes + mapEntryBytes + uint64(unsafe.Sizeof(ResolvedType{}))
	population := max(uint64(len(definitions)), 1)
	if perType > math.MaxUint64/population {
		return nil, model.ErrDescriptorLimit
	}
	budget := perType * population
	reserve := func(count, size uint64) error {
		if size != 0 && count > (budget-resolver.owned)/size {
			return &model.Error{Code: "54000", Message: "declared type metadata budget exceeded"}
		}
		resolver.owned += count * size
		return nil
	}
	if err := reserve(uint64(len(definitions)), mapEntryBytes); err != nil {
		return nil, err
	}
	resolver.types = make(map[model.TypeID]*ResolvedType, len(definitions))
	resolver.objects = make(map[model.ObjectID]*ResolvedType, len(definitions))
	for _, definition := range definitions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if definition.Descriptor == nil || definition.Descriptor.Kind() != model.ObjectType {
			return nil, model.ErrInvalidDescriptor
		}
		if resolver.owned >= budget || budget-resolver.owned < 2 {
			return nil, &model.Error{Code: "54000", Message: "declared type metadata budget exceeded"}
		}
		copyLimits := limits
		if remaining := (budget - resolver.owned) / 2; remaining < copyLimits.MaxBytes {
			copyLimits.MaxBytes = remaining
		}
		spec, copied, err := definition.Descriptor.SpecWithinBytes(ctx, copyLimits)
		if err != nil {
			return nil, err
		}
		if spec.Type == nil || spec.Type.Layout.Type.ID != model.TypeID(definition.Descriptor.OID()) {
			return nil, model.ErrInvalidDescriptor
		}
		id := spec.Type.Layout.Type.ID
		if resolver.types[id] != nil {
			return nil, &model.Error{Code: "42710", Message: "declared type identity appears more than once"}
		}
		resolved := &ResolvedType{
			descriptor: definition.Descriptor,
			object:     definition.Descriptor.ID(),
			version:    definition.Descriptor.Version(),
			published:  definition.Published,
			metadata:   *spec.Type,
		}
		if err := reserve(2, copied); err != nil {
			return nil, err
		}
		if err := reserve(1, uint64(unsafe.Sizeof(*resolved))); err != nil {
			return nil, err
		}
		if spec.Type.DeclaredKind() == model.TypeKindEnum {
			if err := reserve(uint64(len(spec.Type.Elements)), mapEntryBytes); err != nil {
				return nil, err
			}
			resolved.labelOIDs = make(map[string]model.OID, len(spec.Type.Elements))
			resolved.labelOrder = make(map[model.OID]int, len(spec.Type.Elements))
			for i, label := range spec.Type.Elements {
				resolved.labelOIDs[label] = spec.Type.ElementOIDs[i]
				resolved.labelOrder[spec.Type.ElementOIDs[i]] = i
			}
		}
		if spec.Type.DeclaredKind() == model.TypeKindDomain {
			if err := reserve(uint64(len(spec.Type.Constraints)), uint64(unsafe.Sizeof(resolvedDomainCheck{}))); err != nil {
				return nil, err
			}
		}
		if spec.Type.Range != nil {
			if err := reserve(2, uint64(unsafe.Sizeof(TypeScalarCall(nil)))); err != nil {
				return nil, err
			}
		}
		if resolver.objects[resolved.object] != nil {
			return nil, &model.Error{Code: "42710", Message: "declared type object appears more than once"}
		}
		resolver.types[id] = resolved
		resolved.resolver = resolver
		resolver.objects[resolved.object] = resolved
	}
	if err := resolver.validateLinks(); err != nil {
		return nil, err
	}
	for _, resolved := range resolver.types {
		if err := compileDeclaredType(ctx, resolver, resolved, compiler); err != nil {
			return nil, err
		}
	}
	for _, resolved := range resolver.types {
		if resolved.metadata.DeclaredKind() != model.TypeKindMultirange || resolved.metadata.Range == nil {
			continue
		}
		rangeType := resolver.objects[resolved.metadata.Range.Range]
		if rangeType == nil {
			return nil, model.ErrInvalidDescriptor
		}
		resolved.canonical = rangeType.canonical
		resolved.difference = rangeType.difference
	}
	resolver.computeFingerprint()
	return resolver, nil
}

func (r *declaredTypeResolver) validateLinks() error {
	for id, resolved := range r.types {
		switch resolved.metadata.DeclaredKind() {
		case model.TypeKindDomain:
			seen := map[model.TypeID]bool{id: true}
			base := resolved.metadata.BaseLayout.Type.ID
			for {
				next, custom := r.types[base]
				if !custom {
					if _, builtinType := LookupType(base); builtinType {
						break
					}
					if r.information != nil {
						if _, informationType := r.information.ResolveInformationSchemaType(base); informationType {
							break
						}
					}
					return model.ErrInvalidDescriptor
				}
				if seen[base] {
					return model.ErrInvalidDescriptor
				}
				seen[base] = true
				if next.metadata.DeclaredKind() != model.TypeKindDomain {
					break
				}
				base = next.metadata.BaseLayout.Type.ID
			}
		case model.TypeKindRange, model.TypeKindMultirange:
			metadata := resolved.metadata.Range
			if metadata == nil {
				return model.ErrInvalidDescriptor
			}
			rangeType := r.objects[metadata.Range]
			multirangeType := r.objects[metadata.Multirange]
			if rangeType == nil || multirangeType == nil || rangeType.metadata.DeclaredKind() != model.TypeKindRange || multirangeType.metadata.DeclaredKind() != model.TypeKindMultirange || !sameRangeDeclaration(metadata, rangeType.metadata.Range) || !sameRangeDeclaration(metadata, multirangeType.metadata.Range) {
				return model.ErrInvalidDescriptor
			}
		case model.TypeKindArray:
			if resolved.metadata.Base != 0 && r.objects[resolved.metadata.Base] == nil {
				return model.ErrInvalidDescriptor
			}
		}
	}
	return nil
}
func sameRangeDeclaration(left, right *model.RangeTypeMetadata) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.OperatorClass == right.OperatorClass &&
		left.Collation == right.Collation &&
		left.Canonical == right.Canonical &&
		left.Difference == right.Difference &&
		left.CanonicalFunction == right.CanonicalFunction &&
		left.DifferenceFunction == right.DifferenceFunction &&
		left.Range == right.Range &&
		left.Multirange == right.Multirange &&
		sameArrayTrimLayout(left.Subtype, right.Subtype)
}

func (r *declaredTypeResolver) computeFingerprint() {
	ids := make([]model.TypeID, 0, len(r.types))
	for id := range r.types {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	hash := sha256.New()
	var encoded [21]byte
	for _, id := range ids {
		resolved := r.types[id]
		binary.LittleEndian.PutUint32(encoded[:4], uint32(id))
		binary.LittleEndian.PutUint64(encoded[4:12], uint64(resolved.object))
		binary.LittleEndian.PutUint64(encoded[12:20], resolved.version)
		encoded[20] = 0
		if resolved.published {
			encoded[20] = 1
		}
		_, _ = hash.Write(encoded[:])
	}
	copy(r.fingerprint[:], hash.Sum(nil))
}

func compileDeclaredType(ctx context.Context, types TypeResolver, resolved *ResolvedType, compiler TypeRuntimeCompiler) error {
	if resolved.metadata.DeclaredKind() == model.TypeKindDomain {
		resolved.checks = make([]resolvedDomainCheck, 0, len(resolved.metadata.Constraints))
		for _, constraint := range resolved.metadata.Constraints {
			if constraint.Kind == model.ConstraintNotNull {
				continue
			}
			if constraint.Kind != model.ConstraintCheck {
				return model.ErrInvalidDescriptor
			}
			if !constraint.Enforced {
				continue
			}
			if compiler == nil {
				return &model.Error{Code: "0A000", Message: "domain constraint compiler is not configured"}
			}
			call, err := compiler.CompileDomainCheck(ctx, types, resolved.descriptor, constraint, resolved.metadata.BaseLayout)
			if err != nil {
				return err
			}
			resolved.checks = append(resolved.checks, resolvedDomainCheck{id: constraint.ID, name: constraint.Name, call: call})
		}
	}
	if resolved.metadata.Range == nil || resolved.metadata.DeclaredKind() != model.TypeKindRange {
		return nil
	}
	rangeType := resolved.metadata.Range
	if rangeType.CanonicalFunction != 0 {
		if compiler == nil {
			return &model.Error{Code: "0A000", Message: "range support function resolver is not configured"}
		}
		call, err := compiler.ResolveTypeFunction(ctx, types, rangeType.Canonical, rangeType.CanonicalFunction, []model.TypeLayout{resolved.metadata.Layout}, resolved.metadata.Layout)
		if err != nil {
			return err
		}
		resolved.canonical = call
	}
	if rangeType.DifferenceFunction != 0 {
		if compiler == nil {
			return &model.Error{Code: "0A000", Message: "range support function resolver is not configured"}
		}
		result := model.TypeLayout{Type: model.Type{ID: 701, Modifier: -1, Storage: model.TypeFloat64}}
		call, err := compiler.ResolveTypeFunction(ctx, types, rangeType.Difference, rangeType.DifferenceFunction, []model.TypeLayout{rangeType.Subtype, rangeType.Subtype}, result)
		if err != nil {
			return err
		}
		resolved.difference = call
	}
	return nil
}

func (r *declaredTypeResolver) ResolveType(id model.TypeID) (*ResolvedType, bool) {
	if r == nil {
		return nil, false
	}
	resolved, ok := r.types[id]
	return resolved, ok
}

func (r *declaredTypeResolver) SemanticFingerprint() [sha256.Size]byte {
	if r == nil {
		return [sha256.Size]byte{}
	}
	return r.fingerprint
}

func (r *ResolvedType) Object() model.ObjectID       { return r.object }
func (r *ResolvedType) Version() uint64              { return r.version }
func (r *ResolvedType) Published() bool              { return r.published }
func (r *ResolvedType) Kind() model.TypeKind         { return r.metadata.DeclaredKind() }
func (r *ResolvedType) Layout() model.TypeLayout     { return r.metadata.Layout }
func (r *ResolvedType) BaseLayout() model.TypeLayout { return r.metadata.BaseLayout }

func (r *ResolvedType) TypeIO() TypeIO {
	switch r.metadata.DeclaredKind() {
	case model.TypeKindDomain:
		return TypeIO{Input: 2597, Parameter: r.metadata.Layout.Type.ID, Delimiter: ','}
	case model.TypeKindEnum:
		return TypeIO{Input: 3506, Output: 3507, Parameter: r.metadata.Layout.Type.ID, Delimiter: ','}
	case model.TypeKindComposite:
		return TypeIO{Input: 2290, Output: 2291, Parameter: r.metadata.Layout.Type.ID, Delimiter: ','}
	case model.TypeKindRange:
		return TypeIO{Input: 3834, Output: 3835, Parameter: r.metadata.Layout.Type.ID, Delimiter: ','}
	case model.TypeKindMultirange:
		return TypeIO{Input: 4231, Output: 4232, Parameter: r.metadata.Layout.Type.ID, Delimiter: ','}
	case model.TypeKindArray:
		return TypeIO{Input: 750, Output: 751, Parameter: r.metadata.BaseLayout.Type.ID, Delimiter: ','}
	default:
		return TypeIO{}
	}
}

func (r *ResolvedType) RangeFamily() (RangeFamilyInfo, bool) {
	if r == nil || r.metadata.Range == nil || r.resolver == nil {
		return RangeFamilyInfo{}, false
	}
	metadata := r.metadata.Range
	rangeType := r.resolver.objects[metadata.Range]
	multirangeType := r.resolver.objects[metadata.Multirange]
	if rangeType == nil || multirangeType == nil {
		return RangeFamilyInfo{}, false
	}
	representation := metadata.Subtype.Type.ID
	layout := metadata.Subtype
	for range 64 {
		base, custom := r.resolver.types[layout.Type.ID]
		if !custom || base.metadata.DeclaredKind() != model.TypeKindDomain {
			representation = layout.Type.ID
			break
		}
		layout = base.metadata.BaseLayout
	}
	var enum *ResolvedType
	if subtype, custom := r.resolver.types[representation]; custom && subtype.metadata.DeclaredKind() == model.TypeKindEnum {
		enum = subtype
	}
	return RangeFamilyInfo{
		Range:          rangeType.metadata.Layout.Type.ID,
		Multirange:     multirangeType.metadata.Layout.Type.ID,
		Subtype:        metadata.Subtype.Type.ID,
		SubtypeClass:   metadata.OperatorClass,
		Collation:      metadata.Collation,
		Canonical:      metadata.CanonicalFunction,
		Difference:     metadata.DifferenceFunction,
		SubtypeStorage: metadata.Subtype.Type.Storage,
		Representation: representation,
		canonicalCall:  r.canonical,
		differenceCall: r.difference,
		enum:           enum,
	}, true
}
func (r *ResolvedType) SubtypeDifference(ctx context.Context, left, right model.Value, budget CallBudget) (float64, error) {
	if r == nil || r.difference == nil || r.metadata.Range == nil {
		return 0, &model.Error{Code: "42883", Message: "range subtype difference function is not declared"}
	}
	storage := r.metadata.Range.Subtype.Type.Storage
	if left.IsNull || right.IsNull || left.Type != storage || right.Type != storage {
		return 0, &model.Error{Code: "42804", Message: "range subtype difference arguments differ from their declaration"}
	}
	arguments := [2]model.Value{left, right}
	result, err := r.difference(ctx, arguments[:], budget)
	if err != nil {
		return 0, err
	}
	if result.IsNull || result.Type != model.TypeFloat64 {
		return 0, &model.Error{Code: "42804", Message: "range subtype difference function returned an invalid result"}
	}
	return result.FloatVal, nil
}

func resolvedType(options ResolveOptions, id model.TypeID, kind model.TypeKind) (*ResolvedType, bool) {
	if options.Types == nil {
		return nil, false
	}
	resolved, ok := options.Types.ResolveType(id)
	return resolved, ok && resolved.Kind() == kind
}
func resolvedDomainBaseLayout(layout model.TypeLayout, options ResolveOptions) (model.TypeLayout, error) {
	current := layout
	for range 64 {
		if options.Types != nil {
			if resolved, known := options.Types.ResolveType(current.Type.ID); known {
				if !sameArrayConstructionLayout(current, resolved.Layout()) {
					return model.TypeLayout{}, sqlError("42804", "logical type layout differs from its declaration")
				}
				if resolved.Kind() == model.TypeKindDomain {
					current = resolved.BaseLayout()
					continue
				}
				return current, nil
			}
		}
		if options.InformationSchemaTypes != nil {
			if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(current.Type.ID); known {
				if !sameArrayConstructionLayout(current, binding.Layout) {
					return model.TypeLayout{}, sqlError("42804", "logical catalog type layout differs from its declaration")
				}
				if binding.Kind == model.TypeKindDomain {
					current = binding.BaseLayout
					continue
				}
				return current, nil
			}
		}
		return current, nil
	}
	return model.TypeLayout{}, model.ErrDescriptorLimit
}

func resolvedStorageLayout(layout model.TypeLayout, storage model.DataType, options ResolveOptions) (model.TypeLayout, error) {
	current, err := resolvedDomainBaseLayout(layout, options)
	if err != nil {
		return model.TypeLayout{}, err
	}
	if options.Types != nil {
		if resolved, known := options.Types.ResolveType(current.Type.ID); known {
			kind := resolved.Kind()
			if kind == model.TypeKindComposite && storage == model.TypeComposite {
				return current, nil
			}
			if kind == model.TypeKindArray && storage == model.TypeArray {
				if _, _, err := arrayCodecLayout(current, options); err != nil {
					return model.TypeLayout{}, err
				}
				return current, nil
			}
			return model.TypeLayout{}, sqlError("42804", "logical type has incompatible storage")
		}
	}
	if options.InformationSchemaTypes != nil {
		if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(current.Type.ID); known {
			if binding.Kind == model.TypeKindArray && storage == model.TypeArray {
				if _, _, err := arrayCodecLayout(current, options); err != nil {
					return model.TypeLayout{}, err
				}
				return current, nil
			}
			return model.TypeLayout{}, sqlError("42804", "logical catalog type has incompatible storage")
		}
	}
	if storage == model.TypeComposite {
		if current.Type.Storage != model.TypeComposite {
			return model.TypeLayout{}, sqlError("42804", "composite operation requires a composite declaration")
		}
		// Every composite layout shares the record representation codecs, so a
		// concrete layout is self-describing once its field declarations have
		// been validated. A resolver-aware declaration was already checked above.
		return current, nil
	}
	if storage == model.TypeArray {
		if _, _, err := arrayCodecLayout(current, options); err != nil {
			return model.TypeLayout{}, err
		}
		return current, nil
	}
	return model.TypeLayout{}, sqlError("42804", "logical type has incompatible storage")
}

func resolveEnumCall(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, bool, error) {
	var id model.TypeID
	if signature.ID == 3504 || signature.ID == 3506 || signature.ID == 3532 {
		id = result.Type.ID
	} else if len(arguments) != 0 {
		id = arguments[0].Type.ID
	} else {
		id = result.Type.ID
	}
	resolved, ok := resolvedType(options, id, model.TypeKindEnum)
	if !ok {
		// A domain over an enum resolves enum calls through its base type; the
		// layout check below compares each argument's unwrapped base.
		source := result
		if len(arguments) != 0 {
			source = arguments[0]
		}
		base, err := resolvedDomainBaseLayout(source, options)
		if err != nil {
			return Callable{}, true, err
		}
		if base.Type.ID == id {
			return Callable{}, false, nil
		}
		resolved, ok = resolvedType(options, base.Type.ID, model.TypeKindEnum)
		if !ok {
			return Callable{}, false, nil
		}
	}
	input := signature.ID == 3504 || signature.ID == 3506 || signature.ID == 3532
	if input {
		base, err := resolvedDomainBaseLayout(result, options)
		if err != nil || !sameArrayConstructionLayout(base, resolved.Layout()) {
			return Callable{}, true, sqlError("42804", "enum call layout differs from its declaration")
		}
	} else {
		for _, argument := range arguments {
			base, err := resolvedDomainBaseLayout(argument, options)
			if err != nil || !sameArrayConstructionLayout(base, resolved.Layout()) {
				return Callable{}, true, sqlError("42804", "enum call layout differs from its declaration")
			}
		}
	}
	metadata := uint64(unsafe.Sizeof((*ResolvedType)(nil)))
	if metadata > maximum {
		return Callable{}, true, sqlError("54000", "enum callable metadata budget exceeded")
	}
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, metadataBytes: metadata}
	callable.signature.Result = result.Type.ID
	for i := range arguments {
		callable.signature.Arguments[i] = arguments[i].Type.ID
		callable.argumentStorage[i] = arguments[i].Type.Storage
	}
	callable.kernel = resolved.enumKernel(signature.ID)
	if callable.kernel == nil {
		return Callable{}, true, &MissingImplementationError{Signature: signature}
	}
	return callable, true, ctx.Err()
}
func (r *ResolvedType) enumKernel(function model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		switch function {
		case 3504, 3506, 3532:
			expected := 1
			label := ""
			if function == 3506 || function == 3532 {
				expected = 2
			}
			if len(arguments) != expected || arguments[0].IsNull {
				return model.Value{}, sqlError("42804", "enum input arguments differ from their declaration")
			}
			if function == 3532 {
				if arguments[0].Type != model.TypeBytes {
					return model.Value{}, sqlError("42804", "enum input arguments differ from their declaration")
				}
				label = string(arguments[0].BytesVal)
			} else {
				if arguments[0].Type != model.TypeString {
					return model.Value{}, sqlError("42804", "enum input arguments differ from their declaration")
				}
				label = arguments[0].StrVal
			}
			if expected == 2 && (arguments[1].IsNull || arguments[1].Type != model.TypeInt64 || arguments[1].IntVal != int64(r.metadata.Layout.Type.ID)) {
				return model.Value{}, sqlError("42804", "enum input metadata differs from its declaration")
			}
			return r.ParseEnumLabel(label)
		case 3505, 3507, 3533:
			label, err := r.enumLabel(arguments[0])
			if err != nil {
				return model.Value{}, err
			}
			if int64(len(label)) > budget.OutputBytes {
				return model.Value{}, &model.Error{Code: "54000", Message: "enum output exceeds budget"}
			}
			if function == 3533 {
				return model.BytesVal([]byte(label)), nil
			}
			return model.StringVal(label), nil
		case 3508, 3509, 3510, 3511, 3512, 3513, 3514, 3524, 3525:
			order, err := r.compareEnum(arguments[0], arguments[1])
			if err != nil {
				return model.Value{}, err
			}
			switch function {
			case 3508:
				return model.BoolVal(order == 0), nil
			case 3509:
				return model.BoolVal(order != 0), nil
			case 3510:
				return model.BoolVal(order < 0), nil
			case 3511:
				return model.BoolVal(order > 0), nil
			case 3512:
				return model.BoolVal(order <= 0), nil
			case 3513:
				return model.BoolVal(order >= 0), nil
			case 3514:
				return model.Int64Val(int64(order)), nil
			case 3524:
				if order <= 0 {
					return arguments[0].Clone(), nil
				}
				return arguments[1].Clone(), nil
			default:
				if order >= 0 {
					return arguments[0].Clone(), nil
				}
				return arguments[1].Clone(), nil
			}
		case 3515:
			if _, err := r.enumOrder(arguments[0]); err != nil {
				return model.Value{}, err
			}
			value := uint32(arguments[0].IntVal)
			value ^= value >> 16
			value *= 0x7feb352d
			value ^= value >> 15
			return model.Int64Val(int64(int32(value))), nil
		case 3528, 3529:
			if len(r.metadata.ElementOIDs) == 0 {
				return model.Value{}, &model.Error{Code: "22023", Message: "enum has no values"}
			}
			index := 0
			if function == 3529 {
				index = len(r.metadata.ElementOIDs) - 1
			}
			value := model.Int64Val(int64(r.metadata.ElementOIDs[index]))
			if _, err := r.enumOrder(value); err != nil {
				return model.Value{}, err
			}
			return value, nil
		case 3530, 3531:
			start, end := 0, len(r.metadata.ElementOIDs)-1
			if function == 3530 {
				if !arguments[0].IsNull {
					var err error
					start, err = r.enumOrder(arguments[0])
					if err != nil {
						return model.Value{}, err
					}
				}
				if !arguments[1].IsNull {
					var err error
					end, err = r.enumOrder(arguments[1])
					if err != nil {
						return model.Value{}, err
					}
				}
			}
			count := max(0, end-start+1)
			values := make([]model.Value, 0, count)
			if count != 0 {
				for _, oid := range r.metadata.ElementOIDs[start : end+1] {
					value := model.Int64Val(int64(oid))
					if _, err := r.enumOrder(value); err != nil {
						return model.Value{}, err
					}
					values = append(values, value)
				}
			}
			return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}}, nil
		default:
			return model.Value{}, &MissingImplementationError{Signature: Signature{ID: function}}
		}
	}
}

// ParseEnumLabel resolves a label under the pinned enum transaction state.
func (r *ResolvedType) ParseEnumLabel(label string) (model.Value, error) {
	if r == nil || r.metadata.DeclaredKind() != model.TypeKindEnum {
		return model.Value{}, sqlError("42804", "declared type is not an enum")
	}
	oid, found := r.labelOIDs[label]
	if !found {
		return model.Value{}, &model.Error{Code: "22P02", Message: "invalid input value for enum"}
	}
	value := model.Int64Val(int64(oid))
	if _, err := r.enumOrder(value); err != nil {
		return model.Value{}, err
	}
	return value, nil
}

// FormatEnumValue resolves a physical enum datum to its pinned label.
func (r *ResolvedType) FormatEnumValue(value model.Value) (string, error) {
	if r == nil || r.metadata.DeclaredKind() != model.TypeKindEnum {
		return "", sqlError("42804", "declared type is not an enum")
	}
	return r.enumLabel(value)
}
func (r *ResolvedType) enumOrder(value model.Value) (int, error) {
	if value.IsNull || value.Type != model.TypeInt64 || value.IntVal <= 0 {
		return 0, sqlError("42804", "enum value differs from its declaration")
	}
	order, ok := r.labelOrder[model.OID(value.IntVal)]
	if !ok {
		return 0, &model.Error{Code: "22P02", Message: "invalid internal enum value"}
	}
	if !r.published && slices.Contains(r.metadata.UnsafeElementOIDs, model.OID(value.IntVal)) {
		return 0, &model.Error{Code: "55P04", Message: "unsafe use of new enum value"}
	}
	return order, nil
}

func (r *ResolvedType) compareEnum(left, right model.Value) (int, error) {
	a, err := r.enumOrder(left)
	if err != nil {
		return 0, err
	}
	b, err := r.enumOrder(right)
	if err != nil {
		return 0, err
	}
	return cmp.Compare(a, b), nil
}

func (r *ResolvedType) enumLabel(value model.Value) (string, error) {
	order, err := r.enumOrder(value)
	if err != nil {
		return "", err
	}
	return r.metadata.Elements[order], nil
}

func resolveDomainInput(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, bool, error) {
	resolved, custom := resolvedType(options, result.Type.ID, model.TypeKindDomain)
	var base, declaration model.TypeLayout
	var validate func(context.Context, model.Value, CallBudget) (model.Value, error)
	metadata := uint64(unsafe.Sizeof((*ResolvedType)(nil)))
	if custom {
		base, declaration = resolved.metadata.BaseLayout, resolved.metadata.Layout
		validate = resolved.validateDomain
	} else {
		if options.InformationSchemaTypes == nil {
			return Callable{}, false, nil
		}
		binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(result.Type.ID)
		if !known || binding.Kind != model.TypeKindDomain {
			return Callable{}, false, nil
		}
		base, declaration = binding.BaseLayout, binding.Layout
		metadata = uint64(unsafe.Sizeof(binding))
		validate = func(_ context.Context, value model.Value, _ CallBudget) (model.Value, error) {
			if err := validateInformationDomainValue(binding.Domain, value); err != nil {
				return model.Value{}, err
			}
			return value, nil
		}
	}
	if len(arguments) != 3 || !sameArrayConstructionLayout(result, declaration) {
		return Callable{}, true, sqlError("42804", "domain input layout mismatch")
	}
	for i, id := range []model.TypeID{2275, 26, 23} {
		if arguments[i].Type.ID != id || arguments[i].Type.Storage != primitiveStorage(id) {
			return Callable{}, true, sqlError("42804", "domain input metadata layout mismatch")
		}
	}
	baseIO, known := lookupResolvedTypeIO(options, base)
	if !known || baseIO.Input == 0 {
		return Callable{}, true, sqlError("42804", "domain base type has no input function")
	}
	baseInput, err := resolveInputCallable(ctx, baseIO, base, options, maximum)
	if err != nil {
		return Callable{}, true, err
	}
	metadata += baseInput.OwnedBytes()
	if metadata > maximum {
		return Callable{}, true, sqlError("54000", "domain callable metadata budget exceeded")
	}
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, metadataBytes: metadata}
	callable.signature.Result = result.Type.ID
	for i := range arguments {
		callable.signature.Arguments[i] = arguments[i].Type.ID
		callable.argumentStorage[i] = arguments[i].Type.Storage
	}
	callable.kernel = func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		if len(values) != 3 || values[1].IsNull || values[1].Type != model.TypeInt64 || values[1].IntVal != int64(result.Type.ID) || values[2].IsNull || values[2].Type != model.TypeInt64 || values[2].IntVal != int64(result.Type.Modifier) {
			return model.Value{}, sqlError("42804", "domain input metadata differs from its declaration")
		}
		var baseValues [3]model.Value
		count := int(baseInput.ArgumentCount())
		baseValues[0] = values[0]
		if count >= 2 {
			baseValues[1] = model.Int64Val(int64(baseIO.Parameter))
		}
		if count == 3 {
			baseValues[2] = model.Int64Val(int64(base.Type.Modifier))
		}
		value, err := baseInput.Call(ctx, baseValues[:count], budget)
		if err != nil {
			return model.Value{}, err
		}
		return validate(ctx, value, budget)
	}
	return callable, true, nil
}

func (r *ResolvedType) validateDomain(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	if value.IsNull && r.metadata.NotNull {
		return model.Value{}, &model.Error{Code: "23502", Message: "domain does not allow null values"}
	}
	for _, check := range r.checks {
		arguments := [1]model.Value{value}
		result, err := check.call(ctx, arguments[:], budget)
		if err != nil {
			return model.Value{}, err
		}
		if !result.IsNull && (result.Type != model.TypeBoolean || result.IntVal == 0) {
			return model.Value{}, &model.Error{Code: "23514", Message: "value for domain violates check constraint " + check.name}
		}
	}
	return value, nil
}

func (r *ResolvedType) validateDomainConstraint(ctx context.Context, value model.Value, constraint model.ObjectID, budget CallBudget) error {
	for _, metadata := range r.metadata.Constraints {
		if metadata.ID != constraint {
			continue
		}
		if metadata.Kind == model.ConstraintNotNull {
			if value.IsNull {
				return &model.Error{Code: "23502", Message: "domain does not allow null values"}
			}
			return nil
		}
		if metadata.Kind != model.ConstraintCheck {
			return model.ErrInvalidDescriptor
		}
		for _, check := range r.checks {
			if check.id != constraint {
				continue
			}
			arguments := [1]model.Value{value}
			result, err := check.call(ctx, arguments[:], budget)
			if err != nil {
				return err
			}
			if !result.IsNull && (result.Type != model.TypeBoolean || result.IntVal == 0) {
				return &model.Error{Code: "23514", Message: "value for domain violates check constraint " + check.name}
			}
			return nil
		}
	}
	return model.ErrInvalidDescriptor
}

// ValidateDeclaredValue enforces every descriptor-backed domain constraint and
// validates nested enum, composite, array, range, and multirange representations.
func ValidateDeclaredValue(ctx context.Context, resolver TypeResolver, layout model.TypeLayout, value model.Value, budget CallBudget, maxDepth int) error {
	if resolver == nil || maxDepth <= 0 {
		return sqlError("22023", "invalid declared value validation options")
	}
	return validateDeclaredValue(ctx, resolver, nil, nil, 0, layout, value, budget, maxDepth)
}

// ValidateResolvedValue also enforces immutable logical-catalog domain and
// array identities carried by the callable resolver bundle.
func ValidateResolvedValue(ctx context.Context, options ResolveOptions, layout model.TypeLayout, value model.Value, budget CallBudget, maxDepth int) error {
	if (options.Types == nil && options.InformationSchemaTypes == nil) || maxDepth <= 0 {
		return sqlError("22023", "invalid declared value validation options")
	}
	return validateDeclaredValue(ctx, options.Types, options.InformationSchemaTypes, nil, 0, layout, value, budget, maxDepth)
}

func ValidateDomainOccurrences(ctx context.Context, resolver TypeResolver, target model.TypeID, constraint model.ObjectID, layout model.TypeLayout, value model.Value, budget CallBudget, maxDepth int) error {
	if resolver == nil || target == 0 || constraint == 0 || maxDepth <= 0 {
		return sqlError("22023", "invalid domain validation options")
	}
	targetType, known := resolver.ResolveType(target)
	if !known || targetType.Kind() != model.TypeKindDomain {
		return sqlError("42804", "domain validation target is not declared")
	}
	return validateDeclaredValue(ctx, resolver, nil, targetType, constraint, layout, value, budget, maxDepth)
}

func validateInformationDomainValue(kind InformationDomainKind, value model.Value) error {
	if value.IsNull {
		return nil
	}
	switch kind {
	case InformationCardinalNumber:
		if value.Type != model.TypeInt64 || value.IntVal < 0 {
			return &model.Error{Code: "23514", Message: "value for domain violates check constraint cardinal_number_domain_check"}
		}
	case InformationYesOrNo:
		if value.Type != model.TypeString || value.StrVal != "YES" && value.StrVal != "NO" {
			return &model.Error{Code: "23514", Message: "value for domain violates check constraint yes_or_no_check"}
		}
	}
	return nil
}

func validateDeclaredValue(ctx context.Context, resolver TypeResolver, information InformationSchemaTypeResolver, target *ResolvedType, constraint model.ObjectID, layout model.TypeLayout, value model.Value, budget CallBudget, maxDepth int) error {
	var validate func(model.TypeLayout, model.Value, int) error
	validateRange := func(family RangeFamilyInfo, subtype model.TypeLayout, datum rangeDatum, depth int) error {
		if datum.empty {
			return nil
		}
		for _, bound := range [2]rangeBound{datum.lower, datum.upper} {
			if bound.infinite {
				continue
			}
			if err := validate(subtype, borrowRangeBoundValue(family, bound), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	validate = func(current model.TypeLayout, currentValue model.Value, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= maxDepth {
			return model.ErrDescriptorLimit
		}
		if currentValue.Type != current.Type.Storage {
			return sqlError("42804", "declared value has wrong storage")
		}
		var resolved *ResolvedType
		declared := false
		if resolver != nil {
			resolved, declared = resolver.ResolveType(current.Type.ID)
		}
		if declared {
			if !sameArrayConstructionLayout(current, resolved.Layout()) {
				return sqlError("42804", "declared value layout differs from its declaration")
			}
			switch resolved.Kind() {
			case model.TypeKindDomain:
				if target == nil {
					if _, err := resolved.validateDomain(ctx, currentValue, budget); err != nil {
						return err
					}
				} else if resolved.object == target.object {
					if err := resolved.validateDomainConstraint(ctx, currentValue, constraint, budget); err != nil {
						return err
					}
				}
				return validate(resolved.metadata.BaseLayout, currentValue, depth+1)
			case model.TypeKindEnum:
				if !currentValue.IsNull {
					_, err := resolved.enumOrder(currentValue)
					return err
				}
				return nil
			case model.TypeKindArray:
				if currentValue.IsNull {
					return nil
				}
				if err := model.ValidateArrayShape(currentValue.Dimensions, uint64(len(currentValue.ArrayVal))); err != nil {
					return err
				}
				for _, element := range currentValue.ArrayVal {
					if err := validate(resolved.metadata.BaseLayout, element, depth+1); err != nil {
						return err
					}
				}
				return nil
			case model.TypeKindComposite:
				if currentValue.IsNull {
					return nil
				}
				if len(currentValue.ArrayVal) != len(resolved.metadata.Fields) {
					return sqlError("42804", "composite declared value has wrong arity")
				}
				for i, field := range resolved.metadata.Fields {
					if err := validate(field.Layout, currentValue.ArrayVal[i], depth+1); err != nil {
						return err
					}
				}
				return nil
			case model.TypeKindRange, model.TypeKindMultirange:
				if currentValue.IsNull {
					return nil
				}
				family, ok := resolved.RangeFamily()
				if !ok || resolved.metadata.Range == nil {
					return model.ErrInvalidDescriptor
				}
				subtype := resolved.metadata.Range.Subtype
				if resolved.Kind() == model.TypeKindRange {
					datum, err := decodeRange(ctx, family, currentValue)
					if err != nil {
						return err
					}
					return validateRange(family, subtype, datum, depth)
				}
				cursor, err := newMultirangeCursor(family, currentValue)
				if err != nil {
					return err
				}
				for {
					datum, more, err := cursor.next(ctx)
					if err != nil || !more {
						return err
					}
					if err := validateRange(family, subtype, datum, depth); err != nil {
						return err
					}
				}
			case model.TypeKindShell:
				return sqlError("42804", "shell type has no value representation")
			default:
				return model.ErrInvalidDescriptor
			}
		}
		if information != nil {
			if binding, known := information.ResolveInformationSchemaType(current.Type.ID); known {
				if !sameArrayConstructionLayout(current, binding.Layout) {
					return sqlError("42804", "information schema value layout differs from its declaration")
				}
				switch binding.Kind {
				case model.TypeKindDomain:
					if err := validateInformationDomainValue(binding.Domain, currentValue); err != nil {
						return err
					}
					return validate(binding.BaseLayout, currentValue, depth+1)
				case model.TypeKindArray:
					if currentValue.IsNull {
						return nil
					}
					if err := model.ValidateArrayShape(currentValue.Dimensions, uint64(len(currentValue.ArrayVal))); err != nil {
						return err
					}
					for _, element := range currentValue.ArrayVal {
						if err := validate(binding.BaseLayout, element, depth+1); err != nil {
							return err
						}
					}
					return nil
				default:
					return model.ErrInvalidDescriptor
				}
			}
		}
		if !currentValue.IsNull && len(current.Children) == 0 {
			// Builtin range and multirange layouts never resolve through the
			// declared-type resolver, yet their byte storage is opaque: a value
			// claiming a range identity must actually decode as that range
			// family member rather than merely carrying TypeBytes.
			if family, known := LookupRangeFamily(current.Type.ID); known {
				subtype := model.TypeLayout{Type: model.Type{ID: family.Subtype, Storage: family.subtypeStorage(), Modifier: -1}}
				if current.Type.ID == family.Range {
					datum, err := decodeRange(ctx, family, currentValue)
					if err != nil {
						return err
					}
					return validateRange(family, subtype, datum, depth)
				}
				cursor, err := newMultirangeCursor(family, currentValue)
				if err != nil {
					return err
				}
				for {
					datum, more, err := cursor.next(ctx)
					if err != nil || !more {
						return err
					}
					if err := validateRange(family, subtype, datum, depth); err != nil {
						return err
					}
				}
			}
		}
		if currentValue.IsNull || len(current.Children) == 0 {
			return nil
		}
		switch current.Type.Storage {
		case model.TypeArray:
			if len(current.Children) != 1 {
				return sqlError("42804", "array declared value layout is invalid")
			}
			if err := model.ValidateArrayShape(currentValue.Dimensions, uint64(len(currentValue.ArrayVal))); err != nil {
				return err
			}
			for _, element := range currentValue.ArrayVal {
				if err := validate(current.Children[0], element, depth+1); err != nil {
					return err
				}
			}
		case model.TypeComposite:
			if len(current.Children) != len(currentValue.ArrayVal) {
				return sqlError("42804", "composite declared value layout differs from value")
			}
			for i := range current.Children {
				if err := validate(current.Children[i], currentValue.ArrayVal[i], depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return validate(layout, value, 0)
}

func lookupResolvedTypeIO(options ResolveOptions, layout model.TypeLayout) (TypeIO, bool) {
	if info, ok := LookupTypeIO(layout); ok {
		return info, true
	}
	if options.Types != nil {
		if resolved, ok := options.Types.ResolveType(layout.Type.ID); ok {
			info := resolved.TypeIO()
			if resolved.Kind() == model.TypeKindDomain {
				base, known := lookupResolvedTypeIO(options, resolved.BaseLayout())
				if !known {
					return TypeIO{}, false
				}
				info.Output, info.Delimiter = base.Output, base.Delimiter
			}
			return info, info.Input != 0 || info.Output != 0
		}
	}
	if options.InformationSchemaTypes != nil {
		if binding, ok := options.InformationSchemaTypes.ResolveInformationSchemaType(layout.Type.ID); ok && sameArrayConstructionLayout(layout, binding.Layout) {
			return binding.TypeIO, binding.TypeIO.Input != 0 || binding.TypeIO.Output != 0
		}
	}
	// Every composite and array datum uses PostgreSQL's shared record and array
	// representation codecs. A concrete layout therefore carries its own I/O
	// metadata even though no declaration was resolvable for its identity.
	switch layout.Type.Storage {
	case model.TypeComposite:
		if layout.Type.ID != 0 {
			return TypeIO{Input: 2290, Output: 2291, Parameter: layout.Type.ID, Delimiter: ','}, true
		}
	case model.TypeArray:
		if len(layout.Children) == 1 && layout.Children[0].Type.ID != 0 {
			return TypeIO{Input: 750, Output: 751, Parameter: layout.Children[0].Type.ID, Delimiter: ','}, true
		}
	}
	return TypeIO{}, false
}

func resolvedTypeLayout(options ResolveOptions, id model.TypeID) (model.TypeLayout, bool) {
	if options.Types != nil {
		if resolved, known := options.Types.ResolveType(id); known {
			return resolved.Layout(), true
		}
	}
	if options.InformationSchemaTypes != nil {
		if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(id); known {
			return binding.Layout, true
		}
	}
	layout := model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: primitiveStorage(id)}}
	if layout.Type.Storage != model.TypeAny {
		return layout, true
	}
	return model.TypeLayout{}, false
}

func resolveDomainOutput(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, bool, error) {
	if len(arguments) != 1 {
		return Callable{}, false, nil
	}
	var base, declaration model.TypeLayout
	if resolved, ok := resolvedType(options, arguments[0].Type.ID, model.TypeKindDomain); ok {
		base, declaration = resolved.metadata.BaseLayout, resolved.metadata.Layout
	} else {
		if options.InformationSchemaTypes == nil {
			return Callable{}, false, nil
		}
		binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(arguments[0].Type.ID)
		if !known || binding.Kind != model.TypeKindDomain {
			return Callable{}, false, nil
		}
		base, declaration = binding.BaseLayout, binding.Layout
	}
	if !sameArrayConstructionLayout(arguments[0], declaration) {
		return Callable{}, true, sqlError("42804", "domain output layout mismatch")
	}
	baseIO, known := lookupResolvedTypeIO(options, base)
	if !known || baseIO.Output != signature.ID {
		return Callable{}, false, nil
	}
	callable, err := ResolveTyped(ctx, signature.ID, []model.TypeLayout{base}, result, options, maximum)
	if err != nil {
		return Callable{}, true, err
	}
	callable.signature.Arguments[0] = arguments[0].Type.ID
	callable.argumentStorage[0] = arguments[0].Type.Storage
	return callable, true, nil
}

func resolveInputCallable(ctx context.Context, info TypeIO, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	signature, ok := LookupSignature(info.Input)
	if !ok {
		return Callable{}, &UnknownFunctionError{ID: info.Input}
	}
	arguments := make([]model.TypeLayout, signature.ArgumentCount)
	arguments[0] = model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	for i := 1; i < int(signature.ArgumentCount); i++ {
		arguments[i] = model.TypeLayout{Type: model.Type{ID: signature.Arguments[i], Modifier: -1, Storage: model.TypeInt64}}
	}
	return ResolveTyped(ctx, info.Input, arguments, result, options, maximum)
}

// ResolveTypeTextCodec resolves descriptor-backed and built-in text semantics.
func ResolveTypeTextCodec(ctx context.Context, layout model.TypeLayout, options ResolveOptions, maximum uint64) (TypeTextCodec, error) {
	if err := layout.Validate(); err != nil {
		return TypeTextCodec{}, err
	}
	info, known := lookupResolvedTypeIO(options, layout)
	if !known || info.Input == 0 || info.Output == 0 {
		return TypeTextCodec{}, sqlError("42804", "type has no resolved text IO metadata")
	}
	input, err := resolveInputCallable(ctx, info, layout, options, maximum)
	if err != nil {
		return TypeTextCodec{}, err
	}
	if input.OwnedBytes() > maximum {
		return TypeTextCodec{}, sqlError("54000", "type text codec metadata budget exceeded")
	}
	outputLayout := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	output, err := ResolveTyped(ctx, info.Output, []model.TypeLayout{layout}, outputLayout, options, maximum-input.OwnedBytes())
	if err != nil {
		return TypeTextCodec{}, err
	}
	codec := TypeTextCodec{OwnedBytes: input.OwnedBytes() + output.OwnedBytes()}
	codec.Input = func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		if len(values) != 1 || values[0].Type != model.TypeString {
			return model.Value{}, sqlError("42804", "type text input requires one string")
		}
		var arguments [3]model.Value
		count := int(input.ArgumentCount())
		arguments[0] = values[0]
		if count >= 2 {
			arguments[1] = model.Int64Val(int64(info.Parameter))
		}
		if count == 3 {
			arguments[2] = model.Int64Val(int64(layout.Type.Modifier))
		}
		return input.Call(ctx, arguments[:count], budget)
	}
	codec.Output = func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		if len(values) != 1 {
			return model.Value{}, sqlError("42804", "type text output requires one value")
		}
		return output.Call(ctx, values, budget)
	}
	return codec, ctx.Err()
}
