package catalogrel

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type rowEmitter func([]model.Value) error

type schemaDescriptor struct {
	id          model.ObjectID
	oid         model.OID
	name        string
	owner       model.OID
	description string
}

type objectDescriptor struct {
	published catalog.PublishedDescriptor
	spec model.DescriptorSpec
	schema schemaDescriptor
}
func principalName(owner model.OID) (string, error) {
	principal := model.Principal{}
	if owner != principal.OID() {
		return "", &model.Error{Code: "55000", Message: "owner principal metadata is unavailable"}
	}
	return principal.Name(), nil
}

func generateRows(ctx context.Context, snapshot Snapshot, view ReadView, relation Relation, options SourceOptions, emit rowEmitter) error {
	switch relation.ID {
	case IDPgDatabase:
		fields := []fieldValue{
			fv("oid", oidValue(options.Database.OID)), fv("datname", stringValue(options.Database.Name)), fv("datdba", oidValue(options.Database.Owner)),
			fv("encoding", intValue(int64(options.Database.Encoding))), fv("datlocprovider", charValue(options.Database.LocaleProvider)),
			fv("datistemplate", boolValue(options.Database.IsTemplate)), fv("datallowconn", boolValue(options.Database.AllowConnections)),
			fv("dathasloginevt", boolValue(options.Database.HasLoginEventTriggers)), fv("datconnlimit", intValue(int64(options.Database.ConnectionLimit))),
			fv("datcollate", stringValue(options.Database.Collation)), fv("datctype", stringValue(options.Database.CharacterType)),
		}
		if options.Database.Locale != "" {
			fields = append(fields, fv("datlocale", stringValue(options.Database.Locale)))
		}
		if options.Database.CollationVersion != "" {
			fields = append(fields, fv("datcollversion", stringValue(options.Database.CollationVersion)))
		}
		return emitCatalogRow(relation, emit, fields...)
	case IDPgRoles:
		return emitCatalogRow(relation, emit,
			fv("rolname", stringValue(options.Database.RoleName)), fv("rolsuper", boolValue(options.Database.RoleSuperuser)), fv("rolinherit", boolValue(options.Database.RoleInherit)),
			fv("rolcreaterole", boolValue(options.Database.RoleCreateRole)), fv("rolcreatedb", boolValue(options.Database.RoleCreateDB)), fv("rolcanlogin", boolValue(options.Database.RoleCanLogin)),
			fv("rolreplication", boolValue(options.Database.RoleReplication)), fv("rolconnlimit", intValue(int64(options.Database.RoleConnectionLimit))),
			fv("rolbypassrls", boolValue(options.Database.RoleBypassRLS)), fv("oid", oidValue(options.Database.RoleOID)))
	case IDPgUser:
		return emitCatalogRow(relation, emit,
			fv("usename", stringValue(options.Database.RoleName)), fv("usesysid", oidValue(options.Database.RoleOID)), fv("usecreatedb", boolValue(options.Database.RoleCreateDB)),
			fv("usesuper", boolValue(options.Database.RoleSuperuser)), fv("userepl", boolValue(options.Database.RoleReplication)), fv("usebypassrls", boolValue(options.Database.RoleBypassRLS)))
	case IDPgNamespace:
		return emitNamespaces(ctx, snapshot, relation, options, emit)
	case IDPgClass:
		return emitClasses(ctx, snapshot, view, relation, options, emit)
	case IDPgAttribute:
		return emitAttributes(ctx, snapshot, relation, options, emit)
	case IDPgAttrdef:
		return emitDefaults(ctx, snapshot, relation, options, emit)
	case IDPgIndex:
		return emitIndexes(ctx, snapshot, relation, options, emit, false)
	case IDPgIndexes:
		return emitIndexes(ctx, snapshot, relation, options, emit, true)
	case IDPgConstraint:
		return emitConstraints(ctx, snapshot, relation, options, emit, false)
	case IDPgTrigger:
		return emitTriggers(ctx, snapshot, relation, options, emit, false)
	case IDPgInherits:
		return emitInherits(ctx, snapshot, relation, options, emit)
	case IDPgPartitionedTable:
		return emitPartitions(ctx, snapshot, relation, options, emit)
	case IDPgSequence, IDPgSequences:
		return emitSequences(ctx, snapshot, view, relation, options, emit)
	case IDPgTables, IDPgViews, IDPgMatviews:
		return emitPGViews(ctx, snapshot, relation, options, emit)
	case IDPgType:
		return emitTypes(ctx, snapshot, relation, options, emit)
	case IDPgEnum:
		return emitEnums(ctx, snapshot, relation, options, emit)
	case IDPgRange:
		return emitRanges(ctx, snapshot, relation, options, emit)
	case IDPgCollation:
		return emitBuiltinCollations(relation, options, emit)
	case IDPgOperator:
		return emitOperators(ctx, snapshot, relation, options, emit)
	case IDPgCast:
		return emitCasts(ctx, snapshot, relation, options, emit)
	case IDPgProc:
		return emitProcedures(ctx, snapshot, relation, options, emit)
	case IDPgLanguage:
		return emitLanguage(relation, options, emit)
	case IDPgAm:
		return emitAccessMethods(relation, emit)
	case IDPgOpclass:
		return emitOperatorClasses(relation, options, emit)
	case IDPgOpfamily:
		return emitOperatorFamilies(relation, options, emit)
	case IDPgAmop:
		return emitAccessMethodOperators(relation, emit)
	case IDPgAmproc:
		return emitAccessMethodProcedures(relation, emit)
	case IDPgAggregate:
		return emitAggregates(ctx, snapshot, relation, options, emit)
	case IDPgStatistic, IDPgStatisticExt, IDPgStatisticExtData, IDPgStats:
		return emitStatistics(ctx, snapshot, view, relation, options, emit)
	case IDPgTSConfig, IDPgTSConfigMap, IDPgTSDict, IDPgTSParser, IDPgTSTemplate:
		return emitTextSearchCatalog(ctx, snapshot, relation, options, emit)
	case IDPgDepend:
		return emitDependencies(ctx, snapshot, relation, options, emit)
	case IDPgDescription:
		return emitDescriptions(ctx, snapshot, relation, options, emit)
	case IDPgSettings:
		return emitSettings(ctx, relation, options, emit)
	case IDInformationSchemaCatalogName:
		return emitCatalogRow(relation, emit, fv("catalog_name", stringValue(options.Database.Name)))
	case IDSchemata:
		return emitInformationSchemata(ctx, snapshot, relation, options, emit)
	case IDCharacterSets, IDCollations, IDCollationCharacterSetApplicability:
		return emitInformationCatalogConstants(relation, options, emit)
	case IDTables, IDColumns, IDViews, IDSequences, IDTableConstraints, IDCheckConstraints, IDKeyColumnUsage, IDTriggers, IDRoutines, IDParameters, IDDomains, IDUserDefinedTypes, IDAttributes:
		return emitInformationSchema(ctx, snapshot, relation, options, emit)
	case IDColumnDomainUsage, IDColumnUdtUsage:
		return emitColumnTypeUsage(ctx, snapshot, relation, options, emit)
	case IDDomainConstraints, IDDomainUdtUsage:
		return emitDomainUsage(ctx, snapshot, relation, options, emit)
	case IDElementTypes:
		return emitElementTypes(ctx, snapshot, relation, options, emit)
	case IDCheckConstraintRoutineUsage, IDColumnColumnUsage, IDConstraintColumnUsage, IDConstraintTableUsage, IDReferentialConstraints,
		IDRoutineColumnUsage, IDRoutineRoutineUsage, IDRoutineSequenceUsage, IDRoutineTableUsage, IDTriggeredUpdateColumns,
		IDViewColumnUsage, IDViewRoutineUsage, IDViewTableUsage:
		return emitInformationUsage(ctx, snapshot, relation, options, emit)
	case IDSqlFeatures:
		return emitSQLFeatures(ctx, relation, options, emit)
	case IDSqlImplementationInfo:
		return emitImplementationInfo(relation, emit)
	case IDSqlParts, IDSqlSizing:
		return emitSQLStandards(ctx, relation, options, emit)
	default:
		return &model.Error{Code: "XX000", Message: "logical catalog relation has no row projector", Detail: relation.Namespace + "." + relation.Name}
	}
}

type fieldValue struct {
	name string
	value model.Value
}

func fv(name string, value model.Value) fieldValue { return fieldValue{name: name, value: value} }

func emitCatalogRow(relation Relation, emit rowEmitter, fields ...fieldValue) error {
	row := make([]model.Value, len(relation.Columns))
	for i, column := range relation.Columns {
		var value model.Value
		found := false
		for _, field := range fields {
			if field.name == column.Name {
				value, found = field.value, true
				break
			}
		}
		if !found {
			if column.Nullable || column.UnsupportedFeature != "" {
				row[i] = model.NullVal(column.Layout.Type.Storage)
				continue
			}
			return &model.Error{Code: "XX000", Message: "incomplete logical catalog row", Detail: relation.Namespace + "." + relation.Name + "." + column.Name}
		}
		if value.IsNull && !column.Nullable {
			return &model.Error{Code: "XX000", Message: "null in non-null logical catalog field", Detail: relation.Namespace + "." + relation.Name + "." + column.Name}
		}
		row[i] = value
	}
	return emit(row)
}

func oidValue(value model.OID) model.Value { return model.Int64Val(int64(value)) }
func idValue(value model.ObjectID) model.Value { return model.Int64Val(int64(value)) }
func intValue(value int64) model.Value { return model.Int64Val(value) }
func floatValue(value float64) model.Value { return model.Float64Val(float64(float32(value))) }
func stringValue(value string) model.Value { return model.StringVal(value) }
func charValue(value byte) model.Value {
	if value == 0 {
		return model.StringVal("")
	}
	return model.StringVal(string([]byte{value}))
}
func boolValue(value bool) model.Value { return model.BoolVal(value) }

func intArray(values []int64) model.Value {
	items := make([]model.Value, len(values))
	for i, value := range values {
		items[i] = intValue(value)
	}
	return model.Value{Type: model.TypeArray, ArrayVal: items, Dimensions: []model.ArrayDimension{{Length: int32(len(items)), LowerBound: 0}}}
}

func oidArray(values []int64) model.Value {
	value := intArray(values)
	value.Dimensions[0].LowerBound = 1
	return value
}

func stringArray(values []string) model.Value {
	items := make([]model.Value, len(values))
	for i, value := range values {
		items[i] = stringValue(value)
	}
	return model.Value{Type: model.TypeArray, ArrayVal: items, Dimensions: []model.ArrayDimension{{Length: int32(len(items)), LowerBound: 1}}}
}

func informationSchemaIdentity(options SourceOptions) (NamespaceIdentity, error) {
	identity, ok := options.Registry.NamespaceIdentity("information_schema")
	if !ok || identity.ObjectID == 0 || identity.OID == 0 || identity.OID == 11 {
		return NamespaceIdentity{}, &model.Error{Code: "55000", Message: "information_schema namespace identity is unavailable"}
	}
	return identity, nil
}

func scanSchemas(ctx context.Context, snapshot Snapshot, options SourceOptions, callback func(schemaDescriptor) error) error {
	informationSchema, err := informationSchemaIdentity(options)
	if err != nil {
		return err
	}
	published, err := snapshot.Descriptor(ctx, informationSchema.ObjectID)
	if err != nil {
		return err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectSchema ||
		published.Descriptor.OID() != informationSchema.OID || published.Descriptor.Name().Name != "information_schema" {
		return &model.Error{Code: "55000", Message: "information_schema namespace descriptor is unavailable"}
	}
	count := uint64(0)
	return snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		count++
		if count > options.MaxDescriptors {
			return &model.Error{Code: "54000", Message: "catalog descriptor budget exceeded"}
		}
		descriptor := published.Descriptor
		if descriptor == nil || descriptor.State() != model.StatePublic {
			return nil
		}
		name, oid := descriptor.Name().Name, descriptor.OID()
		if name == "pg_catalog" || oid == 11 {
			if name != "pg_catalog" || oid != 11 {
				return &model.Error{Code: "XX000", Message: "persisted schema collides with pg_catalog"}
			}
		} else if (name == "information_schema") != (descriptor.ID() == informationSchema.ObjectID) ||
			(descriptor.ID() == informationSchema.ObjectID && oid != informationSchema.OID) ||
			(oid == informationSchema.OID && descriptor.ID() != informationSchema.ObjectID) {
			return &model.Error{Code: "XX000", Message: "persisted schema collides with information_schema"}
		}
		spec, err := descriptor.SpecWithin(ctx, descriptorLimits(options))
		if err != nil {
			return err
		}
		if spec.Schema == nil || spec.Schema.Owner == 0 {
			return &model.Error{Code: "XX000", Message: "schema owner metadata is unavailable", Detail: name}
		}
		if name == "pg_catalog" && spec.Schema.Owner != 10 {
			return &model.Error{Code: "XX000", Message: "pg_catalog owner metadata is inconsistent"}
		}
		return callback(schemaDescriptor{id: descriptor.ID(), oid: oid, name: name, owner: spec.Schema.Owner, description: spec.Description})
	})
}

func scanObjects(ctx context.Context, snapshot Snapshot, options SourceOptions, kinds []model.ObjectKind, callback func(objectDescriptor) error) error {
	var schemas []schemaDescriptor
	if err := scanSchemas(ctx, snapshot, options, func(schema schemaDescriptor) error {
		schemas = append(schemas, schema)
		return nil
	}); err != nil {
		return err
	}
	count := uint64(len(schemas))
	for _, schema := range schemas {
		for _, kind := range kinds {
			err := snapshot.ScanNamespace(ctx, schema.id, kind, func(published catalog.PublishedDescriptor) error {
				count++
				if count > options.MaxDescriptors {
					return &model.Error{Code: "54000", Message: "catalog descriptor budget exceeded"}
				}
				descriptor := published.Descriptor
				if descriptor == nil || descriptor.State() != model.StatePublic {
					return nil
				}
				spec, err := descriptor.SpecWithin(ctx, descriptorLimits(options))
				if err != nil {
					return err
				}
				return callback(objectDescriptor{published: published, spec: spec, schema: schema})
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func descriptorLimits(options SourceOptions) model.DescriptorLimits {
	return model.DescriptorLimits{
		MaxBytes: options.MaxDescriptorBytes,
		MaxItems: options.MaxDescriptorItems,
		MaxDepth: options.MaxDescriptorDepth,
	}
}

func emitNamespaces(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitCatalogRow(relation, emit, fv("oid", oidValue(11)), fv("nspname", stringValue("pg_catalog")), fv("nspowner", oidValue(10))); err != nil {
		return err
	}
	return scanSchemas(ctx, snapshot, options, func(schema schemaDescriptor) error {
		if schema.oid == 11 {
			return nil
		}
		return emitCatalogRow(relation, emit, fv("oid", oidValue(schema.oid)), fv("nspname", stringValue(schema.name)), fv("nspowner", oidValue(schema.owner)))
	})
}

func relationOID(relation Relation, options SourceOptions) model.OID {
	identity, _ := options.Registry.Identity(relation.ID)
	return identity.RelationOID
}

func relationTypeOID(relation Relation, options SourceOptions) model.OID {
	identity, _ := options.Registry.Identity(relation.ID)
	return identity.RowTypeOID
}

func emitClasses(ctx context.Context, snapshot Snapshot, view ReadView, relation Relation, options SourceOptions, emit rowEmitter) error {
	if options.Statistics == nil {
		return &model.Error{Code: "55000", Message: "statistics catalog is not configured"}
	}
	statisticsSnapshot, err := options.Statistics.OpenCatalogStatistics(ctx, view)
	if err != nil {
		return err
	}
	if statisticsSnapshot == nil || statisticsSnapshot.Timestamp() != view.Timestamp() {
		return &model.Error{Code: "55000", Message: "statistics snapshot timestamp mismatch"}
	}
	informationSchema, err := informationSchemaIdentity(options)
	if err != nil {
		return err
	}
	for _, system := range registry {
		oid := relationOID(system, options)
		namespace := model.OID(11)
		kind := byte('r')
		if logicalRelationDeclaration(system) == DeclarationView {
			kind = 'v'
		}
		if system.Namespace == "information_schema" {
			namespace = informationSchema.OID
		}
		if err := emitClassRow(relation, emit, oid, system.Name, namespace, 10, relationTypeOID(system, options), 0, kind, 'p', -1, len(system.Columns), 0, false, false, false, false, true, "", nil); err != nil {
			return err
		}
	}
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		columns := descriptorColumns(object.spec)
		columnCount := len(columns)
		estimatedRows, err := catalogRelationEstimate(ctx, statisticsSnapshot, object.spec)
		if err != nil {
			return err
		}
		hasIndexes, hasTriggers, hasSubclass, isPartition := false, false, false, false
		populated := true
		persistence := byte('p')
		partitionBound := ""
		ofType := model.OID(0)
		checks := 0
		var relationOptions []string
		if object.spec.Table != nil {
			hasIndexes = len(object.spec.Table.Indexes) != 0
			hasTriggers = len(object.spec.Table.Triggers) != 0
			hasSubclass = object.spec.Table.InheritanceChildren != 0
			isPartition = object.spec.Table.PartitionBound != nil
			if object.spec.Table.PartitionBound != nil {
				partitionBound = object.spec.Table.PartitionBound.Definition
				if partitionBound == "" {
					return &model.Error{Code: "XX000", Message: "partition bound definition is unavailable"}
				}
			}
			if object.spec.Table.TypedOf != 0 {
				ofType, err = resolvedCompositeTypeOID(ctx, snapshot, options, object.spec.Table.TypedOf)
				if err != nil {
					return err
				}
			}
			if object.spec.Table.Persistence == "t" {
				persistence = 't'
			}
			for _, constraint := range object.spec.Table.Constraints {
				if constraint.Kind == model.ConstraintCheck && !constraint.Mutation {
					checks++
				}
			}
		}
		if object.spec.View != nil {
			hasTriggers = len(object.spec.View.Triggers) != 0
			if object.spec.View.Persistence == "t" {
				persistence = 't'
			}
			if object.spec.View.CheckOption != "" {
				relationOptions = []string{"check_option=" + object.spec.View.CheckOption}
			}
			if object.spec.Table != nil {
				if object.spec.Table.Owner != object.spec.View.Owner {
					return &model.Error{Code: "XX000", Message: "materialized view owners do not match"}
				}
				if object.spec.Table.Persistence != object.spec.View.Persistence {
					return &model.Error{Code: "XX000", Message: "materialized view persistence metadata does not match"}
				}
				if object.spec.Table.RowType != object.spec.View.RowType {
					return &model.Error{Code: "XX000", Message: "materialized view row type metadata does not match"}
				}
			}
		}
		if object.spec.Name.Kind == model.ObjectMaterializedView && object.spec.View != nil {
			populated = object.spec.View.Populated
		}
		owner, relationType, kind := model.OID(0), model.OID(0), relationKind(object.spec.Name.Kind)
		switch {
		case object.spec.Table != nil:
			owner = object.spec.Table.Owner
			relationType, err = resolvedRelationRowTypeOID(ctx, snapshot, options, object.spec.ID, object.spec.Table.RowType)
			if err != nil {
				return err
			}
		case object.spec.Index != nil:
			owner = object.spec.Index.Owner
			table, err := snapshot.Descriptor(ctx, object.spec.Index.Table)
			if err != nil {
				return err
			}
			if table.Descriptor == nil || table.Descriptor.State() != model.StatePublic {
				return &model.Error{Code: "55000", Message: "index table descriptor is not public"}
			}
			tableSpec, err := table.Descriptor.SpecWithin(ctx, descriptorLimits(options))
			if err != nil {
				return err
			}
			if tableSpec.Table == nil {
				return &model.Error{Code: "XX000", Message: "index table metadata is unavailable"}
			}
			estimatedRows, err = catalogRelationEstimate(ctx, statisticsSnapshot, tableSpec)
			if err != nil {
				return err
			}
			if object.spec.Index.Predicate != nil {
				estimatedRows = -1
			}
			if tableSpec.Table.Persistence == "t" {
				persistence = 't'
			}
			columnCount = len(object.spec.Index.Keys) + len(object.spec.Index.Include)
		case object.spec.Sequence != nil:
			owner = object.spec.Sequence.Owner
			if object.spec.Sequence.Persistence == "t" {
				persistence = 't'
			}
		case object.spec.View != nil:
			owner = object.spec.View.Owner
			relationType, err = resolvedRelationRowTypeOID(ctx, snapshot, options, object.spec.ID, object.spec.View.RowType)
			if err != nil {
				return err
			}
		case object.spec.CompositeRelation != nil:
			owner = object.spec.CompositeRelation.Owner
			relationType, err = resolvedRelationRowTypeOID(ctx, snapshot, options, object.spec.ID, object.spec.CompositeRelation.RowType)
			if err != nil {
				return err
			}
			kind = 'c'
		}
		if owner == 0 {
			return &model.Error{Code: "55000", Message: "relation owner metadata is unavailable"}
		}
		if object.spec.Table != nil && object.spec.Table.PartitionStrategy != 0 {
			kind = 'p'
		}
		return emitClassRow(relation, emit, object.spec.OID, object.spec.Name.Name, object.schema.oid, owner, relationType, ofType, kind, persistence, estimatedRows, columnCount, checks, hasIndexes, hasTriggers, hasSubclass, isPartition, populated, partitionBound, relationOptions)
	})
}

func resolvedRelationRowTypeOID(ctx context.Context, snapshot Snapshot, options SourceOptions, relationID, rowTypeID model.ObjectID) (model.OID, error) {
	if rowTypeID == 0 {
		return 0, &model.Error{Code: "55000", Message: "relation row type metadata is unavailable"}
	}
	published, err := snapshot.Descriptor(ctx, rowTypeID)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectType {
		return 0, &model.Error{Code: "XX000", Message: "relation row type descriptor is unavailable"}
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return 0, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindComposite || spec.Type.Relation != relationID || spec.OID == 0 {
		return 0, &model.Error{Code: "XX000", Message: "relation row type descriptor is inconsistent"}
	}
	return spec.OID, nil
}

func resolvedCompositeTypeOID(ctx context.Context, snapshot Snapshot, options SourceOptions, id model.ObjectID) (model.OID, error) {
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectType || published.Descriptor.OID() == 0 {
		return 0, &model.Error{Code: "XX000", Message: "typed table type descriptor is unavailable"}
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return 0, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindComposite {
		return 0, &model.Error{Code: "XX000", Message: "typed table type descriptor is inconsistent"}
	}
	return spec.OID, nil
}

func emitClassRow(relation Relation, emit rowEmitter, oid model.OID, name string, namespace, owner, relationType, ofType model.OID, kind, persistence byte, estimatedRows float64, columns, checks int, hasIndexes, hasTriggers, hasSubclass, isPartition, populated bool, partitionBound string, relationOptions []string) error {
	accessMethod := model.OID(0)
	if kind == 'i' {
		accessMethod = 403
	}
	fields := []fieldValue{
		fv("oid", oidValue(oid)), fv("relname", stringValue(name)), fv("relnamespace", oidValue(namespace)), fv("reltype", oidValue(relationType)),
		fv("reloftype", oidValue(ofType)), fv("relowner", oidValue(owner)), fv("relam", oidValue(accessMethod)), fv("reltuples", floatValue(estimatedRows)),
		fv("relhasindex", boolValue(hasIndexes)), fv("relisshared", boolValue(false)), fv("relpersistence", charValue(persistence)), fv("relkind", charValue(kind)),
		fv("relnatts", intValue(int64(columns))), fv("relchecks", intValue(int64(checks))), fv("relhasrules", boolValue(false)), fv("relhastriggers", boolValue(hasTriggers)),
		fv("relhassubclass", boolValue(hasSubclass)), fv("relrowsecurity", boolValue(false)), fv("relforcerowsecurity", boolValue(false)), fv("relispopulated", boolValue(populated)),
		fv("relispartition", boolValue(isPartition)),
	}
	if len(relationOptions) != 0 {
		fields = append(fields, fv("reloptions", stringArray(relationOptions)))
	}
	if partitionBound != "" {
		fields = append(fields, fv("relpartbound", stringValue(partitionBound)))
	}
	return emitCatalogRow(relation, emit, fields...)
}

func catalogRelationEstimate(ctx context.Context, reader Statistics, spec model.DescriptorSpec) (float64, error) {
	if spec.Table == nil {
		return -1, nil
	}
	reference := storage.ObjectRef{ObjectID: spec.ID, Generation: spec.Physical.Generation, DescriptorVersion: spec.Version, EncodingVersion: spec.Physical.EncodingVersion}
	table, found, err := reader.Lookup(ctx, reference)
	if err != nil || !found || !table.LiveRows.Known {
		return -1, err
	}
	return float64(table.LiveRows.Value), nil
}

func relationKind(kind model.ObjectKind) byte {
	switch kind {
	case model.ObjectIndex:
		return 'i'
	case model.ObjectSequence:
		return 'S'
	case model.ObjectView:
		return 'v'
	case model.ObjectMaterializedView:
		return 'm'
	case model.ObjectCompositeRelation:
		return 'c'
	default:
		return 'r'
	}
}

func descriptorColumns(spec model.DescriptorSpec) []model.ColumnDescriptor {
	if spec.Table != nil {
		return spec.Table.Columns
	}
	if spec.View != nil {
		return spec.View.Columns
	}
	if spec.CompositeRelation != nil {
		return spec.CompositeRelation.Columns
	}
	return nil
}

func emitAttributes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, system := range options.Registry.All() {
		oid := relationOID(system, options)
		for i, column := range system.Columns {
			if err := emitAttributeRow(relation, emit, oid, column.Name, column.Layout, column.Nullable, model.ColumnID(i+1), nil, -1); err != nil {
				return err
			}
		}
	}
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		if object.spec.Index != nil {
			return emitIndexAttributes(ctx, snapshot, relation, options, emit, object)
		}
		for _, column := range descriptorColumns(object.spec) {
			if column.SystemIdentity || column.State != model.StatePublic {
				continue
			}
			if err := emitAttributeRow(relation, emit, object.spec.OID, column.Name, column.Layout, column.Nullable, column.ID, &column, catalogStatisticsTarget(column.StatisticsTarget, column.StatisticsTargetSet)); err != nil {
				return err
			}
		}
		return nil
	})
}
func emitIndexAttributes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter, object objectDescriptor) error {
	metadata := object.spec.Index
	if metadata == nil {
		return &model.Error{Code: "XX000", Message: "index metadata is unavailable"}
	}
	published, err := snapshot.Descriptor(ctx, metadata.Table)
	if err != nil {
		return err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return &model.Error{Code: "55000", Message: "index table descriptor is not public"}
	}
	table, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return err
	}
	if table.Table == nil {
		return &model.Error{Code: "XX000", Message: "index table metadata is unavailable"}
	}
	for i, key := range metadata.Keys {
		layout := model.TypeLayout{Type: key.Type, Children: key.Children}
		name := key.ExpressionName
		target := catalogStatisticsTarget(key.StatisticsTarget, key.StatisticsTargetSet)
		if key.Expression == nil {
			column, ok := indexAttributeColumn(table.Table.Columns, key.Column)
			if !ok {
				return &model.Error{Code: "XX000", Message: "index column metadata is unavailable"}
			}
			name, target = column.Name, -1
		}
		if err := emitAttributeRow(relation, emit, object.spec.OID, name, layout, true, model.ColumnID(i+1), nil, target); err != nil {
			return err
		}
	}
	for i, id := range metadata.Include {
		column, ok := indexAttributeColumn(table.Table.Columns, id)
		if !ok {
			return &model.Error{Code: "XX000", Message: "included index column metadata is unavailable"}
		}
		number := model.ColumnID(len(metadata.Keys) + i + 1)
		if err := emitAttributeRow(relation, emit, object.spec.OID, column.Name, column.Layout, true, number, nil, -1); err != nil {
			return err
		}
	}
	return nil
}

func indexAttributeColumn(columns []model.ColumnDescriptor, id model.ColumnID) (model.ColumnDescriptor, bool) {
	for _, column := range columns {
		if column.ID == id && column.State == model.StatePublic && !column.SystemIdentity {
			return column, true
		}
	}
	return model.ColumnDescriptor{}, false
}

func catalogStatisticsTarget(target int32, set bool) int32 {
	if set {
		return target
	}
	return -1
}


func emitAttributeRow(relation Relation, emit rowEmitter, relationOID model.OID, name string, layout model.TypeLayout, nullable bool, number model.ColumnID, descriptor *model.ColumnDescriptor, statisticsTarget int32) error {
	hasDefault, hasMissing := false, false
	identity, generated := byte(0), byte(0)
	if descriptor != nil {
		hasDefault = descriptor.Default != nil || descriptor.ConstantDefault != nil || descriptor.Generated != nil
		hasMissing = descriptor.ConstantDefault != nil
		if descriptor.IdentitySequence != 0 {
			identity = 'd'
			if descriptor.IdentityAlways {
				identity = 'a'
			}
		}
		if descriptor.Generated != nil {
			generated = 'v'
			if descriptor.GeneratedStored {
				generated = 's'
			}
		}
	}
	return emitCatalogRow(relation, emit,
		fv("attrelid", oidValue(relationOID)), fv("attname", stringValue(name)), fv("atttypid", intValue(int64(layout.Type.ID))), fv("attnum", intValue(int64(number))),
		fv("atttypmod", intValue(int64(layout.Type.Modifier))), fv("attndims", intValue(int64(arrayDimensions(layout)))), fv("attnotnull", boolValue(!nullable)),
		fv("atthasdef", boolValue(hasDefault)), fv("atthasmissing", boolValue(hasMissing)), fv("attidentity", charValue(identity)), fv("attgenerated", charValue(generated)),
		fv("attisdropped", boolValue(false)), fv("attislocal", boolValue(descriptor == nil || descriptor.Local || len(descriptor.InheritedFrom) == 0)), fv("attinhcount", intValue(int64(columnInheritanceCount(descriptor)))), fv("attcollation", intValue(int64(layout.Type.Collation))), fv("attstattarget", intValue(int64(statisticsTarget))))
}

func columnInheritanceCount(descriptor *model.ColumnDescriptor) int {
	if descriptor == nil {
		return 0
	}
	return len(descriptor.InheritedFrom)
}

func arrayDimensions(layout model.TypeLayout) int16 {
	if layout.Type.Storage == model.TypeArray {
		return 1
	}
	return 0
}

func emitDefaults(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		for _, column := range descriptorColumns(object.spec) {
			expression := column.Default
			if expression == nil {
				expression = column.Generated
			}
			if expression == nil {
				if column.DefaultOID != 0 {
					return &model.Error{Code: "XX000", Message: "column without a default carries an attribute-default identity", Detail: object.spec.Name.Name + "." + column.Name}
				}
				continue
			}
			if column.DefaultOID == 0 {
				return &model.Error{Code: "55000", Message: "column default has no SQL catalog identity", Detail: object.spec.Name.Name + "." + column.Name}
			}
			if err := emitCatalogRow(relation, emit, fv("oid", oidValue(column.DefaultOID)), fv("adrelid", oidValue(object.spec.OID)), fv("adnum", intValue(int64(column.ID))), fv("adbin", stringValue(expression.SQL))); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitIndexes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter, view bool) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectIndex}, func(object objectDescriptor) error {
		metadata := object.spec.Index
		if metadata == nil {
			return nil
		}
		table, err := snapshot.Descriptor(ctx, metadata.Table)
		if err != nil {
			return err
		}
		if table.Descriptor == nil || table.Descriptor.State() != model.StatePublic || table.Descriptor.Kind() != model.ObjectTable {
			return &model.Error{Code: "55000", Message: "index table descriptor is not public"}
		}
		tableSpec, err := table.Descriptor.SpecWithin(ctx, descriptorLimits(options))
		if err != nil {
			return err
		}
		if tableSpec.Table == nil {
			return &model.Error{Code: "XX000", Message: "index table metadata is unavailable"}
		}
		if view {
			definition, err := catalogIndexDefinition(object, tableSpec, metadata)
			if err != nil {
				return err
			}
			return emitCatalogRow(relation, emit, fv("schemaname", stringValue(object.schema.name)), fv("tablename", stringValue(tableSpec.Name.Name)), fv("indexname", stringValue(object.spec.Name.Name)), fv("indexdef", stringValue(definition)))
		}
		keys := make([]int64, len(metadata.Keys)+len(metadata.Include))
		collations := make([]int64, len(metadata.Keys))
		classes := make([]int64, len(metadata.Keys))
		optionsVector := make([]int64, len(metadata.Keys))
		for i, key := range metadata.Keys {
			keys[i] = int64(key.Column)
			collations[i] = int64(key.Collation)
			classes[i] = int64(key.OperatorClass)
			if key.Descending {
				optionsVector[i] |= 1
			}
			if key.NullsFirst {
				optionsVector[i] |= 2
			}
		}
		for i, id := range metadata.Include {
			keys[len(metadata.Keys)+i] = int64(id)
		}
		primary, exclusion, immediate, err := catalogIndexConstraintFlags(tableSpec.Table, object.spec.ID)
		if err != nil {
			return err
		}
		fields := []fieldValue{
			fv("indexrelid", oidValue(object.spec.OID)), fv("indrelid", oidValue(tableSpec.OID)), fv("indnatts", intValue(int64(len(keys)))), fv("indnkeyatts", intValue(int64(len(metadata.Keys)))),
			fv("indisunique", boolValue(metadata.Unique)), fv("indnullsnotdistinct", boolValue(metadata.NullsNotDistinct)), fv("indisprimary", boolValue(primary)), fv("indisexclusion", boolValue(exclusion)),
			fv("indimmediate", boolValue(immediate)), fv("indisclustered", boolValue(false)), fv("indisvalid", boolValue(metadata.Valid())), fv("indisready", boolValue(metadata.Ready())), fv("indislive", boolValue(metadata.Live())),
			fv("indkey", intArray(keys)), fv("indcollation", intArray(collations)), fv("indclass", intArray(classes)), fv("indoption", intArray(optionsVector)),
		}
		if metadata.Predicate != nil {
			fields = append(fields, fv("indpred", stringValue(metadata.Predicate.SQL)))
		}
		var expressions []string
		for _, key := range metadata.Keys {
			if key.Expression != nil {
				expressions = append(expressions, key.Expression.SQL)
			}
		}
		if len(expressions) != 0 {
			fields = append(fields, fv("indexprs", stringValue(strings.Join(expressions, ", "))))
		}
		return emitCatalogRow(relation, emit, fields...)
	})
}

func catalogIndexConstraintFlags(table *model.TableMetadata, index model.ObjectID) (primary, exclusion, immediate bool, err error) {
	immediate = true
	found := false
	for _, constraint := range table.Constraints {
		if constraint.Mutation || constraint.Index != index {
			continue
		}
		switch constraint.Kind {
		case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
		default:
			continue
		}
		if found {
			return false, false, false, &model.Error{Code: "XX000", Message: "index is owned by multiple public constraints"}
		}
		found = true
		primary = constraint.Kind == model.ConstraintPrimaryKey
		exclusion = constraint.Kind == model.ConstraintExclude
		immediate = !constraint.Deferrable
	}
	return primary, exclusion, immediate, nil
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
