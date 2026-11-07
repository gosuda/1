package catalogrel

import (
	"context"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func emitBuiltinCollations(relation Relation, options SourceOptions, emit rowEmitter) error {
	for info := range builtin.Collations() {
		fields := []fieldValue{
			fv("oid", intValue(int64(info.ID))), fv("collname", stringValue(info.Name)), fv("collnamespace", oidValue(11)), fv("collowner", oidValue(10)),
			fv("collprovider", charValue(info.Provider)), fv("collisdeterministic", boolValue(info.Deterministic)), fv("collencoding", intValue(int64(info.Encoding))),
		}
		collate, characterType, locale, version := builtinCollationDetails(info)
		if collate != "" {
			fields = append(fields, fv("collcollate", stringValue(collate)))
		}
		if characterType != "" {
			fields = append(fields, fv("collctype", stringValue(characterType)))
		}
		if locale != "" {
			fields = append(fields, fv("colllocale", stringValue(locale)))
		}
		if version != "" {
			fields = append(fields, fv("collversion", stringValue(version)))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}
func builtinCollationDetails(info builtin.CollationInfo) (collate, characterType, locale, version string) {
	switch info.Provider {
	case 'c':
		return info.Name, info.Name, "", ""
	case 'b':
		switch info.Name {
		case "pg_c_utf8":
			locale = "C.UTF-8"
		case "ucs_basic":
			locale = "C"
		case "pg_unicode_fast":
			locale = "PG_UNICODE_FAST"
		}
		if locale != "" {
			version = "1"
		}
	case 'i':
		if info.Name == "unicode" {
			locale = "und"
		}
	}
	return collate, characterType, locale, version
}


func emitOperators(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitBuiltinOperators(relation, options, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectOperator}, func(object objectDescriptor) error {
		operator := object.spec.Operator
		if operator == nil {
			return nil
		}
		kind := byte('b')
		if operator.Left.Type.ID == 0 {
			kind = 'l'
		} else if operator.Right.Type.ID == 0 {
			kind = 'r'
		}
		return emitCatalogRow(relation, emit,
			fv("oid", oidValue(object.spec.OID)), fv("oprname", stringValue(operator.Name)), fv("oprnamespace", oidValue(object.schema.oid)), fv("oprowner", oidValue(operator.Owner)),
			fv("oprkind", charValue(kind)), fv("oprcanmerge", boolValue(operator.CanMerge)), fv("oprcanhash", boolValue(operator.CanHash)),
			fv("oprleft", oidValue(model.OID(operator.Left.Type.ID))), fv("oprright", oidValue(model.OID(operator.Right.Type.ID))), fv("oprresult", oidValue(model.OID(operator.Result.Type.ID))),
			fv("oprcom", oidValue(model.OID(operator.Commutator))), fv("oprnegate", oidValue(model.OID(operator.Negator))), fv("oprcode", oidValue(model.OID(operator.Function))),
			fv("oprrest", oidValue(model.OID(operator.Restrict))), fv("oprjoin", oidValue(model.OID(operator.Join))))
	})
}

func emitBuiltinOperators(relation Relation, options SourceOptions, emit rowEmitter) error {
	if !builtin.OperatorCatalogMetadataReady() {
		return &model.Error{Code: "55000", Message: "operator catalog metadata requires regeneration"}
	}
	for operator := range builtin.Operators() {
		kind := byte('b')
		if operator.Left == 0 {
			kind = 'l'
		} else if operator.Right == 0 {
			kind = 'r'
		}
		if err := emitCatalogRow(relation, emit,
			fv("oid", intValue(int64(operator.ID))), fv("oprname", stringValue(operator.Name)), fv("oprnamespace", oidValue(11)), fv("oprowner", oidValue(10)),
			fv("oprkind", charValue(kind)), fv("oprcanmerge", boolValue(operator.MergeJoinable)), fv("oprcanhash", boolValue(operator.HashJoinable)),
			fv("oprleft", intValue(int64(operator.Left))), fv("oprright", intValue(int64(operator.Right))), fv("oprresult", intValue(int64(operator.Result))),
			fv("oprcom", intValue(int64(operator.Commutator))), fv("oprnegate", intValue(int64(operator.Negator))), fv("oprcode", intValue(int64(operator.Function))),
			fv("oprrest", intValue(int64(operator.Restriction))), fv("oprjoin", intValue(int64(operator.Join)))); err != nil {
			return err
		}
	}
	return nil
}

func emitCasts(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitBuiltinCasts(relation, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectCast}, func(object objectDescriptor) error {
		cast := object.spec.Cast
		if cast == nil {
			return nil
		}
		return emitCatalogRow(relation, emit,
			fv("oid", oidValue(object.spec.OID)), fv("castsource", oidValue(model.OID(cast.Source.Type.ID))), fv("casttarget", oidValue(model.OID(cast.Target.Type.ID))),
			fv("castfunc", oidValue(model.OID(cast.Function))), fv("castcontext", charValue(cast.Context)), fv("castmethod", charValue(cast.Method)))
	})
}

func emitBuiltinCasts(relation Relation, emit rowEmitter) error {
	for cast := range builtin.Casts() {
		if cast.OID == 0 {
			return &model.Error{Code: "55000", Message: "cast catalog metadata requires regeneration"}
		}
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(cast.OID)), fv("castsource", intValue(int64(cast.Source))), fv("casttarget", intValue(int64(cast.Target))),
			fv("castfunc", intValue(int64(cast.Function))), fv("castcontext", charValue(cast.Context)), fv("castmethod", charValue(cast.Method))); err != nil {
			return err
		}
	}
	return nil
}

func emitRanges(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitBuiltinRanges(relation, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		metadata := object.spec.Type
		if metadata == nil || metadata.DeclaredKind() != model.TypeKindRange {
			return nil
		}
		if metadata.Range == nil {
			return &model.Error{Code: "XX000", Message: "range type has no catalog metadata"}
		}
		multirange, err := referencedCatalogOID(ctx, snapshot, options, metadata.Range.Multirange)
		if err != nil {
			return err
		}
		canonical := model.OID(metadata.Range.CanonicalFunction)
		if metadata.Range.Canonical != 0 {
			canonical, err = referencedCatalogOID(ctx, snapshot, options, metadata.Range.Canonical)
			if err != nil {
				return err
			}
		}
		difference := model.OID(metadata.Range.DifferenceFunction)
		if metadata.Range.Difference != 0 {
			difference, err = referencedCatalogOID(ctx, snapshot, options, metadata.Range.Difference)
			if err != nil {
				return err
			}
		}
		return emitCatalogRow(relation, emit,
			fv("rngtypid", oidValue(object.spec.OID)),
			fv("rngsubtype", intValue(int64(metadata.Range.Subtype.Type.ID))),
			fv("rngmultitypid", oidValue(multirange)),
			fv("rngcollation", intValue(int64(metadata.Range.Collation))),
			fv("rngsubopc", oidValue(metadata.Range.OperatorClass)),
			fv("rngcanonical", oidValue(canonical)),
			fv("rngsubdiff", oidValue(difference)),
		)
	})
}

func emitBuiltinRanges(relation Relation, emit rowEmitter) error {
	for family := range builtin.RangeFamilies() {
		if err := emitCatalogRow(relation, emit,
			fv("rngtypid", intValue(int64(family.Range))), fv("rngsubtype", intValue(int64(family.Subtype))), fv("rngmultitypid", intValue(int64(family.Multirange))),
			fv("rngcollation", intValue(int64(family.Collation))), fv("rngsubopc", oidValue(family.SubtypeClass)), fv("rngcanonical", intValue(int64(family.Canonical))), fv("rngsubdiff", intValue(int64(family.Difference)))); err != nil {
			return err
		}
	}
	return nil
}

func emitTypes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if !builtin.TypeCatalogMetadataReady() {
		return &model.Error{Code: "55000", Message: "type catalog metadata requires regeneration"}
	}
	informationSchema, err := informationSchemaIdentity(options)
	if err != nil {
		return err
	}
	logicalRelations := make(map[model.OID]RelationIdentity, len(registry))
	for _, logical := range registry {
		identity, _ := options.Registry.Identity(logical.ID)
		logicalRelations[identity.RowTypeOID] = identity
	}
	for info := range builtin.Types() {
		logical := logicalRelations[model.OID(info.ID)]
		if err := emitTypeRow(relation, emit, 10, model.OID(info.ID), info.Name, 11, logical.RelationOID, info.Kind, info.Category, info.Preferred, info.Delimiter, info.Element, info.Array, info.Input, info.Output, info.Receive, info.Send, info.ModifierInput, info.ModifierOutput, info.Subscript, info.Analyze, info.Base, info.Collation, false, -1); err != nil {
			return err
		}
	}
	for _, logical := range registry {
		identity, _ := options.Registry.Identity(logical.ID)
		if _, builtIn := builtin.LookupType(model.TypeID(identity.RowTypeOID)); builtIn {
			continue
		}
		namespace := model.OID(11)
		if logical.Namespace == "information_schema" {
			namespace = informationSchema.OID
		}
		if err := emitTypeRow(relation, emit, 10, identity.RowTypeOID, logical.Name, namespace, identity.RelationOID, 'c', 'C', false, ',', 0, model.TypeID(identity.ArrayTypeOID), 2290, 2291, 2402, 2403, 0, 0, 0, 0, 0, 0, false, -1); err != nil {
			return err
		}
		_, arrayName := registryTypeNames(logical)
		if err := emitTypeRow(relation, emit, 10, identity.ArrayTypeOID, arrayName, namespace, 0, 'b', 'A', false, ',', model.TypeID(identity.RowTypeOID), 0, 750, 751, 2400, 2401, 0, 0, 6179, 3816, 0, 0, false, -1); err != nil {
			return err
		}
	}
	for _, registered := range options.Registry.InformationDomains() {
		domain := registered.Definition
		base, ok := builtin.LookupType(domain.Base.Type.ID)
		if !ok {
			return &model.Error{Code: "XX000", Message: "information schema domain has unknown base type", Detail: domain.Name}
		}
		fields := []fieldValue{
			fv("oid", oidValue(registered.Identity.TypeOID)), fv("typname", stringValue(domain.Name)), fv("typnamespace", oidValue(informationSchema.OID)), fv("typowner", oidValue(10)),
			fv("typtype", charValue('d')), fv("typcategory", charValue(base.Category)), fv("typispreferred", boolValue(false)), fv("typisdefined", boolValue(true)), fv("typdelim", charValue(base.Delimiter)),
			fv("typrelid", oidValue(0)), fv("typsubscript", oidValue(0)), fv("typelem", oidValue(0)), fv("typarray", oidValue(registered.Identity.ArrayTypeOID)),
			fv("typinput", oidValue(2597)), fv("typoutput", oidValue(model.OID(base.Output))), fv("typreceive", oidValue(2598)), fv("typsend", oidValue(model.OID(base.Send))),
			fv("typmodin", oidValue(0)), fv("typmodout", oidValue(0)), fv("typanalyze", oidValue(model.OID(base.Analyze))), fv("typnotnull", boolValue(domain.NotNull)),
			fv("typbasetype", intValue(int64(domain.Base.Type.ID))), fv("typtypmod", intValue(int64(domain.Base.Type.Modifier))), fv("typndims", intValue(0)),
			fv("typcollation", intValue(int64(domain.Base.Type.Collation))),
		}
		if domain.DefaultSQL != "" {
			fields = append(fields, fv("typdefaultbin", stringValue(domain.DefaultSQL)), fv("typdefault", stringValue(domain.DefaultSQL)))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
		if err := emitTypeRow(relation, emit, 10, registered.Identity.ArrayTypeOID, "_"+domain.Name, informationSchema.OID, 0, 'b', 'A', false, base.Delimiter, model.TypeID(registered.Identity.TypeOID), 0, 750, 751, 2400, 2401, 0, 0, 6179, 3816, 0, domain.Base.Type.Collation, false, -1); err != nil {
			return err
		}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		metadata := object.spec.Type
		if metadata == nil {
			return nil
		}
		kind := byte('b')
		declaredKind := metadata.DeclaredKind()
		category := byte('U')
		base := model.TypeID(0)
		element := model.TypeID(0)
		relationOID := model.OID(0)
		defined := true
		var err error
		switch declaredKind {
		case model.TypeKindDomain:
			kind, base = 'd', metadata.BaseLayout.Type.ID
		case model.TypeKindEnum:
			kind, category = 'e', 'E'
		case model.TypeKindComposite:
			kind, category = 'c', 'C'
			relationOID, err = typeRelationCatalogOID(ctx, snapshot, options, object.spec.ID, metadata.Relation)
			if err != nil {
				return err
			}
		case model.TypeKindRange:
			kind, category = 'r', 'R'
		case model.TypeKindMultirange:
			kind, category = 'm', 'R'
		case model.TypeKindArray:
			category, element = 'A', metadata.BaseLayout.Type.ID
		case model.TypeKindShell:
			kind, category, defined = 'p', 'P', false
		}
		arrayOID, err := referencedCatalogOID(ctx, snapshot, options, metadata.Array)
		if err != nil {
			return err
		}
		functions, err := customTypeCatalogFunctions(ctx, snapshot, options, metadata)
		if err != nil {
			return err
		}
		typeModifier := int32(-1)
		if declaredKind == model.TypeKindDomain {
			typeModifier = metadata.BaseLayout.Type.Modifier
		}
		fields := []fieldValue{
			fv("oid", oidValue(object.spec.OID)), fv("typname", stringValue(object.spec.Name.Name)), fv("typnamespace", oidValue(object.schema.oid)), fv("typowner", oidValue(metadata.Owner)),
			fv("typtype", charValue(kind)), fv("typcategory", charValue(category)), fv("typispreferred", boolValue(false)), fv("typisdefined", boolValue(defined)), fv("typdelim", charValue(',')),
			fv("typrelid", oidValue(relationOID)), fv("typsubscript", oidValue(functions.subscript)), fv("typelem", intValue(int64(element))), fv("typarray", oidValue(arrayOID)),
			fv("typinput", oidValue(functions.input)), fv("typoutput", oidValue(functions.output)), fv("typreceive", oidValue(functions.receive)), fv("typsend", oidValue(functions.send)),
			fv("typmodin", oidValue(0)), fv("typmodout", oidValue(0)), fv("typanalyze", oidValue(functions.analyze)), fv("typnotnull", boolValue(metadata.NotNull)), fv("typbasetype", intValue(int64(base))),
			fv("typtypmod", intValue(int64(typeModifier))), fv("typndims", intValue(0)), fv("typcollation", intValue(int64(metadata.Layout.Type.Collation))),
		}
		if metadata.Default != nil {
			fields = append(fields, fv("typdefaultbin", stringValue(metadata.Default.SQL)), fv("typdefault", stringValue(metadata.Default.SQL)))
		}
		return emitCatalogRow(relation, emit, fields...)
	})
}

func emitTypeRow(relation Relation, emit rowEmitter, owner, oid model.OID, name string, namespace, relationOID model.OID, kind, category byte, preferred bool, delimiter byte, element, array model.TypeID, input, output, receive, send, modifierInput, modifierOutput, subscript, analyze model.FunctionID, base model.TypeID, collation model.CollationID, notNull bool, typeModifier int32) error {
	return emitCatalogRow(relation, emit,
		fv("oid", oidValue(oid)), fv("typname", stringValue(name)), fv("typnamespace", oidValue(namespace)), fv("typowner", oidValue(owner)),
		fv("typtype", charValue(kind)), fv("typcategory", charValue(category)), fv("typispreferred", boolValue(preferred)), fv("typisdefined", boolValue(true)), fv("typdelim", charValue(delimiter)),
		fv("typrelid", oidValue(relationOID)), fv("typsubscript", intValue(int64(subscript))), fv("typelem", intValue(int64(element))), fv("typarray", intValue(int64(array))),
		fv("typinput", intValue(int64(input))), fv("typoutput", intValue(int64(output))), fv("typreceive", intValue(int64(receive))), fv("typsend", intValue(int64(send))),
		fv("typmodin", intValue(int64(modifierInput))), fv("typmodout", intValue(int64(modifierOutput))), fv("typanalyze", intValue(int64(analyze))), fv("typnotnull", boolValue(notNull)), fv("typbasetype", intValue(int64(base))),
		fv("typtypmod", intValue(int64(typeModifier))), fv("typndims", intValue(0)), fv("typcollation", intValue(int64(collation))))
}
type typeCatalogFunctions struct {
	input     model.OID
	output    model.OID
	receive   model.OID
	send      model.OID
	subscript model.OID
	analyze   model.OID
}

func referencedCatalogOID(ctx context.Context, snapshot Snapshot, options SourceOptions, id model.ObjectID) (model.OID, error) {
	if id == 0 {
		return 0, nil
	}
	object, public, err := objectByID(ctx, snapshot, options, id)
	if err != nil {
		return 0, err
	}
	if !public || object.spec.OID == 0 {
		return 0, &model.Error{Code: "55000", Message: "referenced catalog object is not public"}
	}
	return object.spec.OID, nil
}

func typeRelationCatalogOID(ctx context.Context, snapshot Snapshot, options SourceOptions, typeID, relationID model.ObjectID) (model.OID, error) {
	if relationID == 0 {
		return 0, &model.Error{Code: "55000", Message: "composite type relation metadata is unavailable"}
	}
	published, err := snapshot.Descriptor(ctx, relationID)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.OID() == 0 {
		return 0, &model.Error{Code: "XX000", Message: "composite type relation descriptor is unavailable"}
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return 0, err
	}
	rowType := model.ObjectID(0)
	switch {
	case spec.Table != nil:
		rowType = spec.Table.RowType
	case spec.View != nil:
		rowType = spec.View.RowType
	case spec.CompositeRelation != nil:
		rowType = spec.CompositeRelation.RowType
	default:
		return 0, &model.Error{Code: "XX000", Message: "composite type relation descriptor has invalid kind"}
	}
	if rowType != typeID {
		return 0, &model.Error{Code: "XX000", Message: "composite type relation descriptor is inconsistent"}
	}
	return spec.OID, nil
}

func customTypeCatalogFunctions(ctx context.Context, snapshot Snapshot, options SourceOptions, metadata *model.TypeMetadata) (typeCatalogFunctions, error) {
	switch metadata.DeclaredKind() {
	case model.TypeKindDomain:
		functions := typeCatalogFunctions{input: 2597, receive: 2598}
		if base, ok := builtin.LookupType(metadata.BaseLayout.Type.ID); ok {
			functions.output = model.OID(base.Output)
			functions.send = model.OID(base.Send)
			functions.analyze = model.OID(base.Analyze)
			return functions, nil
		}
		base, public, err := objectByID(ctx, snapshot, options, metadata.Base)
		if err != nil {
			return typeCatalogFunctions{}, err
		}
		if !public || base.spec.Type == nil || base.spec.Type.Kind == model.TypeKindDomain {
			return typeCatalogFunctions{}, &model.Error{Code: "55000", Message: "domain base type has no catalog functions"}
		}
		baseFunctions, err := customTypeCatalogFunctions(ctx, snapshot, options, base.spec.Type)
		if err != nil {
			return typeCatalogFunctions{}, err
		}
		functions.output, functions.send, functions.analyze = baseFunctions.output, baseFunctions.send, baseFunctions.analyze
		return functions, nil
	case model.TypeKindEnum:
		return typeCatalogFunctions{input: 3506, output: 3507, receive: 3532, send: 3533}, nil
	case model.TypeKindComposite:
		return typeCatalogFunctions{input: 2290, output: 2291, receive: 2402, send: 2403}, nil
	case model.TypeKindRange:
		return typeCatalogFunctions{input: 3834, output: 3835, receive: 3836, send: 3837, analyze: 3916}, nil
	case model.TypeKindMultirange:
		return typeCatalogFunctions{input: 4231, output: 4232, receive: 4233, send: 4234, analyze: 4242}, nil
	case model.TypeKindArray:
		return typeCatalogFunctions{input: 750, output: 751, receive: 2400, send: 2401, subscript: 6179, analyze: 3816}, nil
	case model.TypeKindShell:
		return typeCatalogFunctions{input: 2398, output: 2399}, nil
	default:
		return typeCatalogFunctions{}, &model.Error{Code: "XX000", Message: "unknown custom type kind"}
	}
}

func emitEnums(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		metadata := object.spec.Type
		if metadata == nil || len(metadata.Elements) == 0 {
			return nil
		}
		if len(metadata.ElementOIDs) != len(metadata.Elements) {
			return &model.Error{Code: "XX000", Message: "enum labels have no stable SQL catalog identities", Detail: object.schema.name + "." + object.spec.Name.Name}
		}
		for i, label := range metadata.Elements {
			if err := emitCatalogRow(relation, emit, fv("oid", oidValue(metadata.ElementOIDs[i])), fv("enumtypid", oidValue(object.spec.OID)), fv("enumsortorder", floatValue(float64(i+1))), fv("enumlabel", stringValue(label))); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitProcedures(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitBuiltinProcedures(relation, options, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectRoutine}, func(object objectDescriptor) error {
		switch {
		case object.spec.Routine != nil:
			return emitRoutineProcedure(ctx, snapshot, relation, options, object, emit)
		case object.spec.Aggregate != nil:
			return emitAggregateProcedure(relation, object, emit)
		default:
			return nil
		}
	})
}

func emitBuiltinProcedures(relation Relation, options SourceOptions, emit rowEmitter) error {
	for signature := range builtin.Signatures() {
		if err := emitBuiltinProcedure(relation, signature, 11, 0, emit); err != nil {
			return err
		}
	}
	informationSchema, err := informationSchemaIdentity(options)
	if err != nil {
		return err
	}
	for _, function := range options.Registry.InformationSchemaFunctions() {
		if err := emitBuiltinProcedure(relation, function.Signature, informationSchema.OID, function.Operation, emit); err != nil {
			return err
		}
	}
	return nil
}

func emitBuiltinProcedure(relation Relation, signature builtin.Signature, namespace model.OID, informationOperation builtin.InformationSchemaFunction, emit rowEmitter) error {
	if signature.Language == 0 || signature.Parallel == 0 || (signature.Source == "" && informationOperation == 0) {
		return &model.Error{Code: "55000", Message: "builtin procedure catalog metadata requires regeneration", Detail: signature.Name}
	}
	arguments := make([]int64, signature.ArgumentCount)
	for i := range int(signature.ArgumentCount) {
		arguments[i] = int64(signature.Arguments[i])
	}
	defaults := 0
	var argumentNames []string
	var allArgumentTypes []int64
	var allArgumentModes, allArgumentNames []string
	allArgumentTypesPresent, allArgumentModesPresent, allArgumentNamesPresent := false, false, false
	if parameters, ok := builtin.LookupFunctionParameters(signature.ID); ok {
		defaults = parameters.ArgumentCount() - parameters.RequiredCount()
		allArgumentTypesPresent = parameters.HasAllArgumentTypes()
		allArgumentModesPresent = parameters.HasAllArgumentModes()
		allArgumentNamesPresent = parameters.HasAllArgumentNames()
		for i := range parameters.ArgumentCount() {
			name, _ := parameters.ArgumentName(i)
			argumentNames = append(argumentNames, name)
		}
		for i := range parameters.AllArgumentCount() {
			typeID, mode, name, _ := parameters.AllArgument(i)
			allArgumentTypes = append(allArgumentTypes, int64(typeID))
			allArgumentModes = append(allArgumentModes, string([]byte{mode}))
			allArgumentNames = append(allArgumentNames, name)
		}
	}
	fields := []fieldValue{
		fv("oid", intValue(int64(signature.ID))), fv("proname", stringValue(signature.Name)), fv("pronamespace", oidValue(namespace)), fv("proowner", oidValue(10)),
		fv("prolang", oidValue(signature.Language)), fv("procost", floatValue(signature.Cost)), fv("prorows", floatValue(signature.Rows)), fv("provariadic", intValue(int64(signature.Variadic))), fv("prosupport", oidValue(model.OID(signature.Support))),
		fv("prokind", charValue(signature.Kind)), fv("prosecdef", boolValue(signature.SecurityDefiner)), fv("proleakproof", boolValue(signature.Leakproof)), fv("proisstrict", boolValue(signature.Strict)), fv("proretset", boolValue(signature.ReturnsSet)),
		fv("provolatile", charValue(signature.Volatility)), fv("proparallel", charValue(signature.Parallel)), fv("pronargs", intValue(int64(signature.ArgumentCount))), fv("pronargdefaults", intValue(int64(defaults))),
		fv("prorettype", intValue(int64(signature.Result))), fv("proargtypes", intArray(arguments)), fv("prosrc", stringValue(signature.Source)),
	}
	if len(argumentNames) == 0 && len(allArgumentTypes) == 0 {
		if names, count, ok := builtin.InformationSchemaInputArguments(informationOperation); ok {
			argumentNames = make([]string, count)
			copy(argumentNames, names[:count])
		}
	}
	if len(argumentNames) != 0 && !allArgumentNamesPresent {
		fields = append(fields, fv("proargnames", stringArray(argumentNames)))
	}
	if defaults != 0 {
		defaultSQL := make([]string, 0, defaults)
		parameters, _ := builtin.LookupFunctionParameters(signature.ID)
		for i := parameters.RequiredCount(); i < parameters.ArgumentCount(); i++ {
			sql, _ := parameters.DefaultSQL(i)
			defaultSQL = append(defaultSQL, sql)
		}
		fields = append(fields, fv("proargdefaults", stringValue(strings.Join(defaultSQL, ", "))))
	}
	if allArgumentTypesPresent {
		fields = append(fields, fv("proallargtypes", oidArray(allArgumentTypes)))
	}
	if allArgumentModesPresent {
		fields = append(fields, fv("proargmodes", stringArray(allArgumentModes)))
	}
	if allArgumentNamesPresent {
		fields = append(fields, fv("proargnames", stringArray(allArgumentNames)))
	}
	if len(allArgumentTypes) == 0 {
		if allTypes, modes, names, count, ok := builtin.InformationSchemaOutputArguments(informationOperation); ok {
			types := make([]int64, count)
			modeNames := make([]string, count)
			argumentNames := make([]string, count)
			for i := range int(count) {
				types[i] = int64(allTypes[i])
				modeNames[i] = string([]byte{modes[i]})
				argumentNames[i] = names[i]
			}
			fields = append(fields, fv("proallargtypes", oidArray(types)), fv("proargmodes", stringArray(modeNames)), fv("proargnames", stringArray(argumentNames)))
		}
	}
	return emitCatalogRow(relation, emit, fields...)
}
func emitRoutineProcedure(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, object objectDescriptor, emit rowEmitter) error {
	routine := object.spec.Routine
	inputTypes := make([]int64, 0, len(routine.Parameters))
	allTypes := make([]int64, len(routine.Parameters))
	allModes := make([]string, len(routine.Parameters))
	allNames := make([]string, len(routine.Parameters))
	defaults := make([]string, 0, len(routine.Parameters))
	hasOutput, hasNames := false, false
	var variadic model.TypeID
	for i, parameter := range routine.Parameters {
		allTypes[i] = int64(parameter.Layout.Type.ID)
		allModes[i] = string([]byte{routineParameterMode(parameter.Mode)})
		allNames[i] = parameter.Name
		hasNames = hasNames || parameter.Name != ""
		input := parameter.Mode == "in" || parameter.Mode == "inout" || parameter.Mode == "variadic"
		hasOutput = hasOutput || !input || parameter.Mode == "inout"
		if input {
			inputTypes = append(inputTypes, int64(parameter.Layout.Type.ID))
		}
		if parameter.Mode == "variadic" {
			if len(parameter.Layout.Children) != 1 {
				return &model.Error{Code: "XX000", Message: "variadic routine parameter has no element type"}
			}
			variadic = parameter.Layout.Children[0].Type.ID
		}
		if parameter.Default != nil {
			defaults = append(defaults, parameter.Default.SQL)
		}
	}
	support := model.OID(routine.SupportFunction)
	if routine.Support != 0 {
		var err error
		support, err = referencedCatalogOID(ctx, snapshot, options, routine.Support)
		if err != nil {
			return err
		}
	}
	kind := byte('f')
	if routine.Procedure {
		kind = 'p'
	}
	source := routine.Source
	if source == "" {
		source = routine.ExternalCode
	}
	fields := []fieldValue{
		fv("oid", oidValue(object.spec.OID)), fv("proname", stringValue(object.spec.Name.Name)), fv("pronamespace", oidValue(object.schema.oid)), fv("proowner", oidValue(routine.Owner)),
		fv("prolang", oidValue(routine.LanguageOID)), fv("procost", floatValue(routine.Cost)), fv("prorows", floatValue(routine.Rows)), fv("provariadic", intValue(int64(variadic))), fv("prosupport", oidValue(support)),
		fv("prokind", charValue(kind)), fv("prosecdef", boolValue(routine.SecurityDefiner)), fv("proleakproof", boolValue(routine.Leakproof)), fv("proisstrict", boolValue(routine.Strict)), fv("proretset", boolValue(routine.ReturnsSet)),
		fv("provolatile", charValue(routineVolatility(routine.Volatility))), fv("proparallel", charValue(routineParallel(routine.Parallel))), fv("pronargs", intValue(int64(len(inputTypes)))), fv("pronargdefaults", intValue(int64(len(defaults)))),
		fv("prorettype", intValue(int64(routine.Result.Type.ID))), fv("proargtypes", intArray(inputTypes)), fv("prosrc", stringValue(source)),
	}
	if hasOutput {
		fields = append(fields, fv("proallargtypes", oidArray(allTypes)), fv("proargmodes", stringArray(allModes)))
	}
	if hasNames {
		fields = append(fields, fv("proargnames", stringArray(allNames)))
	}
	if len(defaults) != 0 {
		fields = append(fields, fv("proargdefaults", stringValue(strings.Join(defaults, ", "))))
	}
	if routine.Binary != "" {
		fields = append(fields, fv("probin", stringValue(routine.Binary)))
	}
	if routine.Atomic {
		fields = append(fields, fv("prosqlbody", stringValue(routine.Body.SQL)))
	}
	if len(routine.Settings) != 0 {
		settings := make([]string, len(routine.Settings))
		for i, setting := range routine.Settings {
			value := setting.Value
			if setting.Reset {
				value = "default"
			}
			settings[i] = setting.Name + "=" + value
		}
		fields = append(fields, fv("proconfig", stringArray(settings)))
	}
	return emitCatalogRow(relation, emit, fields...)
}

func emitAggregateProcedure(relation Relation, object objectDescriptor, emit rowEmitter) error {
	aggregate := object.spec.Aggregate
	arguments := make([]int64, len(aggregate.Arguments))
	for i := range aggregate.Arguments {
		arguments[i] = int64(aggregate.Arguments[i].Type.ID)
	}
	fields := []fieldValue{
		fv("oid", oidValue(object.spec.OID)), fv("proname", stringValue(object.spec.Name.Name)), fv("pronamespace", oidValue(object.schema.oid)), fv("proowner", oidValue(aggregate.Owner)),
		fv("prolang", oidValue(12)), fv("procost", floatValue(1)), fv("prorows", floatValue(0)), fv("provariadic", oidValue(model.OID(aggregate.Variadic))), fv("prosupport", oidValue(0)),
		fv("prokind", charValue('a')), fv("prosecdef", boolValue(false)), fv("proleakproof", boolValue(false)), fv("proisstrict", boolValue(false)), fv("proretset", boolValue(false)),
		fv("provolatile", charValue(routineVolatility(aggregate.Volatility))), fv("proparallel", charValue(routineParallel(aggregate.Parallel))), fv("pronargs", intValue(int64(len(arguments)))), fv("pronargdefaults", intValue(0)),
		fv("prorettype", oidValue(model.OID(aggregate.Result.Type.ID))), fv("proargtypes", intArray(arguments)), fv("prosrc", stringValue("aggregate_dummy")),
	}
	if len(aggregate.ArgumentNames) != 0 {
		fields = append(fields, fv("proargnames", stringArray(aggregate.ArgumentNames)))
	}
	return emitCatalogRow(relation, emit, fields...)
}

func routineParameterMode(mode string) byte {
	switch mode {
	case "in":
		return 'i'
	case "out":
		return 'o'
	case "inout":
		return 'b'
	case "variadic":
		return 'v'
	case "table":
		return 't'
	default:
		return 0
	}
}

func routineVolatility(volatility string) byte {
	switch volatility {
	case "immutable":
		return 'i'
	case "stable":
		return 's'
	default:
		return 'v'
	}
}

func routineParallel(parallel string) byte {
	switch parallel {
	case "safe":
		return 's'
	case "restricted":
		return 'r'
	default:
		return 'u'
	}
}

func emitAggregates(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitBuiltinAggregates(relation, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectRoutine}, func(object objectDescriptor) error {
		aggregate := object.spec.Aggregate
		if aggregate == nil {
			return nil
		}
		kind, ok := aggregateCatalogKind(aggregate.Kind)
		if !ok {
			return &model.Error{Code: "XX000", Message: "aggregate kind metadata is invalid"}
		}
		finalModify, ok := aggregateCatalogFinalModify(aggregate.FinalModify)
		if !ok {
			return &model.Error{Code: "XX000", Message: "aggregate final modification metadata is invalid"}
		}
		movingFinalModify := byte('r')
		if aggregate.MovingFinalModify != 0 {
			movingFinalModify, ok = aggregateCatalogFinalModify(aggregate.MovingFinalModify)
			if !ok {
				return &model.Error{Code: "XX000", Message: "aggregate moving-final modification metadata is invalid"}
			}
		}
		fields := []fieldValue{
			fv("aggfnoid", oidValue(object.spec.OID)), fv("aggkind", charValue(kind)), fv("aggnumdirectargs", intValue(int64(aggregate.DirectArguments))),
			fv("aggtransfn", oidValue(model.OID(aggregate.Transition))), fv("aggfinalfn", oidValue(model.OID(aggregate.Final))), fv("aggcombinefn", oidValue(model.OID(aggregate.Combine))),
			fv("aggserialfn", oidValue(model.OID(aggregate.Serial))), fv("aggdeserialfn", oidValue(model.OID(aggregate.Deserial))),
			fv("aggmtransfn", oidValue(model.OID(aggregate.MovingTransition))), fv("aggminvtransfn", oidValue(model.OID(aggregate.MovingInverse))), fv("aggmfinalfn", oidValue(model.OID(aggregate.MovingFinal))),
			fv("aggfinalextra", boolValue(aggregate.FinalExtra)), fv("aggmfinalextra", boolValue(aggregate.MovingFinalExtra)),
			fv("aggfinalmodify", charValue(finalModify)), fv("aggmfinalmodify", charValue(movingFinalModify)),
			fv("aggsortop", oidValue(model.OID(aggregate.SortOperator))), fv("aggtranstype", oidValue(model.OID(aggregate.TransitionLayout.Type.ID))),
			fv("aggtransspace", intValue(int64(aggregate.TransitionSpace))), fv("aggmtranstype", oidValue(model.OID(aggregate.MovingTransitionLayout.Type.ID))),
			fv("aggmtransspace", intValue(int64(aggregate.MovingTransitionSpace))),
		}
		if aggregate.HasInitialValue {
			fields = append(fields, fv("agginitval", stringValue(aggregate.InitialValue)))
		}
		if aggregate.HasMovingInitialValue {
			fields = append(fields, fv("aggminitval", stringValue(aggregate.MovingInitialValue)))
		}
		return emitCatalogRow(relation, emit, fields...)
	})
}

func emitBuiltinAggregates(relation Relation, emit rowEmitter) error {
	for signature := range builtin.Signatures() {
		if signature.Kind != 'a' {
			continue
		}
		if signature.AggregateKind == 0 || signature.AggregateTransition == 0 || signature.AggregateTransitionType == 0 {
			return &model.Error{Code: "55000", Message: "builtin aggregate catalog metadata requires regeneration", Detail: signature.Name}
		}
		fields := []fieldValue{
			fv("aggfnoid", intValue(int64(signature.ID))), fv("aggkind", charValue(signature.AggregateKind)), fv("aggnumdirectargs", intValue(int64(signature.AggregateDirectArguments))),
			fv("aggtransfn", intValue(int64(signature.AggregateTransition))), fv("aggfinalfn", intValue(int64(signature.AggregateFinal))), fv("aggcombinefn", intValue(int64(signature.AggregateCombine))),
			fv("aggserialfn", intValue(int64(signature.AggregateSerial))), fv("aggdeserialfn", intValue(int64(signature.AggregateDeserial))),
			fv("aggmtransfn", intValue(int64(signature.AggregateMovingTransition))), fv("aggminvtransfn", intValue(int64(signature.AggregateMovingInverse))), fv("aggmfinalfn", intValue(int64(signature.AggregateMovingFinal))),
			fv("aggfinalextra", boolValue(signature.AggregateFinalExtra)), fv("aggmfinalextra", boolValue(signature.AggregateMovingFinalExtra)),
			fv("aggfinalmodify", charValue(signature.AggregateFinalModify)), fv("aggmfinalmodify", charValue(signature.AggregateMovingFinalModify)),
			fv("aggsortop", intValue(int64(signature.AggregateSortOperator))), fv("aggtranstype", intValue(int64(signature.AggregateTransitionType))),
			fv("aggtransspace", intValue(int64(signature.AggregateTransitionSpace))), fv("aggmtranstype", intValue(int64(signature.AggregateMovingTransitionType))),
			fv("aggmtransspace", intValue(int64(signature.AggregateMovingTransitionSpace))),
		}
		if signature.AggregateHasInitialValue {
			fields = append(fields, fv("agginitval", stringValue(signature.AggregateInitialValue)))
		}
		if signature.AggregateHasMovingInitialValue {
			fields = append(fields, fv("aggminitval", stringValue(signature.AggregateMovingInitialValue)))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}

func aggregateCatalogKind(kind model.AggregateKind) (byte, bool) {
	switch kind {
	case model.AggregateOrdinary:
		return 'n', true
	case model.AggregateOrderedSet:
		return 'o', true
	case model.AggregateHypotheticalSet:
		return 'h', true
	default:
		return 0, false
	}
}

func aggregateCatalogFinalModify(modify model.AggregateFinalModify) (byte, bool) {
	switch modify {
	case model.AggregateFinalReadOnly:
		return 'r', true
	case model.AggregateFinalShareable:
		return 's', true
	case model.AggregateFinalReadWrite:
		return 'w', true
	default:
		return 0, false
	}
}

func emitLanguage(relation Relation, _ SourceOptions, emit rowEmitter) error {
	for _, language := range []struct {
		oid                 model.OID
		name                string
		procedural, trusted bool
		call, inline        model.OID
		validator           model.OID
	}{
		{12, "internal", false, false, 0, 0, 2246},
		{13, "c", false, false, 0, 0, 2247},
		{14, "sql", false, true, 0, 0, 2248},
		{13563, "plpgsql", true, true, 13560, 13561, 13562},
	} {
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(language.oid)),
			fv("lanname", stringValue(language.name)),
			fv("lanowner", oidValue(10)),
			fv("lanispl", boolValue(language.procedural)),
			fv("lanpltrusted", boolValue(language.trusted)),
			fv("lanplcallfoid", oidValue(language.call)),
			fv("laninline", oidValue(language.inline)),
			fv("lanvalidator", oidValue(language.validator)),
		); err != nil {
			return err
		}
	}
	return nil
}

func emitAccessMethods(relation Relation, emit rowEmitter) error {
	for _, method := range []struct {
		oid     model.OID
		name    string
		handler model.OID
		kind    byte
	}{
		{2, "heap", 3, 't'},
		{403, "btree", 330, 'i'},
		{405, "hash", 331, 'i'},
		{783, "gist", 332, 'i'},
		{2742, "gin", 333, 'i'},
		{4000, "spgist", 334, 'i'},
		{3580, "brin", 335, 'i'},
	} {
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(method.oid)),
			fv("amname", stringValue(method.name)),
			fv("amhandler", oidValue(method.handler)),
			fv("amtype", charValue(method.kind)),
		); err != nil {
			return err
		}
	}
	return nil
}

func emitOperatorClasses(relation Relation, options SourceOptions, emit rowEmitter) error {
	for info := range builtin.OperatorClasses() {
		method := accessMethodOID(info.Method)
		if method == 0 {
			continue
		}
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(info.ID)), fv("opcmethod", oidValue(method)), fv("opcname", stringValue(info.Name)), fv("opcnamespace", oidValue(11)), fv("opcowner", oidValue(10)),
			fv("opcfamily", oidValue(info.Family)), fv("opcintype", intValue(int64(info.InputType))), fv("opcdefault", boolValue(info.Default)), fv("opckeytype", intValue(int64(info.KeyType)))); err != nil {
			return err
		}
	}
	return nil
}

func emitOperatorFamilies(relation Relation, options SourceOptions, emit rowEmitter) error {
	seen := make(map[model.OID]struct{})
	for info := range builtin.OperatorClasses() {
		if info.FamilyName == "" {
			return &model.Error{Code: "55000", Message: "operator family catalog metadata requires regeneration"}
		}
		if info.Family == 0 {
			continue
		}
		if _, ok := seen[info.Family]; ok {
			continue
		}
		seen[info.Family] = struct{}{}
		method := accessMethodOID(info.Method)
		if method == 0 {
			continue
		}
		if err := emitCatalogRow(relation, emit, fv("oid", oidValue(info.Family)), fv("opfmethod", oidValue(method)), fv("opfname", stringValue(info.FamilyName)), fv("opfnamespace", oidValue(11)), fv("opfowner", oidValue(10))); err != nil {
			return err
		}
	}
	return nil
}


func emitAccessMethodOperators(relation Relation, emit rowEmitter) error {
	if !builtin.AccessMethodCatalogReady() {
		return &model.Error{Code: "55000", Message: "access method catalog metadata requires regeneration"}
	}
	for info := range builtin.AccessMethodOperators() {
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(info.OID)), fv("amopfamily", oidValue(info.Family)), fv("amoplefttype", intValue(int64(info.Left))),
			fv("amoprighttype", intValue(int64(info.Right))), fv("amopstrategy", intValue(int64(info.Strategy))), fv("amoppurpose", charValue(info.Purpose)),
			fv("amopopr", intValue(int64(info.Operator))), fv("amopmethod", oidValue(info.Method)), fv("amopsortfamily", oidValue(info.SortFamily))); err != nil {
			return err
		}
	}
	return nil
}


func emitAccessMethodProcedures(relation Relation, emit rowEmitter) error {
	if !builtin.AccessMethodCatalogReady() {
		return &model.Error{Code: "55000", Message: "access method catalog metadata requires regeneration"}
	}
	for info := range builtin.AccessMethodProcedures() {
		if err := emitCatalogRow(relation, emit,
			fv("oid", oidValue(info.OID)), fv("amprocfamily", oidValue(info.Family)), fv("amproclefttype", intValue(int64(info.Left))),
			fv("amprocrighttype", intValue(int64(info.Right))), fv("amprocnum", intValue(int64(info.Number))), fv("amproc", intValue(int64(info.Procedure)))); err != nil {
			return err
		}
	}
	return nil
}

func accessMethodOID(method string) model.OID {
	switch method {
	case "btree": return 403
	case "hash": return 405
	case "gist": return 783
	case "gin": return 2742
	case "spgist": return 4000
	case "brin": return 3580
	default: return 0
	}
}


func emitSettings(ctx context.Context, relation Relation, options SourceOptions, emit rowEmitter) error {
	if options.Settings == nil {
		return &model.Error{Code: "55000", Message: "session settings are not attached to catalog source"}
	}
	settings, err := options.Settings.CatalogSettings(ctx)
	if err != nil {
		return err
	}
	for _, setting := range settings {
		contextName := setting.Context
		if contextName == "" {
			if setting.ReadOnly {
				contextName = "internal"
			} else {
				contextName = "user"
			}
		}
		category := setting.Category
		if category == "" {
			category = settingCategory(setting.Name)
		}
		fields := []fieldValue{
			fv("name", stringValue(strings.ToLower(setting.Name))), fv("setting", stringValue(formatSettingValue(setting.Value))), fv("category", stringValue(category)),
			fv("short_desc", stringValue(setting.Description)), fv("context", stringValue(contextName)), fv("vartype", stringValue(setting.ValueType)), fv("source", stringValue(setting.Source)),
			fv("boot_val", stringValue(setting.BootValue)), fv("reset_val", stringValue(setting.ResetValue)), fv("pending_restart", boolValue(setting.PendingRestart)),
		}
		if setting.Unit != "" { fields = append(fields, fv("unit", stringValue(setting.Unit))) }
		if setting.ExtraDescription != "" { fields = append(fields, fv("extra_desc", stringValue(setting.ExtraDescription))) }
		if setting.MinValue != "" { fields = append(fields, fv("min_val", stringValue(setting.MinValue))) }
		if setting.MaxValue != "" { fields = append(fields, fv("max_val", stringValue(setting.MaxValue))) }
		if setting.EnumValues != nil { fields = append(fields, fv("enumvals", stringArray(setting.EnumValues))) }
		if setting.SourceFile != "" { fields = append(fields, fv("sourcefile", stringValue(setting.SourceFile))) }
		if setting.SourceLine != nil { fields = append(fields, fv("sourceline", intValue(int64(*setting.SourceLine)))) }
		if err := emitCatalogRow(relation, emit, fields...); err != nil { return err }
	}
	return nil
}

func formatSettingValue(value model.Value) string {
	if value.IsNull { return "" }
	switch value.Type {
	case model.TypeString: return value.StrVal
	case model.TypeInt64: return strconv.FormatInt(value.IntVal, 10)
	case model.TypeFloat64: return strconv.FormatFloat(value.FloatVal, 'g', -1, 64)
	case model.TypeBoolean:
		if value.IntVal != 0 { return "on" }
		return "off"
	default: return ""
	}
}

func settingCategory(name string) string {
	switch strings.ToLower(name) {
	case "work_mem", "maintenance_work_mem", "temp_file_limit":
		return "Resource Usage / Memory"
	case "statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout", "transaction_timeout":
		return "Client Connection Defaults / Statement Behavior"
	case "server_encoding", "server_version", "server_version_num", "integer_datetimes", "max_identifier_length":
		return "Preset Options"
	case "datestyle", "intervalstyle", "timezone", "extra_float_digits":
		return "Client Connection Defaults / Locale and Formatting"
	default:
		return "Client Connection Defaults / Statement Behavior"
	}
}

func emitSQLFeatures(ctx context.Context, relation Relation, options SourceOptions, emit rowEmitter) error {
	if options.Features == nil {
		return &model.Error{Code: "55000", Message: "fixed feature catalog is not configured"}
	}
	features, err := options.Features.CatalogFeatures(ctx)
	if err != nil { return err }
	for _, feature := range features {
		fields := []fieldValue{fv("feature_id", stringValue(feature.ID)), fv("feature_name", stringValue(feature.Name)), fv("is_supported", stringValue(yesNo(feature.Supported)))}
		if feature.SubfeatureID != "" { fields = append(fields, fv("sub_feature_id", stringValue(feature.SubfeatureID))) }
		if feature.SubfeatureName != "" { fields = append(fields, fv("sub_feature_name", stringValue(feature.SubfeatureName))) }
		if feature.VerifiedBy != "" { fields = append(fields, fv("is_verified_by", stringValue(feature.VerifiedBy))) }
		if feature.Comment != "" { fields = append(fields, fv("comments", stringValue(feature.Comment))) }
		if err := emitCatalogRow(relation, emit, fields...); err != nil { return err }
	}
	return nil
}

func emitSQLStandards(ctx context.Context, relation Relation, options SourceOptions, emit rowEmitter) error {
	if options.Standards == nil {
		return &model.Error{Code: "55000", Message: "SQL standards catalog is not configured"}
	}
	if relation.ID == IDSqlParts {
		parts, err := options.Standards.CatalogSQLParts(ctx)
		if err != nil {
			return err
		}
		for _, part := range parts {
			fields := []fieldValue{
				fv("feature_id", stringValue(part.ID)), fv("feature_name", stringValue(part.Name)), fv("is_supported", stringValue(yesNo(part.Supported))),
			}
			if part.VerifiedBy != "" {
				fields = append(fields, fv("is_verified_by", stringValue(part.VerifiedBy)))
			}
			if part.Comments != "" {
				fields = append(fields, fv("comments", stringValue(part.Comments)))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	}
	sizing, err := options.Standards.CatalogSQLSizing(ctx)
	if err != nil {
		return err
	}
	for _, value := range sizing {
		fields := []fieldValue{fv("sizing_id", intValue(int64(value.ID))), fv("sizing_name", stringValue(value.Name))}
		if value.SupportedValue != nil {
			fields = append(fields, fv("supported_value", intValue(int64(*value.SupportedValue))))
		}
		if value.Comments != "" {
			fields = append(fields, fv("comments", stringValue(value.Comments)))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}

func yesNo(value bool) string { if value { return "YES" }; return "NO" }

func emitImplementationInfo(relation Relation, emit rowEmitter) error {
	rows := [][]fieldValue{
		{fv("implementation_info_id", stringValue("17")), fv("implementation_info_name", stringValue("DBMS NAME")), fv("character_value", stringValue("SuperSQL"))},
		{fv("implementation_info_id", stringValue("18")), fv("implementation_info_name", stringValue("DBMS VERSION")), fv("character_value", stringValue("18.4"))},
	}
	for _, row := range rows { if err := emitCatalogRow(relation, emit, row...); err != nil { return err } }
	return nil
}
