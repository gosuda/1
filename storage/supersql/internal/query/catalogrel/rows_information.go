package catalogrel

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/viewmutation"
)

func emitInformationSchemata(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitCatalogRow(relation, emit,
		fv("catalog_name", stringValue(options.Database.Name)),
		fv("schema_name", stringValue("pg_catalog")),
		fv("schema_owner", stringValue(options.Database.RoleName))); err != nil {
		return err
	}
	return scanSchemas(ctx, snapshot, options, func(schema schemaDescriptor) error {
		if schema.oid == 11 {
			return nil
		}
		owner, err := principalName(schema.owner)
		if err != nil {
			return err
		}
		return emitCatalogRow(relation, emit, fv("catalog_name", stringValue(options.Database.Name)), fv("schema_name", stringValue(schema.name)), fv("schema_owner", stringValue(owner)))
	})
}

func emitInformationCatalogConstants(relation Relation, options SourceOptions, emit rowEmitter) error {
	switch relation.ID {
	case IDCharacterSets:
		return emitCatalogRow(relation, emit,
			fv("character_set_name", stringValue("UTF8")), fv("character_repertoire", stringValue("UCS")), fv("form_of_use", stringValue("UTF8")),
			fv("default_collate_catalog", stringValue(options.Database.Name)), fv("default_collate_schema", stringValue("pg_catalog")), fv("default_collate_name", stringValue(options.Database.Collation)))
	case IDCollations, IDCollationCharacterSetApplicability:
		for info := range builtin.Collations() {
			fields := []fieldValue{
				fv("collation_catalog", stringValue(options.Database.Name)), fv("collation_schema", stringValue("pg_catalog")), fv("collation_name", stringValue(info.Name)),
			}
			if relation.ID == IDCollations {
				fields = append(fields, fv("pad_attribute", stringValue("NO PAD")))
			} else {
				fields = append(fields, fv("character_set_name", stringValue("UTF8")))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	default:
		return &model.Error{Code: "XX000", Message: "unknown information schema constant projection"}
	}
}

func emitLogicalInformationTables(relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, logical := range options.Registry.All() {
		tableType := "BASE TABLE"
		if logicalRelationDeclaration(logical) == DeclarationView {
			tableType = "VIEW"
		}
		if err := emitCatalogRow(relation, emit,
			fv("table_catalog", stringValue(options.Database.Name)),
			fv("table_schema", stringValue(logical.Namespace)),
			fv("table_name", stringValue(logical.Name)),
			fv("table_type", stringValue(tableType)),
			fv("is_insertable_into", stringValue("NO")),
			fv("is_typed", stringValue("NO"))); err != nil {
			return err
		}
	}
	return nil
}
func emitLogicalInformationViews(relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, logical := range options.Registry.All() {
		if logicalRelationDeclaration(logical) != DeclarationView {
			continue
		}
		if err := emitCatalogRow(relation, emit,
			fv("table_catalog", stringValue(options.Database.Name)),
			fv("table_schema", stringValue(logical.Namespace)),
			fv("table_name", stringValue(logical.Name)),
			fv("view_definition", stringValue(logical.ViewDefinition.SQL)),
			fv("check_option", stringValue("NONE")),
			fv("is_updatable", stringValue("NO")),
			fv("is_insertable_into", stringValue("NO")),
			fv("is_trigger_updatable", stringValue("NO")),
			fv("is_trigger_deletable", stringValue("NO")),
			fv("is_trigger_insertable_into", stringValue("NO"))); err != nil {
			return err
		}
	}
	return nil
}


func emitInformationSchema(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	switch relation.ID {
	case IDTables:
		if err := emitLogicalInformationTables(relation, options, emit); err != nil {
			return err
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable, model.ObjectView}, func(object objectDescriptor) error {
			tableType := "BASE TABLE"
			insertable := object.spec.Name.Kind == model.ObjectTable
			if object.spec.Name.Kind == model.ObjectView {
				capabilities, err := viewmutation.RelationCapabilities(ctx, snapshot, object.published.Descriptor, false, nil)
				if err != nil {
					return err
				}
				insertable = capabilities&viewmutation.Insert != 0
			}
			persistence := ""
			if object.spec.Table != nil {
				persistence = object.spec.Table.Persistence
			} else if object.spec.View != nil {
				persistence = object.spec.View.Persistence
			}
			if persistence == "t" {
				tableType = "LOCAL TEMPORARY"
			} else if object.spec.Name.Kind == model.ObjectView {
				tableType = "VIEW"
			}
			fields := []fieldValue{
				fv("table_catalog", stringValue(options.Database.Name)),
				fv("table_schema", stringValue(object.schema.name)),
				fv("table_name", stringValue(object.spec.Name.Name)),
				fv("table_type", stringValue(tableType)),
				fv("is_insertable_into", stringValue(yesNo(insertable))),
				fv("is_typed", stringValue("NO")),
			}
			if object.spec.Table != nil && object.spec.Table.TypedOf != 0 {
				typed, ok, err := objectByID(ctx, snapshot, options, object.spec.Table.TypedOf)
				if err != nil {
					return err
				}
				if !ok || typed.spec.Type == nil || typed.spec.Type.DeclaredKind() != model.TypeKindComposite {
					return &model.Error{Code: "XX000", Message: "typed table references an invalid composite type"}
				}
				fields = append(fields,
					fv("user_defined_type_catalog", stringValue(options.Database.Name)),
					fv("user_defined_type_schema", stringValue(typed.schema.name)),
					fv("user_defined_type_name", stringValue(typed.spec.Name.Name)),
					fv("is_typed", stringValue("YES")),
				)
			}
			return emitCatalogRow(relation, emit, fields...)
		})
	case IDColumns:
		return emitInformationColumns(ctx, snapshot, relation, options, emit)
	case IDViews:
		if err := emitLogicalInformationViews(relation, options, emit); err != nil {
			return err
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectView}, func(object objectDescriptor) error {
			if object.spec.View == nil {
				return &model.Error{Code: "XX000", Message: "view metadata is unavailable"}
			}
			capabilities, err := viewmutation.RelationCapabilities(ctx, snapshot, object.published.Descriptor, false, nil)
			if err != nil {
				return err
			}
			triggerUpdate, triggerDelete, triggerInsert := viewTriggerCapabilities(object.spec.View)
			checkOption := "NONE"
			if object.spec.View.CheckOption != "" {
				checkOption = strings.ToUpper(object.spec.View.CheckOption)
			}
			return emitCatalogRow(relation, emit, fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)), fv("view_definition", stringValue(object.spec.View.Definition)), fv("check_option", stringValue(checkOption)), fv("is_updatable", stringValue(yesNo(capabilities&viewmutation.Update != 0 && capabilities&viewmutation.Delete != 0))), fv("is_insertable_into", stringValue(yesNo(capabilities&viewmutation.Insert != 0))), fv("is_trigger_updatable", stringValue(yesNo(triggerUpdate))), fv("is_trigger_deletable", stringValue(yesNo(triggerDelete))), fv("is_trigger_insertable_into", stringValue(yesNo(triggerInsert))))
		})
	case IDSequences:
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectSequence}, func(object objectDescriptor) error {
			sequence := object.spec.Sequence
			if sequence == nil { return nil }
			return emitCatalogRow(relation, emit,
				fv("sequence_catalog", stringValue(options.Database.Name)), fv("sequence_schema", stringValue(object.schema.name)), fv("sequence_name", stringValue(object.spec.Name.Name)),
				fv("data_type", stringValue(informationDataType(model.TypeLayout{Type: sequence.Type}))), fv("numeric_precision", intValue(typePrecision(sequence.Type.ID))), fv("numeric_precision_radix", intValue(2)), fv("numeric_scale", intValue(0)),
				fv("start_value", stringValue(strconv.FormatInt(sequence.Start, 10))), fv("minimum_value", stringValue(strconv.FormatInt(sequence.Min, 10))), fv("maximum_value", stringValue(strconv.FormatInt(sequence.Max, 10))),
				fv("increment", stringValue(strconv.FormatInt(sequence.Increment, 10))), fv("cycle_option", stringValue(yesNo(sequence.Cycle))))
		})
	case IDTableConstraints:
		return emitConstraints(ctx, snapshot, relation, options, emit, true)
	case IDCheckConstraints:
		return emitConstraints(ctx, snapshot, relation, options, emit, true)
	case IDKeyColumnUsage:
		return emitConstraints(ctx, snapshot, relation, options, emit, true)
	case IDTriggers:
		return emitTriggers(ctx, snapshot, relation, options, emit, true)
	case IDRoutines, IDParameters:
		return emitInformationRoutines(ctx, snapshot, relation, options, emit)
	case IDDomains, IDUserDefinedTypes:
		return emitInformationTypes(ctx, snapshot, relation, options, emit)
	case IDAttributes:
		return emitInformationAttributes(ctx, snapshot, relation, options, emit)
	default:
		return &model.Error{Code: "XX000", Message: "unimplemented information schema projection", Detail: relation.Name}
	}
}
func viewTriggerCapabilities(view *model.ViewMetadata) (update, delete, insert bool) {
	if view == nil {
		return false, false, false
	}
	for _, trigger := range view.Triggers {
		if !trigger.EnableMode.Fires(false) || !trigger.ForEachRow || trigger.Timing != "instead" {
			continue
		}
		for _, event := range trigger.Events {
			switch event {
			case "update":
				update = true
			case "delete":
				delete = true
			case "insert":
				insert = true
			}
		}
	}
	return update, delete, insert
}

func emitInformationUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	switch relation.ID {
	case IDConstraintColumnUsage, IDConstraintTableUsage, IDReferentialConstraints:
		return emitConstraintUsage(ctx, snapshot, relation, options, emit)
	case IDTriggeredUpdateColumns:
		return emitTriggeredUpdateColumns(ctx, snapshot, relation, options, emit)
	case IDViewColumnUsage, IDViewRoutineUsage, IDViewTableUsage:
		return emitObjectUsage(ctx, snapshot, relation, options, emit, model.ObjectView)
	case IDRoutineColumnUsage, IDRoutineRoutineUsage, IDRoutineSequenceUsage, IDRoutineTableUsage:
		return emitObjectUsage(ctx, snapshot, relation, options, emit, model.ObjectRoutine)
	case IDCheckConstraintRoutineUsage:
		return emitCheckRoutineUsage(ctx, snapshot, relation, options, emit)
	case IDColumnColumnUsage:
		return emitColumnColumnUsage(ctx, snapshot, relation, options, emit)
	}
	return &model.Error{Code: "XX000", Message: "unimplemented information schema usage projection", Detail: relation.Name}
}

func objectByID(ctx context.Context, snapshot Snapshot, options SourceOptions, id model.ObjectID) (objectDescriptor, bool, error) {
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return objectDescriptor{}, false, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return objectDescriptor{}, false, nil
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return objectDescriptor{}, false, err
	}
	namespace, err := snapshot.Descriptor(ctx, spec.Name.Parent)
	if err != nil {
		return objectDescriptor{}, false, err
	}
	if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic {
		return objectDescriptor{}, false, nil
	}
	return objectDescriptor{
		published: published,
		spec: spec,
		schema: schemaDescriptor{id: namespace.Descriptor.ID(), oid: namespace.Descriptor.OID(), name: namespace.Descriptor.Name().Name},
	}, true, nil
}

func specificName(object objectDescriptor) string {
	return informationSpecificName(object.spec.Name.Name, object.spec.OID)
}

func informationSpecificName(name string, oid model.OID) string {
	suffix := "_" + strconv.FormatUint(uint64(uint32(oid)), 10)
	end := min(len(name), 63-len(suffix))
	for end < len(name) && end > 0 && !utf8.RuneStart(name[end]) {
		end--
	}
	return name[:end] + suffix
}

func emitObjectUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter, sourceKind model.ObjectKind) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{sourceKind}, func(source objectDescriptor) error {
		if sourceKind == model.ObjectRoutine && source.spec.Routine == nil {
			return nil
		}
		seen := make(map[model.ObjectID]map[model.ColumnID]bool)
		for _, dependency := range source.spec.Dependencies {
			columns := seen[dependency.Object]
			if columns == nil {
				columns = make(map[model.ColumnID]bool)
				seen[dependency.Object] = columns
			}
			if columns[dependency.Column] {
				continue
			}
			columns[dependency.Column] = true
			target, ok, err := objectByID(ctx, snapshot, options, dependency.Object)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			fields := []fieldValue{
				fv("view_catalog", stringValue(options.Database.Name)), fv("view_schema", stringValue(source.schema.name)), fv("view_name", stringValue(source.spec.Name.Name)),
				fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(source.schema.name)), fv("specific_name", stringValue(specificName(source))),
				fv("routine_catalog", stringValue(options.Database.Name)), fv("routine_schema", stringValue(target.schema.name)), fv("routine_name", stringValue(target.spec.Name.Name)),
				fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(target.schema.name)), fv("table_name", stringValue(target.spec.Name.Name)),
				fv("sequence_catalog", stringValue(options.Database.Name)), fv("sequence_schema", stringValue(target.schema.name)), fv("sequence_name", stringValue(target.spec.Name.Name)),
			}
			switch relation.ID {
			case IDViewColumnUsage, IDRoutineColumnUsage:
				if dependency.Column == 0 || (target.spec.Name.Kind != model.ObjectTable && target.spec.Name.Kind != model.ObjectView) {
					continue
				}
				name := columnName(descriptorColumns(target.spec), dependency.Column)
				if name == "" {
					continue
				}
				fields = append(fields, fv("column_name", stringValue(name)))
			case IDViewTableUsage, IDRoutineTableUsage:
				if target.spec.Name.Kind != model.ObjectTable && target.spec.Name.Kind != model.ObjectView {
					continue
				}
				if dependency.Column != 0 {
					if columns[0] {
						continue
					}
					columns[0] = true
				}
			case IDViewRoutineUsage:
				if target.spec.Name.Kind != model.ObjectRoutine {
					continue
				}
			case IDRoutineRoutineUsage:
				if target.spec.Name.Kind != model.ObjectRoutine || target.spec.Routine == nil {
					continue
				}
			case IDRoutineSequenceUsage:
				if target.spec.Name.Kind != model.ObjectSequence {
					continue
				}
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitConstraintUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(source objectDescriptor) error {
		if source.spec.Table == nil {
			return nil
		}
		for _, constraint := range source.spec.Table.Constraints {
			if constraint.Mutation {
				continue
			}
			if relation.ID == IDReferentialConstraints {
				if constraint.Kind != model.ConstraintForeignKey || constraint.ReferencedObject == 0 {
					continue
				}
				target, ok, err := objectByID(ctx, snapshot, options, constraint.ReferencedObject)
				if err != nil {
					return err
				}
				if !ok || target.spec.Table == nil {
					continue
				}
				if constraint.ReferencedConstraint == 0 {
					return &model.Error{Code: "XX000", Message: "foreign key lacks referenced unique constraint", Detail: constraint.Name}
				}
				uniqueName := ""
				for _, candidate := range target.spec.Table.Constraints {
					if !candidate.Mutation && candidate.ID == constraint.ReferencedConstraint && (candidate.Kind == model.ConstraintPrimaryKey || candidate.Kind == model.ConstraintUnique) {
						uniqueName = candidate.Name
						break
					}
				}
				if uniqueName == "" {
					return &model.Error{Code: "XX000", Message: "referenced unique constraint is unavailable", Detail: constraint.Name}
				}
				if err := emitCatalogRow(relation, emit,
					fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(source.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
					fv("unique_constraint_catalog", stringValue(options.Database.Name)), fv("unique_constraint_schema", stringValue(target.schema.name)), fv("unique_constraint_name", stringValue(uniqueName)),
					fv("match_option", stringValue(constraintMatch(constraint.Match))), fv("update_rule", stringValue(constraintAction(constraint.OnUpdate))), fv("delete_rule", stringValue(constraintAction(constraint.OnDelete)))); err != nil {
					return err
				}
				continue
			}
			type targetColumn struct {
				object objectDescriptor
				column model.ColumnID
			}
			var targets []targetColumn
			if constraint.Kind == model.ConstraintForeignKey && constraint.ReferencedObject != 0 {
				target, ok, err := objectByID(ctx, snapshot, options, constraint.ReferencedObject)
				if err != nil {
					return err
				}
				if ok {
					for _, column := range constraint.ReferencedColumns {
						targets = append(targets, targetColumn{target, column})
					}
				}
			} else {
				for _, column := range constraint.Columns {
					targets = append(targets, targetColumn{source, column})
				}
			}
			for _, target := range targets {
				fields := []fieldValue{
					fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(target.object.schema.name)), fv("table_name", stringValue(target.object.spec.Name.Name)),
					fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(source.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
				}
				if relation.ID == IDConstraintColumnUsage {
					name := columnName(descriptorColumns(target.object.spec), target.column)
					if name == "" {
						continue
					}
					fields = append(fields, fv("column_name", stringValue(name)))
				}
				if err := emitCatalogRow(relation, emit, fields...); err != nil {
					return err
				}
				if relation.ID == IDConstraintTableUsage {
					break
				}
			}
		}
		return nil
	})
}

func constraintMatch(value string) string {
	switch strings.ToLower(value) {
	case "full":
		return "FULL"
	case "partial":
		return "PARTIAL"
	default:
		return "NONE"
	}
}

func constraintAction(value string) string {
	switch strings.ToLower(value) {
	case "cascade":
		return "CASCADE"
	case "set null":
		return "SET NULL"
	case "set default":
		return "SET DEFAULT"
	case "restrict":
		return "RESTRICT"
	default:
		return "NO ACTION"
	}
}

func emitTriggeredUpdateColumns(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable, model.ObjectView}, func(object objectDescriptor) error {
		var triggers []model.TriggerDescriptor
		if object.spec.Table != nil {
			triggers = object.spec.Table.Triggers
		} else if object.spec.View != nil {
			triggers = object.spec.View.Triggers
		}
		for _, trigger := range triggers {
			for _, column := range trigger.Columns {
				name := columnName(descriptorColumns(object.spec), column)
				if name == "" {
					continue
				}
				if err := emitCatalogRow(relation, emit,
					fv("trigger_catalog", stringValue(options.Database.Name)), fv("trigger_schema", stringValue(object.schema.name)), fv("trigger_name", stringValue(trigger.Name)),
					fv("event_object_catalog", stringValue(options.Database.Name)), fv("event_object_schema", stringValue(object.schema.name)), fv("event_object_table", stringValue(object.spec.Name.Name)),
					fv("event_object_column", stringValue(name))); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func emitCheckRoutineUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		if object.spec.Table == nil {
			return nil
		}
		for _, constraint := range object.spec.Table.Constraints {
			if constraint.Mutation {
				continue
			}
			if constraint.Kind != model.ConstraintCheck || constraint.Expression == nil {
				continue
			}
			seen := make(map[model.ObjectID]bool)
			for _, dependency := range constraint.Expression.Dependencies {
				if seen[dependency.Object] {
					continue
				}
				target, ok, err := objectByID(ctx, snapshot, options, dependency.Object)
				if err != nil {
					return err
				}
				if !ok || target.spec.Name.Kind != model.ObjectRoutine {
					continue
				}
				seen[dependency.Object] = true
				if err := emitCatalogRow(relation, emit,
					fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(object.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
					fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(target.schema.name)), fv("specific_name", stringValue(specificName(target)))); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func emitColumnColumnUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(source objectDescriptor) error {
		for _, dependent := range descriptorColumns(source.spec) {
			expressions := []*model.ExpressionDefinition{dependent.Default, dependent.Generated}
			seen := make(map[model.Dependency]bool)
			for _, expression := range expressions {
				if expression == nil {
					continue
				}
				for _, dependency := range expression.Dependencies {
					if dependency.Column == 0 || seen[dependency] {
						continue
					}
					seen[dependency] = true
					target, ok, err := objectByID(ctx, snapshot, options, dependency.Object)
					if err != nil {
						return err
					}
					if !ok || target.spec.Name.Kind != model.ObjectTable {
						continue
					}
					name := columnName(descriptorColumns(target.spec), dependency.Column)
					if name == "" {
						continue
					}
					if err := emitCatalogRow(relation, emit,
						fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(target.schema.name)), fv("table_name", stringValue(target.spec.Name.Name)),
						fv("column_name", stringValue(name)), fv("dependent_column", stringValue(dependent.Name))); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func emitLogicalInformationColumns(relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, logical := range options.Registry.All() {
		for _, column := range logical.Columns {
			typeInfo := describeInformationType(options.Registry, nil, column.Layout)
			fields := []fieldValue{
				fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(logical.Namespace)), fv("table_name", stringValue(logical.Name)), fv("column_name", stringValue(column.Name)),
				fv("ordinal_position", intValue(int64(column.ID))), fv("is_nullable", stringValue(yesNo(column.Nullable && !typeInfo.domainNotNull))), fv("data_type", stringValue(typeInfo.dataType)),
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)), fv("udt_name", stringValue(typeInfo.udtName)),
				fv("dtd_identifier", stringValue(strconv.FormatInt(int64(column.ID), 10))), fv("is_self_referencing", stringValue("NO")), fv("is_identity", stringValue("NO")), fv("is_generated", stringValue("NEVER")),
				fv("is_updatable", stringValue("NO")),
			}
			if typeInfo.domainName != "" {
				fields = append(fields,
					fv("domain_catalog", stringValue(options.Database.Name)),
					fv("domain_schema", stringValue(typeInfo.domainSchema)),
					fv("domain_name", stringValue(typeInfo.domainName)),
				)
			}
			appendTypeInformation(&fields, options.Database.Name, typeInfo.layout)
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
	}
	return nil
}

func emitInformationColumns(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	if err := emitLogicalInformationColumns(relation, options, emit); err != nil {
		return err
	}
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectView}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		for _, column := range descriptorColumns(object.spec) {
			if column.State != model.StatePublic || column.SystemIdentity { continue }
			typeInfo := describeInformationType(options.Registry, types, column.Layout)
			updatable := object.spec.Name.Kind == model.ObjectTable && column.Generated == nil
			if object.spec.Name.Kind == model.ObjectView {
				capabilities, err := viewmutation.RelationCapabilities(ctx, snapshot, object.published.Descriptor, false, []model.ColumnID{column.ID})
				if err != nil {
					return err
				}
				updatable = capabilities&viewmutation.Update != 0
			}
			fields := []fieldValue{
				fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)), fv("column_name", stringValue(column.Name)),
				fv("ordinal_position", intValue(int64(column.ID))), fv("is_nullable", stringValue(yesNo(column.Nullable && !typeInfo.domainNotNull))), fv("data_type", stringValue(typeInfo.dataType)),
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)), fv("udt_name", stringValue(typeInfo.udtName)),
				fv("dtd_identifier", stringValue(strconv.FormatInt(int64(column.ID), 10))), fv("is_self_referencing", stringValue("NO")), fv("is_identity", stringValue(yesNo(column.IdentitySequence != 0))), fv("is_generated", stringValue(generationName(column))),
				fv("is_updatable", stringValue(yesNo(updatable))),
			}
			if column.Default != nil {
				fields = append(fields, fv("column_default", stringValue(column.Default.SQL)))
			} else if column.ConstantDefault != nil {
				fields = append(fields, fv("column_default", stringValue(formatSettingValue(*column.ConstantDefault))))
			}
			if typeInfo.domainName != "" {
				fields = append(fields,
					fv("domain_catalog", stringValue(options.Database.Name)),
					fv("domain_schema", stringValue(typeInfo.domainSchema)),
					fv("domain_name", stringValue(typeInfo.domainName)),
				)
			}
			appendTypeInformation(&fields, options.Database.Name, typeInfo.layout)
			if column.IdentitySequence != 0 {
				sequence, ok, err := objectByID(ctx, snapshot, options, column.IdentitySequence)
				if err != nil {
					return err
				}
				if !ok || sequence.spec.Sequence == nil {
					return &model.Error{Code: "XX000", Message: "identity column sequence descriptor is unavailable"}
				}
				fields = append(fields,
					fv("identity_generation", stringValue(identityGeneration(column))),
					fv("identity_start", stringValue(strconv.FormatInt(sequence.spec.Sequence.Start, 10))),
					fv("identity_increment", stringValue(strconv.FormatInt(sequence.spec.Sequence.Increment, 10))),
					fv("identity_maximum", stringValue(strconv.FormatInt(sequence.spec.Sequence.Max, 10))),
					fv("identity_minimum", stringValue(strconv.FormatInt(sequence.spec.Sequence.Min, 10))),
					fv("identity_cycle", stringValue(yesNo(sequence.spec.Sequence.Cycle))),
				)
			}
			if column.Generated != nil { fields = append(fields, fv("generation_expression", stringValue(column.Generated.SQL))) }
			if err := emitCatalogRow(relation, emit, fields...); err != nil { return err }
		}
		return nil
	})
}

type informationTypeDetails struct {
	layout                   model.TypeLayout
	dataType                 string
	udtSchema, udtName       string
	domainSchema, domainName string
	domainNotNull            bool
}

func describeInformationType(registry *Registry, types map[model.TypeID]objectDescriptor, layout model.TypeLayout) informationTypeDetails {
	details := informationTypeDetails{layout: layout}
	if domain, ok := registry.InformationDomainByType(layout.Type.ID); ok {
		details.layout = domain.Definition.Base
		details.domainSchema, details.domainName, details.domainNotNull = "information_schema", domain.Definition.Name, domain.Definition.NotNull
	}
	if declared, ok := types[details.layout.Type.ID]; ok && declared.spec.Type != nil && declared.spec.Type.DeclaredKind() == model.TypeKindDomain {
		details.domainSchema, details.domainName, details.domainNotNull = declared.schema.name, declared.spec.Name.Name, declared.spec.Type.NotNull
		details.layout = declared.spec.Type.BaseLayout
	}
	if declared, ok := types[details.layout.Type.ID]; ok && declared.spec.Type != nil {
		details.udtSchema, details.udtName = declared.schema.name, declared.spec.Name.Name
		if declared.spec.Type.DeclaredKind() == model.TypeKindArray {
			details.dataType = "ARRAY"
		} else {
			details.dataType = "USER-DEFINED"
		}
		return details
	}
	details.udtSchema, details.udtName = typeSchema(details.layout.Type.ID), typeName(details.layout.Type.ID)
	details.dataType = informationDataType(details.layout)
	return details
}

func appendTypeInformation(fields *[]fieldValue, catalog string, layout model.TypeLayout) {
	id, modifier := layout.Type.ID, layout.Type.Modifier
	switch id {
	case 21:
		*fields = append(*fields, fv("numeric_precision", intValue(16)), fv("numeric_precision_radix", intValue(2)), fv("numeric_scale", intValue(0)))
	case 23:
		*fields = append(*fields, fv("numeric_precision", intValue(32)), fv("numeric_precision_radix", intValue(2)), fv("numeric_scale", intValue(0)))
	case 20:
		*fields = append(*fields, fv("numeric_precision", intValue(64)), fv("numeric_precision_radix", intValue(2)), fv("numeric_scale", intValue(0)))
	case 700:
		*fields = append(*fields, fv("numeric_precision", intValue(24)), fv("numeric_precision_radix", intValue(2)))
	case 701:
		*fields = append(*fields, fv("numeric_precision", intValue(53)), fv("numeric_precision_radix", intValue(2)))
	case 1700:
		*fields = append(*fields, fv("numeric_precision_radix", intValue(10)))
		if modifier != -1 {
			*fields = append(*fields, fv("numeric_precision", intValue(int64(uint32(modifier-4)>>16))), fv("numeric_scale", intValue(int64((modifier-4)&0xffff))))
		}
	case 1560, 1562:
		if modifier != -1 {
			*fields = append(*fields, fv("character_maximum_length", intValue(int64(modifier))))
		}
	case 25, 1042, 1043:
		if id == 25 || modifier == -1 {
			*fields = append(*fields, fv("character_octet_length", intValue(1<<30)))
		} else {
			*fields = append(*fields, fv("character_maximum_length", intValue(int64(modifier-4))), fv("character_octet_length", intValue(int64(modifier-4)*4)))
		}
		*fields = append(*fields, fv("character_set_catalog", stringValue(catalog)), fv("character_set_schema", stringValue("pg_catalog")), fv("character_set_name", stringValue("UTF8")))
	case 1082:
		*fields = append(*fields, fv("datetime_precision", intValue(0)))
	case 1083, 1114, 1184, 1266:
		precision := int64(modifier)
		if modifier < 0 {
			precision = 6
		}
		*fields = append(*fields, fv("datetime_precision", intValue(precision)))
	case 1186:
		precision := int64(modifier & 0xffff)
		if modifier < 0 || modifier&0xffff == 0xffff {
			precision = 6
		}
		*fields = append(*fields, fv("datetime_precision", intValue(precision)))
		if interval := informationIntervalType(modifier); interval != "" {
			*fields = append(*fields, fv("interval_type", stringValue(interval)))
		}
	}
	baseCollation := model.CollationID(0)
	if info, ok := builtin.LookupType(layout.Type.ID); ok {
		baseCollation = info.Collation
	}
	if layout.Type.Collation != 0 && layout.Type.Collation != baseCollation {
		if collation, ok := builtin.LookupCollation(layout.Type.Collation); ok {
			*fields = append(*fields, fv("collation_catalog", stringValue(catalog)), fv("collation_schema", stringValue("pg_catalog")), fv("collation_name", stringValue(collation.Name)))
		}
	}
}

func informationIntervalType(modifier int32) string {
	if modifier < 0 {
		return ""
	}
	switch (modifier >> 16) & 0x7fff {
	case 1:
		return "YEAR"
	case 2:
		return "MONTH"
	case 3:
		return "YEAR TO MONTH"
	case 4:
		return "DAY"
	case 8:
		return "HOUR"
	case 12:
		return "DAY TO HOUR"
	case 16:
		return "MINUTE"
	case 24:
		return "HOUR TO MINUTE"
	case 28:
		return "DAY TO MINUTE"
	case 32:
		return "SECOND"
	case 48:
		return "MINUTE TO SECOND"
	case 56:
		return "HOUR TO SECOND"
	case 60:
		return "DAY TO SECOND"
	default:
		return ""
	}
}

func generationName(column model.ColumnDescriptor) string {
	if column.Generated == nil { return "NEVER" }
	if column.GeneratedStored { return "ALWAYS" }
	return "ALWAYS"
}

func identityGeneration(column model.ColumnDescriptor) string {
	if column.IdentityAlways { return "ALWAYS" }
	return "BY DEFAULT"
}

func informationDataType(layout model.TypeLayout) string {
	if info, ok := builtin.LookupType(layout.Type.ID); ok {
		if info.Category == 'A' {
			return "ARRAY"
		}
		switch info.ID {
		case 16:
			return "boolean"
		case 20:
			return "bigint"
		case 21:
			return "smallint"
		case 23:
			return "integer"
		case 700:
			return "real"
		case 701:
			return "double precision"
		case 1042:
			return "character"
		case 1043:
			return "character varying"
		case 1083:
			return "time without time zone"
		case 1114:
			return "timestamp without time zone"
		case 1184:
			return "timestamp with time zone"
		case 1266:
			return "time with time zone"
		case 1562:
			return "bit varying"
		default:
			return info.Name
		}
	}
	return "USER-DEFINED"
}

func typeName(id model.TypeID) string {
	if info, ok := builtin.LookupType(id); ok { return info.Name }
	return "USER-DEFINED"
}

func typeSchema(id model.TypeID) string {
	if _, ok := builtin.LookupType(id); ok { return "pg_catalog" }
	return "public"
}

func typePrecision(id model.TypeID) int64 {
	switch id { case 21: return 16; case 23: return 32; default: return 64 }
}

func emitInformationConstraint(ctx context.Context, snapshot Snapshot, relation Relation, emit rowEmitter, options SourceOptions, object objectDescriptor, constraint model.ConstraintDescriptor) error {
	switch relation.ID {
	case IDTableConstraints:
		if constraint.Kind == model.ConstraintExclude || constraint.Kind == model.ConstraintTrigger {
			return nil
		}
		fields := []fieldValue{
			fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(object.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
			fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)),
			fv("constraint_type", stringValue(informationConstraintType(constraint.Kind))), fv("is_deferrable", stringValue(yesNo(constraint.Deferrable))), fv("initially_deferred", stringValue(yesNo(constraint.InitiallyDeferred))), fv("enforced", stringValue(yesNo(constraint.Enforced))),
		}
		if constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique {
			index, err := constraintIndexMetadata(ctx, snapshot, constraint.Index)
			if err != nil {
				return err
			}
			fields = append(fields, fv("nulls_distinct", stringValue(yesNo(!index.NullsNotDistinct))))
		}
		return emitCatalogRow(relation, emit, fields...)
	case IDCheckConstraints:
		if constraint.Kind == model.ConstraintCheck && constraint.Expression != nil {
			return emitCatalogRow(relation, emit,
				fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(object.schema.name)),
				fv("constraint_name", stringValue(constraint.Name)), fv("check_clause", stringValue(constraint.Expression.SQL)))
		}
		if constraint.Kind != model.ConstraintNotNull {
			return nil
		}
		name := "VALUE"
		if object.spec.Table != nil {
			if len(constraint.Columns) != 1 {
				return &model.Error{Code: "XX000", Message: "not-null constraint column metadata is incomplete"}
			}
			name = columnName(object.spec.Table.Columns, constraint.Columns[0])
			if name == "" {
				return &model.Error{Code: "XX000", Message: "not-null constraint column is unavailable"}
			}
		}
		return emitCatalogRow(relation, emit,
			fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(object.schema.name)),
			fv("constraint_name", stringValue(constraint.Name)), fv("check_clause", stringValue(name+" IS NOT NULL")))
	case IDKeyColumnUsage:
		if constraint.Kind != model.ConstraintPrimaryKey && constraint.Kind != model.ConstraintUnique && constraint.Kind != model.ConstraintForeignKey {
			return nil
		}
		var referenced model.IndexMetadata
		if constraint.Kind == model.ConstraintForeignKey {
			var err error
			referenced, err = constraintIndexMetadata(ctx, snapshot, constraint.ReferencedIndex)
			if err != nil {
				return err
			}
		}
		for i, columnID := range constraint.Columns {
			name := columnName(object.spec.Table.Columns, columnID)
			if name == "" {
				return &model.Error{Code: "XX000", Message: "constraint column is unavailable"}
			}
			fields := []fieldValue{
				fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(object.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
				fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)), fv("column_name", stringValue(name)), fv("ordinal_position", intValue(int64(i+1))),
			}
			if constraint.Kind == model.ConstraintForeignKey {
				if i >= len(constraint.ReferencedColumns) {
					return &model.Error{Code: "XX000", Message: "foreign-key column metadata is incomplete"}
				}
				position := 0
				for keyIndex, key := range referenced.Keys {
					if key.Column == constraint.ReferencedColumns[i] {
						position = keyIndex + 1
						break
					}
				}
				if position == 0 {
					return &model.Error{Code: "XX000", Message: "referenced unique index column is unavailable"}
				}
				fields = append(fields, fv("position_in_unique_constraint", intValue(int64(position))))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
	}
	return nil
}

func constraintIndexMetadata(ctx context.Context, snapshot Snapshot, id model.ObjectID) (model.IndexMetadata, error) {
	if id == 0 {
		return model.IndexMetadata{}, &model.Error{Code: "XX000", Message: "constraint index metadata is unavailable"}
	}
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return model.IndexMetadata{}, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return model.IndexMetadata{}, &model.Error{Code: "XX000", Message: "constraint index descriptor is unavailable"}
	}
	index, ok := published.Descriptor.Index()
	if !ok {
		return model.IndexMetadata{}, &model.Error{Code: "XX000", Message: "constraint index metadata is unavailable"}
	}
	return index, nil
}

func informationConstraintType(kind model.ConstraintKind) string {
	switch kind {
	case model.ConstraintCheck: return "CHECK"
	case model.ConstraintForeignKey: return "FOREIGN KEY"
	case model.ConstraintPrimaryKey: return "PRIMARY KEY"
	case model.ConstraintUnique: return "UNIQUE"
	case model.ConstraintNotNull: return "CHECK"
	default: return ""
	}
}

func columnName(columns []model.ColumnDescriptor, id model.ColumnID) string {
	for _, column := range columns { if column.ID == id { return column.Name } }
	return ""
}

func emitInformationTrigger(ctx context.Context, snapshot Snapshot, relation Relation, emit rowEmitter, options SourceOptions, object objectDescriptor, triggers []model.TriggerDescriptor, trigger model.TriggerDescriptor) error {
	action, err := triggerActionStatement(ctx, snapshot, trigger)
	if err != nil {
		return err
	}
	for _, event := range trigger.Events {
		if event != "insert" && event != "delete" && event != "update" {
			continue
		}
		fields := []fieldValue{
			fv("trigger_catalog", stringValue(options.Database.Name)), fv("trigger_schema", stringValue(object.schema.name)), fv("trigger_name", stringValue(trigger.Name)),
			fv("event_manipulation", stringValue(strings.ToUpper(event))), fv("event_object_catalog", stringValue(options.Database.Name)), fv("event_object_schema", stringValue(object.schema.name)), fv("event_object_table", stringValue(object.spec.Name.Name)),
			fv("action_order", intValue(triggerActionOrder(triggers, trigger, event))), fv("action_statement", stringValue(action)), fv("action_orientation", stringValue(triggerOrientation(trigger))), fv("action_timing", stringValue(triggerActionTiming(trigger))),
		}
		if trigger.When != nil {
			fields = append(fields, fv("action_condition", stringValue(trigger.When.SQL)))
		}
		if trigger.OldTransitionTable != "" {
			fields = append(fields, fv("action_reference_old_table", stringValue(trigger.OldTransitionTable)))
		}
		if trigger.NewTransitionTable != "" {
			fields = append(fields, fv("action_reference_new_table", stringValue(trigger.NewTransitionTable)))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}

func triggerActionStatement(ctx context.Context, snapshot Snapshot, trigger model.TriggerDescriptor) (string, error) {
	name := ""
	if trigger.Routine != 0 {
		published, err := snapshot.Descriptor(ctx, trigger.Routine)
		if err != nil {
			return "", err
		}
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return "", &model.Error{Code: "XX000", Message: "trigger routine descriptor is unavailable"}
		}
		name = published.Descriptor.Name().Name
	} else if signature, ok := builtin.LookupSignature(trigger.Function); ok {
		name = signature.Name
	}
	if name == "" {
		return "", &model.Error{Code: "55000", Message: "trigger routine catalog metadata is unavailable"}
	}
	arguments := make([]string, len(trigger.Arguments))
	for i, argument := range trigger.Arguments {
		arguments[i] = "'" + strings.ReplaceAll(argument, "'", "''") + "'"
	}
	return "EXECUTE FUNCTION " + quoteIdentifier(trigger.SchemaName) + "." + quoteIdentifier(name) + "(" + strings.Join(arguments, ", ") + ")", nil
}

func triggerActionOrder(triggers []model.TriggerDescriptor, trigger model.TriggerDescriptor, event string) int64 {
	order := int64(1)
	for _, candidate := range triggers {
		if candidate.Name >= trigger.Name || candidate.Timing != trigger.Timing || candidate.ForEachRow != trigger.ForEachRow {
			continue
		}
		for _, candidateEvent := range candidate.Events {
			if candidateEvent == event {
				order++
				break
			}
		}
	}
	return order
}

func triggerOrientation(trigger model.TriggerDescriptor) string {
	if trigger.ForEachRow { return "ROW" }
	return "STATEMENT"
}

func triggerActionTiming(trigger model.TriggerDescriptor) string {
	if trigger.Timing == "instead" {
		return "INSTEAD OF"
	}
	return strings.ToUpper(trigger.Timing)
}
func informationLanguage(id model.OID) (string, bool) {
	switch id {
	case 12:
		return "internal", true
	case 13:
		return "c", true
	case 14:
		return "sql", true
	default:
		return "", false
	}
}

func emitBuiltinInformationRoutines(relation Relation, options SourceOptions, emit rowEmitter) error {
	for signature := range builtin.Signatures() {
		if err := emitBuiltinInformationRoutine(relation, options, emit, signature, "pg_catalog", 0); err != nil {
			return err
		}
	}
	for _, function := range options.Registry.InformationSchemaFunctions() {
		if err := emitBuiltinInformationRoutine(relation, options, emit, function.Signature, "information_schema", function.Operation); err != nil {
			return err
		}
	}
	return nil
}

func emitBuiltinInformationRoutine(relation Relation, options SourceOptions, emit rowEmitter, signature builtin.Signature, schema string, operation builtin.InformationSchemaFunction) error {
	if relation.ID == IDParameters {
		return emitBuiltinInformationParameters(relation, options, emit, signature, schema, operation)
	}
	language, ok := informationLanguage(signature.Language)
	if !ok {
		return &model.Error{Code: "XX000", Message: "builtin routine language is unavailable", Detail: signature.Name}
	}
	routineBody := "EXTERNAL"
	if language == "sql" {
		routineBody = "SQL"
	}
	fields := []fieldValue{
		fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(schema)), fv("specific_name", stringValue(informationSpecificName(signature.Name, model.OID(signature.ID)))),
		fv("routine_catalog", stringValue(options.Database.Name)), fv("routine_schema", stringValue(schema)), fv("routine_name", stringValue(signature.Name)),
		fv("routine_body", stringValue(routineBody)), fv("routine_definition", stringValue(signature.Source)),
		fv("external_language", stringValue(strings.ToUpper(language))), fv("parameter_style", stringValue("GENERAL")), fv("is_deterministic", stringValue(yesNo(signature.Volatility == 'i'))),
		fv("sql_data_access", stringValue("MODIFIES")), fv("schema_level_routine", stringValue("YES")), fv("max_dynamic_result_sets", intValue(0)),
		fv("security_type", stringValue(securityType(signature.SecurityDefiner))), fv("as_locator", stringValue("NO")), fv("is_udt_dependent", stringValue("NO")),
	}
	switch signature.Kind {
	case 'f':
		fields = append(fields, fv("routine_type", stringValue("FUNCTION")))
	case 'p':
		fields = append(fields, fv("routine_type", stringValue("PROCEDURE")))
	}
	if signature.Kind != 'p' {
		typeInfo := describeInformationType(options.Registry, nil, catalogLayout(signature.Result, -1))
		fields = append(fields,
			fv("data_type", stringValue(typeInfo.dataType)), fv("type_udt_catalog", stringValue(options.Database.Name)),
			fv("type_udt_schema", stringValue(typeInfo.udtSchema)), fv("type_udt_name", stringValue(typeInfo.udtName)),
			fv("dtd_identifier", stringValue("0")), fv("is_null_call", stringValue(yesNo(signature.Strict))),
		)
	}
	if language == "c" {
		fields = append(fields, fv("external_name", stringValue(signature.Source)))
	}
	return emitCatalogRow(relation, emit, fields...)
}

type informationSignatureArguments struct {
	count    int
	typeIDs  [builtin.MaxCallArguments]model.TypeID
	modes    [builtin.MaxCallArguments]byte
	names    [builtin.MaxCallArguments]string
	defaults [builtin.MaxCallArguments]string
}

func informationRoutineArguments(signature builtin.Signature, operation builtin.InformationSchemaFunction) informationSignatureArguments {
	result := informationSignatureArguments{count: int(signature.ArgumentCount)}
	for i := range result.count {
		result.typeIDs[i], result.modes[i] = signature.Arguments[i], 'i'
		if signature.Variadic != 0 && i == result.count-1 {
			result.modes[i] = 'v'
		}
	}
	parameters, hasParameters := builtin.LookupFunctionParameters(signature.ID)
	if hasParameters && parameters.AllArgumentCount() != 0 {
		result.count = parameters.AllArgumentCount()
		for i := range result.count {
			result.typeIDs[i], result.modes[i], result.names[i], _ = parameters.AllArgument(i)
		}
	} else if hasParameters {
		for i := range result.count {
			result.names[i], _ = parameters.ArgumentName(i)
		}
	}
	if allTypes, allModes, allNames, count, ok := builtin.InformationSchemaOutputArguments(operation); ok {
		result.count = int(count)
		for i := range result.count {
			result.typeIDs[i], result.modes[i], result.names[i] = allTypes[i], allModes[i], allNames[i]
		}
	} else if inputNames, count, ok := builtin.InformationSchemaInputArguments(operation); ok {
		for i := range min(result.count, int(count)) {
			result.names[i] = inputNames[i]
		}
	}
	input := 0
	for i := range result.count {
		if result.modes[i] != 'i' && result.modes[i] != 'b' && result.modes[i] != 'v' {
			continue
		}
		if hasParameters {
			result.defaults[i], _ = parameters.DefaultSQL(input)
		}
		input++
	}
	return result
}

func emitBuiltinInformationParameters(relation Relation, options SourceOptions, emit rowEmitter, signature builtin.Signature, schema string, operation builtin.InformationSchemaFunction) error {
	arguments := informationRoutineArguments(signature, operation)
	specific := informationSpecificName(signature.Name, model.OID(signature.ID))
	for i := range arguments.count {
		typeInfo := describeInformationType(options.Registry, nil, catalogLayout(arguments.typeIDs[i], -1))
		fields := []fieldValue{
			fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(schema)), fv("specific_name", stringValue(specific)), fv("ordinal_position", intValue(int64(i+1))),
			fv("parameter_mode", stringValue(informationParameterMode(string([]byte{arguments.modes[i]})))), fv("is_result", stringValue("NO")), fv("as_locator", stringValue("NO")),
			fv("data_type", stringValue(typeInfo.dataType)), fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)),
			fv("udt_name", stringValue(typeInfo.udtName)), fv("dtd_identifier", stringValue(strconv.Itoa(i+1))),
		}
		if arguments.names[i] != "" {
			fields = append(fields, fv("parameter_name", stringValue(arguments.names[i])))
		}
		if arguments.defaults[i] != "" {
			fields = append(fields, fv("parameter_default", stringValue(arguments.defaults[i])))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}


func emitInformationRoutines(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	if err := emitBuiltinInformationRoutines(relation, options, emit); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectRoutine}, func(object objectDescriptor) error {
		if object.spec.Aggregate != nil {
			return emitInformationAggregate(relation, options, object, types, emit)
		}
		routine := object.spec.Routine
		if routine == nil {
			return nil
		}
		specificName := informationSpecificName(object.spec.Name.Name, object.spec.OID)
		routineType := "FUNCTION"
		if routine.Procedure {
			routineType = "PROCEDURE"
		}
		if relation.ID == IDRoutines {
			language := strings.ToLower(routine.Language)
			routineBody := "EXTERNAL"
			if language == "sql" {
				routineBody = "SQL"
			}
			fields := []fieldValue{
				fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(object.schema.name)), fv("specific_name", stringValue(specificName)),
				fv("routine_catalog", stringValue(options.Database.Name)), fv("routine_schema", stringValue(object.schema.name)), fv("routine_name", stringValue(object.spec.Name.Name)), fv("routine_type", stringValue(routineType)),
				fv("routine_body", stringValue(routineBody)), fv("routine_definition", stringValue(routine.Source)), fv("external_language", stringValue(strings.ToUpper(language))),
				fv("parameter_style", stringValue("GENERAL")), fv("is_deterministic", stringValue(yesNo(routine.Volatility == "immutable"))), fv("sql_data_access", stringValue("MODIFIES")),
				fv("schema_level_routine", stringValue("YES")), fv("max_dynamic_result_sets", intValue(0)), fv("security_type", stringValue(securityType(routine.SecurityDefiner))),
				fv("as_locator", stringValue("NO")), fv("is_udt_dependent", stringValue("NO")),
			}
			if !routine.Procedure {
				typeInfo := describeInformationType(options.Registry, types, routine.Result)
				fields = append(fields,
					fv("data_type", stringValue(typeInfo.dataType)),
					fv("type_udt_catalog", stringValue(options.Database.Name)),
					fv("type_udt_schema", stringValue(typeInfo.udtSchema)),
					fv("type_udt_name", stringValue(typeInfo.udtName)),
					fv("dtd_identifier", stringValue("0")),
					fv("is_null_call", stringValue(yesNo(routine.Strict))),
				)
			}
			if language == "c" && routine.ExternalCode != "" {
				fields = append(fields, fv("external_name", stringValue(routine.ExternalCode)))
			}
			return emitCatalogRow(relation, emit, fields...)
		}
		for i, parameter := range routine.Parameters {
			typeInfo := describeInformationType(options.Registry, types, parameter.Layout)
			fields := []fieldValue{
				fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(object.schema.name)), fv("specific_name", stringValue(specificName)), fv("ordinal_position", intValue(int64(i+1))),
				fv("parameter_mode", stringValue(informationParameterMode(parameter.Mode))), fv("is_result", stringValue("NO")), fv("as_locator", stringValue("NO")), fv("data_type", stringValue(typeInfo.dataType)),
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)), fv("udt_name", stringValue(typeInfo.udtName)), fv("dtd_identifier", stringValue(strconv.Itoa(i+1))),
			}
			if parameter.Name != "" {
				fields = append(fields, fv("parameter_name", stringValue(parameter.Name)))
			}
			if parameter.Default != nil {
				fields = append(fields, fv("parameter_default", stringValue(parameter.Default.SQL)))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitInformationAggregate(relation Relation, options SourceOptions, object objectDescriptor, types map[model.TypeID]objectDescriptor, emit rowEmitter) error {
	aggregate := object.spec.Aggregate
	specificName := informationSpecificName(object.spec.Name.Name, object.spec.OID)
	if relation.ID == IDRoutines {
		typeInfo := describeInformationType(options.Registry, types, aggregate.Result)
		return emitCatalogRow(relation, emit,
			fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(object.schema.name)), fv("specific_name", stringValue(specificName)),
			fv("routine_catalog", stringValue(options.Database.Name)), fv("routine_schema", stringValue(object.schema.name)), fv("routine_name", stringValue(object.spec.Name.Name)),
			fv("routine_body", stringValue("EXTERNAL")), fv("routine_definition", stringValue("aggregate_dummy")), fv("external_language", stringValue("INTERNAL")),
			fv("parameter_style", stringValue("GENERAL")), fv("is_deterministic", stringValue(yesNo(aggregate.Volatility == "immutable"))), fv("sql_data_access", stringValue("MODIFIES")),
			fv("schema_level_routine", stringValue("YES")), fv("max_dynamic_result_sets", intValue(0)), fv("security_type", stringValue("INVOKER")),
			fv("as_locator", stringValue("NO")), fv("is_udt_dependent", stringValue("NO")),
			fv("data_type", stringValue(typeInfo.dataType)), fv("type_udt_catalog", stringValue(options.Database.Name)),
			fv("type_udt_schema", stringValue(typeInfo.udtSchema)), fv("type_udt_name", stringValue(typeInfo.udtName)),
			fv("dtd_identifier", stringValue("0")), fv("is_null_call", stringValue("NO")))
	}
	for i, argument := range aggregate.Arguments {
		typeInfo := describeInformationType(options.Registry, types, argument)
		fields := []fieldValue{
			fv("specific_catalog", stringValue(options.Database.Name)), fv("specific_schema", stringValue(object.schema.name)), fv("specific_name", stringValue(specificName)), fv("ordinal_position", intValue(int64(i+1))),
			fv("parameter_mode", stringValue("IN")), fv("is_result", stringValue("NO")), fv("as_locator", stringValue("NO")), fv("data_type", stringValue(typeInfo.dataType)),
			fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)), fv("udt_name", stringValue(typeInfo.udtName)), fv("dtd_identifier", stringValue(strconv.Itoa(i+1))),
		}
		if len(aggregate.ArgumentNames) != 0 && aggregate.ArgumentNames[i] != "" {
			fields = append(fields, fv("parameter_name", stringValue(aggregate.ArgumentNames[i])))
		}
		if err := emitCatalogRow(relation, emit, fields...); err != nil {
			return err
		}
	}
	return nil
}

func securityType(definer bool) string { if definer { return "DEFINER" }; return "INVOKER" }

func informationParameterMode(mode string) string {
	switch mode {
	case "table", "out", "o", "t":
		return "OUT"
	case "variadic", "in", "i", "v":
		return "IN"
	case "inout", "b":
		return "INOUT"
	default:
		return strings.ToUpper(mode)
	}
}

func emitInformationTypes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	var types map[model.TypeID]objectDescriptor
	if relation.ID == IDDomains {
		var err error
		types, err = informationTypeIndex(ctx, snapshot, options)
		if err != nil {
			return err
		}
	}
	if relation.ID == IDDomains {
		for _, registered := range options.Registry.InformationDomains() {
			domain := registered.Definition
			fields := []fieldValue{
				fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue("information_schema")), fv("domain_name", stringValue(domain.Name)),
				fv("data_type", stringValue(informationDataType(domain.Base))), fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue("pg_catalog")),
				fv("udt_name", stringValue(typeName(domain.Base.Type.ID))), fv("dtd_identifier", stringValue("1")),
			}
			if domain.DefaultSQL != "" {
				fields = append(fields, fv("domain_default", stringValue(domain.DefaultSQL)))
			}
			appendTypeInformation(&fields, options.Database.Name, domain.Base)
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		metadata := object.spec.Type
		if metadata == nil {
			return nil
		}
		if relation.ID == IDDomains {
			if metadata.DeclaredKind() != model.TypeKindDomain {
				return nil
			}
			baseInfo := describeInformationType(options.Registry, types, metadata.BaseLayout)
			fields := []fieldValue{
				fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue(object.schema.name)), fv("domain_name", stringValue(object.spec.Name.Name)),
				fv("data_type", stringValue(baseInfo.dataType)), fv("udt_catalog", stringValue(options.Database.Name)),
				fv("udt_schema", stringValue(baseInfo.udtSchema)), fv("udt_name", stringValue(baseInfo.udtName)),
				fv("dtd_identifier", stringValue("1")),
			}
			if metadata.Default != nil {
				fields = append(fields, fv("domain_default", stringValue(metadata.Default.SQL)))
			}
			appendTypeInformation(&fields, options.Database.Name, baseInfo.layout)
			return emitCatalogRow(relation, emit, fields...)
		}
		if metadata.DeclaredKind() != model.TypeKindComposite {
			return nil
		}
		composite, ok, err := objectByID(ctx, snapshot, options, metadata.Relation)
		if err != nil {
			return err
		}
		if !ok || composite.spec.CompositeRelation == nil {
			return nil
		}
		if composite.spec.CompositeRelation.RowType != object.spec.ID {
			return &model.Error{Code: "XX000", Message: "composite type relation descriptor is inconsistent"}
		}
		return emitCatalogRow(relation, emit,
			fv("user_defined_type_catalog", stringValue(options.Database.Name)), fv("user_defined_type_schema", stringValue(object.schema.name)),
			fv("user_defined_type_name", stringValue(object.spec.Name.Name)), fv("user_defined_type_category", stringValue("STRUCTURED")),
			fv("is_instantiable", stringValue("YES")))
	})
}

func informationTypeIndex(ctx context.Context, snapshot Snapshot, options SourceOptions) (map[model.TypeID]objectDescriptor, error) {
	result := make(map[model.TypeID]objectDescriptor)
	err := scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		if object.spec.Type != nil {
			result[object.spec.Type.Layout.Type.ID] = object
		}
		return nil
	})
	return result, err
}


func emitDomainUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	if relation.ID == IDDomainConstraints {
		for _, registered := range options.Registry.InformationDomains() {
			domain := registered.Definition
			if domain.CheckSQL == "" {
				continue
			}
			if err := emitCatalogRow(relation, emit,
				fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue("information_schema")), fv("constraint_name", stringValue(domain.ConstraintName)),
				fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue("information_schema")), fv("domain_name", stringValue(domain.Name)),
				fv("is_deferrable", stringValue("NO")), fv("initially_deferred", stringValue("NO"))); err != nil {
				return err
			}
		}
	}
	if relation.ID == IDDomainUdtUsage {
		for _, registered := range options.Registry.InformationDomains() {
			domain := registered.Definition
			if err := emitCatalogRow(relation, emit,
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue("pg_catalog")), fv("udt_name", stringValue(typeName(domain.Base.Type.ID))),
				fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue("information_schema")), fv("domain_name", stringValue(domain.Name))); err != nil {
				return err
			}
		}
	}
	for _, domain := range types {
		metadata := domain.spec.Type
		if metadata.DeclaredKind() != model.TypeKindDomain {
			continue
		}
		switch relation.ID {
		case IDDomainConstraints:
			for _, constraint := range metadata.Constraints {
				if constraint.Mutation {
					continue
				}
				if err := emitCatalogRow(relation, emit,
					fv("constraint_catalog", stringValue(options.Database.Name)), fv("constraint_schema", stringValue(domain.schema.name)), fv("constraint_name", stringValue(constraint.Name)),
					fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue(domain.schema.name)), fv("domain_name", stringValue(domain.spec.Name.Name)),
					fv("is_deferrable", stringValue(yesNo(constraint.Deferrable))), fv("initially_deferred", stringValue(yesNo(constraint.InitiallyDeferred)))); err != nil {
					return err
				}
			}
		case IDDomainUdtUsage:
			baseSchema, baseName := "pg_catalog", typeName(metadata.BaseLayout.Type.ID)
			if base, ok := types[metadata.BaseLayout.Type.ID]; ok {
				baseSchema, baseName = base.schema.name, base.spec.Name.Name
			}
			if err := emitCatalogRow(relation, emit,
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(baseSchema)), fv("udt_name", stringValue(baseName)),
				fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue(domain.schema.name)), fv("domain_name", stringValue(domain.spec.Name.Name))); err != nil {
				return err
			}
		}
	}
	return nil
}

func emitColumnTypeUsage(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	if relation.ID == IDColumnDomainUsage {
		for _, logical := range options.Registry.All() {
			for _, column := range logical.Columns {
				domain, ok := options.Registry.InformationDomainByType(column.Layout.Type.ID)
				if !ok {
					continue
				}
				if err := emitCatalogRow(relation, emit,
					fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue("information_schema")), fv("domain_name", stringValue(domain.Definition.Name)),
					fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(logical.Namespace)), fv("table_name", stringValue(logical.Name)), fv("column_name", stringValue(column.Name))); err != nil {
					return err
				}
			}
		}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable, model.ObjectView}, func(object objectDescriptor) error {
		for _, column := range descriptorColumns(object.spec) {
			if column.State != model.StatePublic || column.SystemIdentity {
				continue
			}
			domain, custom := types[column.Layout.Type.ID]
			builtinDomain, builtinCustom := options.Registry.InformationDomainByType(column.Layout.Type.ID)
			if relation.ID == IDColumnDomainUsage {
				if !builtinCustom && (!custom || domain.spec.Type.Kind != model.TypeKindDomain) {
					continue
				}
				schema, name := "information_schema", builtinDomain.Definition.Name
				if custom {
					schema, name = domain.schema.name, domain.spec.Name.Name
				}
				if err := emitCatalogRow(relation, emit,
					fv("domain_catalog", stringValue(options.Database.Name)), fv("domain_schema", stringValue(schema)), fv("domain_name", stringValue(name)),
					fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)), fv("column_name", stringValue(column.Name))); err != nil {
					return err
				}
				continue
			}
			if !custom || domain.spec.Type.Kind == model.TypeKindDomain {
				continue
			}
			if err := emitCatalogRow(relation, emit,
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(domain.schema.name)), fv("udt_name", stringValue(domain.spec.Name.Name)),
				fv("table_catalog", stringValue(options.Database.Name)), fv("table_schema", stringValue(object.schema.name)), fv("table_name", stringValue(object.spec.Name.Name)), fv("column_name", stringValue(column.Name))); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitInformationAttributes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectCompositeRelation}, func(object objectDescriptor) error {
		if object.spec.CompositeRelation == nil {
			return &model.Error{Code: "XX000", Message: "composite relation metadata is unavailable"}
		}
		rowType, ok, err := objectByID(ctx, snapshot, options, object.spec.CompositeRelation.RowType)
		if err != nil {
			return err
		}
		if !ok || rowType.spec.Type == nil || rowType.spec.Type.DeclaredKind() != model.TypeKindComposite || rowType.spec.Type.Relation != object.spec.ID {
			return &model.Error{Code: "XX000", Message: "composite relation row type descriptor is inconsistent"}
		}
		for _, column := range object.spec.CompositeRelation.Columns {
			if column.State != model.StatePublic || column.SystemIdentity {
				continue
			}
			typeInfo := describeInformationType(options.Registry, types, column.Layout)
			fields := []fieldValue{
				fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(rowType.schema.name)), fv("udt_name", stringValue(rowType.spec.Name.Name)),
				fv("attribute_name", stringValue(column.Name)), fv("ordinal_position", intValue(int64(column.ID))), fv("is_nullable", stringValue(yesNo(column.Nullable && !typeInfo.domainNotNull))),
				fv("data_type", stringValue(typeInfo.dataType)), fv("attribute_udt_catalog", stringValue(options.Database.Name)), fv("attribute_udt_schema", stringValue(typeInfo.udtSchema)), fv("attribute_udt_name", stringValue(typeInfo.udtName)),
				fv("dtd_identifier", stringValue(strconv.FormatInt(int64(column.ID), 10))), fv("is_derived_reference_attribute", stringValue("NO")),
			}
			appendTypeInformation(&fields, options.Database.Name, typeInfo.layout)
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitElementTypes(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	types, err := informationTypeIndex(ctx, snapshot, options)
	if err != nil {
		return err
	}
	emitLayout := func(schema, name, objectType, identifier string, layout model.TypeLayout, collation model.CollationID) error {
		if layout.Type.Storage != model.TypeArray || len(layout.Children) != 1 {
			return nil
		}
		element := layout.Children[0]
		typeInfo := describeInformationType(options.Registry, types, element)
		fields := []fieldValue{
			fv("object_catalog", stringValue(options.Database.Name)), fv("object_schema", stringValue(schema)), fv("object_name", stringValue(name)),
			fv("object_type", stringValue(objectType)), fv("collection_type_identifier", stringValue(identifier)), fv("data_type", stringValue(typeInfo.dataType)),
			fv("udt_catalog", stringValue(options.Database.Name)), fv("udt_schema", stringValue(typeInfo.udtSchema)), fv("udt_name", stringValue(typeInfo.udtName)),
			fv("dtd_identifier", stringValue("a"+identifier)),
		}
		if collation != 0 && collation != 100 {
			if info, ok := builtin.LookupCollation(collation); ok {
				fields = append(fields,
					fv("collation_catalog", stringValue(options.Database.Name)),
					fv("collation_schema", stringValue("pg_catalog")),
					fv("collation_name", stringValue(info.Name)),
				)
			}
		}
		return emitCatalogRow(relation, emit, fields...)
	}
	for _, logical := range options.Registry.All() {
		for _, column := range logical.Columns {
			if err := emitLayout(logical.Namespace, logical.Name, "TABLE", strconv.FormatUint(uint64(column.ID), 10), column.Layout, column.Layout.Type.Collation); err != nil {
				return err
			}
		}
	}
	emitRoutine := func(signature builtin.Signature, schema string, operation builtin.InformationSchemaFunction) error {
		name := informationSpecificName(signature.Name, model.OID(signature.ID))
		if signature.Kind != 'p' {
			layout := catalogLayout(signature.Result, -1)
			if err := emitLayout(schema, name, "ROUTINE", "0", layout, layout.Type.Collation); err != nil {
				return err
			}
		}
		arguments := informationRoutineArguments(signature, operation)
		for i := range arguments.count {
			layout := catalogLayout(arguments.typeIDs[i], -1)
			if err := emitLayout(schema, name, "ROUTINE", strconv.Itoa(i+1), layout, layout.Type.Collation); err != nil {
				return err
			}
		}
		return nil
	}
	for signature := range builtin.Signatures() {
		if err := emitRoutine(signature, "pg_catalog", 0); err != nil {
			return err
		}
	}
	for _, function := range options.Registry.InformationSchemaFunctions() {
		if err := emitRoutine(function.Signature, "information_schema", function.Operation); err != nil {
			return err
		}
	}
	if err := scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable, model.ObjectView}, func(object objectDescriptor) error {
		for _, column := range descriptorColumns(object.spec) {
			if column.State == model.StatePublic && !column.SystemIdentity {
				if err := emitLayout(object.schema.name, object.spec.Name.Name, "TABLE", strconv.FormatUint(uint64(column.ID), 10), column.Layout, column.Layout.Type.Collation); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectRoutine}, func(object objectDescriptor) error {
		name := specificName(object)
		if aggregate := object.spec.Aggregate; aggregate != nil {
			if err := emitLayout(object.schema.name, name, "ROUTINE", "0", aggregate.Result, 0); err != nil {
				return err
			}
			for i, argument := range aggregate.Arguments {
				if err := emitLayout(object.schema.name, name, "ROUTINE", strconv.Itoa(i+1), argument, 0); err != nil {
					return err
				}
			}
			return nil
		}
		if object.spec.Routine == nil {
			return nil
		}
		if err := emitLayout(object.schema.name, name, "ROUTINE", "0", object.spec.Routine.Result, 0); err != nil {
			return err
		}
		for i, parameter := range object.spec.Routine.Parameters {
			if err := emitLayout(object.schema.name, name, "ROUTINE", strconv.Itoa(i+1), parameter.Layout, 0); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectType}, func(object objectDescriptor) error {
		if object.spec.Type == nil || object.spec.Type.DeclaredKind() != model.TypeKindDomain {
			return nil
		}
		return emitLayout(object.schema.name, object.spec.Name.Name, "DOMAIN", "1", object.spec.Type.BaseLayout, object.spec.Type.Layout.Type.Collation)
	}); err != nil {
		return err
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectCompositeRelation}, func(object objectDescriptor) error {
		if object.spec.CompositeRelation == nil {
			return &model.Error{Code: "XX000", Message: "composite relation metadata is unavailable"}
		}
		rowType, ok, err := objectByID(ctx, snapshot, options, object.spec.CompositeRelation.RowType)
		if err != nil {
			return err
		}
		if !ok || rowType.spec.Type == nil || rowType.spec.Type.Relation != object.spec.ID {
			return &model.Error{Code: "XX000", Message: "composite relation row type descriptor is inconsistent"}
		}
		for _, field := range object.spec.CompositeRelation.Columns {
			if field.State != model.StatePublic || field.SystemIdentity {
				continue
			}
			if err := emitLayout(rowType.schema.name, rowType.spec.Name.Name, "USER-DEFINED TYPE", strconv.FormatUint(uint64(field.ID), 10), field.Layout, field.Layout.Type.Collation); err != nil {
				return err
			}
		}
		return nil
	})
}
