package catalogrel

import (
	"bytes"
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func intList(values []model.ColumnID) model.Value {
	items := make([]model.Value, len(values))
	for i, value := range values {
		items[i] = intValue(int64(value))
	}
	return model.Value{Type: model.TypeArray, ArrayVal: items, Dimensions: []model.ArrayDimension{{Length: int32(len(items)), LowerBound: 1}}}
}

func columnVector(values []model.ColumnID) model.Value {
	items := make([]int64, len(values))
	for i, value := range values {
		items[i] = int64(value)
	}
	return intArray(items)
}

func operatorList(values []model.OperatorID) model.Value {
	items := make([]int64, len(values))
	for i, value := range values {
		items[i] = int64(value)
	}
	return oidArray(items)
}

func emitConstraints(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter, informationSchema bool) error {
	informationNamespace, err := informationSchemaIdentity(options)
	if err != nil {
		return err
	}
	if !informationSchema {
		for _, registered := range options.Registry.InformationDomains() {
			domain := registered.Definition
			if registered.Identity.ConstraintOID == 0 {
				continue
			}
			if err := emitCatalogRow(relation, emit,
				fv("oid", oidValue(registered.Identity.ConstraintOID)), fv("conname", stringValue(domain.ConstraintName)), fv("connamespace", oidValue(informationNamespace.OID)),
				fv("contype", charValue('c')), fv("condeferrable", boolValue(false)), fv("condeferred", boolValue(false)),
				fv("conenforced", boolValue(true)), fv("convalidated", boolValue(true)), fv("conrelid", oidValue(0)),
				fv("contypid", oidValue(registered.Identity.TypeOID)), fv("conindid", oidValue(0)), fv("conparentid", oidValue(0)), fv("confrelid", oidValue(0)),
				fv("confupdtype", charValue(' ')), fv("confdeltype", charValue(' ')), fv("confmatchtype", charValue(' ')),
				fv("conislocal", boolValue(true)), fv("coninhcount", intValue(0)), fv("connoinherit", boolValue(false)), fv("conperiod", boolValue(false)),
				fv("conbin", stringValue(domain.CheckSQL))); err != nil {
				return err
			}
		}
	}
	if informationSchema && relation.ID == IDCheckConstraints {
		for _, registered := range options.Registry.InformationDomains() {
			domain := registered.Definition
			if registered.Identity.ConstraintOID == 0 || domain.CheckSQL == "" {
				continue
			}
			if err := emitCatalogRow(relation, emit,
				fv("constraint_catalog", stringValue(options.Database.Name)),
				fv("constraint_schema", stringValue("information_schema")),
				fv("constraint_name", stringValue(domain.ConstraintName)),
				fv("check_clause", stringValue(domain.CheckSQL))); err != nil {
				return err
			}
		}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable, model.ObjectType}, func(object objectDescriptor) error {
		var constraints []model.ConstraintDescriptor
		constraintRelation, constraintTypeOID := object.spec.OID, model.OID(0)
		if object.spec.Table != nil {
			constraints = object.spec.Table.Constraints
		} else if object.spec.Type != nil && object.spec.Type.Kind == model.TypeKindDomain && (!informationSchema || relation.ID == IDCheckConstraints) {
			constraints = object.spec.Type.Constraints
			constraintRelation, constraintTypeOID = 0, object.spec.OID
		} else {
			return nil
		}
		for _, constraint := range constraints {
			if constraint.Mutation {
				continue
			}
			if informationSchema {
				if err := emitInformationConstraint(ctx, snapshot, relation, emit, options, object, constraint); err != nil {
					return err
				}
				continue
			}
			if constraint.OID == 0 {
				return &model.Error{Code: "XX000", Message: "constraint has no SQL catalog identity", Detail: object.schema.name + "." + object.spec.Name.Name + "." + constraint.Name}
			}
			indexOID, referencedOID := model.OID(0), model.OID(0)
			indexID := constraint.Index
			if constraint.ReferencedIndex != 0 {
				indexID = constraint.ReferencedIndex
			}
			if indexID != 0 {
				published, err := snapshot.Descriptor(ctx, indexID)
				if err != nil {
					return err
				}
				if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectIndex || published.Descriptor.OID() == 0 {
					return &model.Error{Code: "XX000", Message: "constraint index descriptor is unavailable"}
				}
				indexOID = published.Descriptor.OID()
			}
			if constraint.ReferencedObject != 0 {
				published, err := snapshot.Descriptor(ctx, constraint.ReferencedObject)
				if err != nil {
					return err
				}
				if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectTable || published.Descriptor.OID() == 0 {
					return &model.Error{Code: "XX000", Message: "referenced constraint descriptor is unavailable"}
				}
				referencedOID = published.Descriptor.OID()
			}
			parentOID, err := constraintParentCatalogOID(ctx, snapshot, options, constraint)
			if err != nil {
				return err
			}
			updateAction, deleteAction, matchType := byte(' '), byte(' '), byte(' ')
			if constraint.Kind == model.ConstraintForeignKey {
				updateAction = actionCode(constraint.OnUpdate)
				deleteAction = actionCode(constraint.OnDelete)
				matchType = matchCode(constraint.Match)
			}
			fields := []fieldValue{
				fv("oid", oidValue(constraint.OID)), fv("conname", stringValue(constraint.Name)), fv("connamespace", oidValue(object.schema.oid)),
				fv("contype", charValue(constraintType(constraint.Kind))), fv("condeferrable", boolValue(constraint.Deferrable)), fv("condeferred", boolValue(constraint.InitiallyDeferred)),
				fv("conenforced", boolValue(constraint.Enforced)), fv("convalidated", boolValue(constraint.Validated)), fv("conrelid", oidValue(constraintRelation)),
				fv("contypid", oidValue(constraintTypeOID)), fv("conindid", oidValue(indexOID)), fv("conparentid", oidValue(parentOID)), fv("confrelid", oidValue(referencedOID)),
				fv("confupdtype", charValue(updateAction)), fv("confdeltype", charValue(deleteAction)), fv("confmatchtype", charValue(matchType)),
				fv("conislocal", boolValue(constraint.Local || constraint.ParentConstraint == 0)), fv("coninhcount", intValue(int64(constraint.InheritanceCount))), fv("connoinherit", boolValue(constraint.NoInherit)), fv("conperiod", boolValue(false)),
			}
			if len(constraint.Columns) != 0 {
				fields = append(fields, fv("conkey", intList(constraint.Columns)))
			}
			if len(constraint.ReferencedColumns) != 0 {
				fields = append(fields, fv("confkey", intList(constraint.ReferencedColumns)))
			}
			if len(constraint.OnDeleteColumns) != 0 {
				fields = append(fields, fv("confdelsetcols", intList(constraint.OnDeleteColumns)))
			}
			if len(constraint.ParentForeignEqualityOperators) != 0 {
				fields = append(fields, fv("conpfeqop", operatorList(constraint.ParentForeignEqualityOperators)))
			}
			if len(constraint.ParentEqualityOperators) != 0 {
				fields = append(fields, fv("conppeqop", operatorList(constraint.ParentEqualityOperators)))
			}
			if len(constraint.ForeignEqualityOperators) != 0 {
				fields = append(fields, fv("conffeqop", operatorList(constraint.ForeignEqualityOperators)))
			}
			if len(constraint.ExcludeOperators) != 0 {
				fields = append(fields, fv("conexclop", operatorList(constraint.ExcludeOperators)))
			}
			if constraint.Expression != nil {
				fields = append(fields, fv("conbin", stringValue(constraint.Expression.SQL)))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func constraintParentCatalogOID(ctx context.Context, snapshot Snapshot, options SourceOptions, constraint model.ConstraintDescriptor) (model.OID, error) {
	if constraint.ParentConstraint == 0 {
		if constraint.ParentTable != 0 || constraint.InheritanceCount != 0 {
			return 0, &model.Error{Code: "XX000", Message: "constraint inheritance metadata is inconsistent"}
		}
		return 0, nil
	}
	if constraint.ParentTable == 0 || constraint.InheritanceCount == 0 || constraint.NoInherit {
		return 0, &model.Error{Code: "XX000", Message: "constraint inheritance metadata is inconsistent"}
	}
	published, err := snapshot.Descriptor(ctx, constraint.ParentTable)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectTable {
		return 0, &model.Error{Code: "XX000", Message: "parent constraint table descriptor is unavailable"}
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return 0, err
	}
	if spec.Table == nil {
		return 0, &model.Error{Code: "XX000", Message: "parent constraint table metadata is unavailable"}
	}
	for _, parent := range spec.Table.Constraints {
		if parent.ID == constraint.ParentConstraint {
			if parent.OID == 0 || parent.Kind != constraint.Kind {
				return 0, &model.Error{Code: "XX000", Message: "parent constraint metadata is inconsistent"}
			}
			return parent.OID, nil
		}
	}
	return 0, &model.Error{Code: "XX000", Message: "parent constraint metadata is unavailable"}
}

func constraintType(kind model.ConstraintKind) byte {
	switch kind {
	case model.ConstraintCheck:
		return 'c'
	case model.ConstraintForeignKey:
		return 'f'
	case model.ConstraintPrimaryKey:
		return 'p'
	case model.ConstraintUnique:
		return 'u'
	case model.ConstraintExclude:
		return 'x'
	case model.ConstraintNotNull:
		return 'n'
	case model.ConstraintTrigger:
		return 't'
	default:
		return 0
	}
}

func actionCode(action string) byte {
	switch strings.ToLower(action) {
	case "restrict":
		return 'r'
	case "cascade":
		return 'c'
	case "set null":
		return 'n'
	case "set default":
		return 'd'
	default:
		return 'a'
	}
}

func matchCode(match string) byte {
	switch strings.ToLower(match) {
	case "full":
		return 'f'
	case "partial":
		return 'p'
	default:
		return 's'
	}
}

func emitTriggers(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter, informationSchema bool) error {
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectView}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		var triggers []model.TriggerDescriptor
		if object.spec.Table != nil {
			triggers = object.spec.Table.Triggers
		} else if object.spec.View != nil {
			triggers = object.spec.View.Triggers
		}
		if informationSchema {
			visible := triggers[:0]
			for _, trigger := range triggers {
				_, _, _, include, err := triggerConstraintCatalogOIDs(ctx, snapshot, options, trigger)
				if err != nil {
					return err
				}
				if include {
					visible = append(visible, trigger)
				}
			}
			for _, trigger := range visible {
				if err := emitInformationTrigger(ctx, snapshot, relation, emit, options, object, visible, trigger); err != nil {
					return err
				}
			}
			return nil
		}
		for _, trigger := range triggers {
			constraintOID, constraintRelationOID, constraintIndexOID, visible, err := triggerConstraintCatalogOIDs(ctx, snapshot, options, trigger)
			if err != nil {
				return err
			}
			if !visible {
				continue
			}
			if trigger.OID == 0 {
				return &model.Error{Code: "XX000", Message: "trigger has no SQL catalog identity", Detail: object.schema.name + "." + object.spec.Name.Name + "." + trigger.Name}
			}
			routineOID := model.OID(trigger.Function)
			if trigger.Routine != 0 {
				published, err := snapshot.Descriptor(ctx, trigger.Routine)
				if err != nil {
					return err
				}
				if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectRoutine || published.Descriptor.OID() == 0 {
					return &model.Error{Code: "XX000", Message: "trigger routine descriptor is unavailable"}
				}
				routineOID = published.Descriptor.OID()
			}
			fields := []fieldValue{
				fv("oid", oidValue(trigger.OID)), fv("tgrelid", oidValue(object.spec.OID)), fv("tgparentid", oidValue(0)), fv("tgname", stringValue(trigger.Name)),
				fv("tgfoid", oidValue(routineOID)), fv("tgtype", intValue(int64(triggerType(trigger)))), fv("tgenabled", charValue(triggerEnabled(trigger.EnableMode))),
				fv("tgisinternal", boolValue(trigger.Internal)), fv("tgconstrrelid", oidValue(constraintRelationOID)), fv("tgconstrindid", oidValue(constraintIndexOID)), fv("tgconstraint", oidValue(constraintOID)),
				fv("tgdeferrable", boolValue(trigger.Deferrable)), fv("tginitdeferred", boolValue(trigger.InitiallyDeferred)), fv("tgnargs", intValue(int64(len(trigger.Arguments)))),
				fv("tgattr", columnVector(trigger.Columns)), fv("tgargs", model.BytesVal(triggerArguments(trigger.Arguments))),
			}
			if trigger.When != nil {
				fields = append(fields, fv("tgqual", stringValue(trigger.When.SQL)))
			}
			if trigger.OldTransitionTable != "" {
				fields = append(fields, fv("tgoldtable", stringValue(trigger.OldTransitionTable)))
			}
			if trigger.NewTransitionTable != "" {
				fields = append(fields, fv("tgnewtable", stringValue(trigger.NewTransitionTable)))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func triggerConstraintCatalogOIDs(ctx context.Context, snapshot Snapshot, options SourceOptions, trigger model.TriggerDescriptor) (model.OID, model.OID, model.OID, bool, error) {
	hasConstraintMetadata := trigger.ConstraintTable != 0 || trigger.ConstraintID != 0 || trigger.ConstraintRelation != 0 || trigger.ConstraintIndex != 0
	if !trigger.Constraint {
		if hasConstraintMetadata {
			return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "non-constraint trigger carries constraint metadata"}
		}
		return 0, 0, 0, true, nil
	}
	if trigger.ConstraintTable == 0 || trigger.ConstraintID == 0 {
		return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "constraint trigger association is unavailable"}
	}
	published, err := snapshot.Descriptor(ctx, trigger.ConstraintTable)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "constraint trigger table descriptor is unavailable"}
	}
	spec, err := published.Descriptor.SpecWithin(ctx, descriptorLimits(options))
	if err != nil {
		return 0, 0, 0, false, err
	}
	if spec.Table == nil {
		return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "constraint trigger table metadata is unavailable"}
	}
	constraintOID := model.OID(0)
	for _, constraint := range spec.Table.Constraints {
		if constraint.ID != trigger.ConstraintID {
			continue
		}
		if constraint.Mutation {
			return 0, 0, 0, false, nil
		}
		if constraint.Kind != model.ConstraintTrigger || constraint.OID == 0 {
			return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "constraint trigger association is inconsistent"}
		}
		constraintOID = constraint.OID
		break
	}
	if constraintOID == 0 {
		return 0, 0, 0, false, &model.Error{Code: "XX000", Message: "constraint trigger association is unavailable"}
	}
	constraintRelationOID, err := triggerReferencedRelationOID(ctx, snapshot, trigger.ConstraintRelation)
	if err != nil {
		return 0, 0, 0, false, err
	}
	constraintIndexOID, err := triggerReferencedIndexOID(ctx, snapshot, trigger.ConstraintIndex)
	if err != nil {
		return 0, 0, 0, false, err
	}
	return constraintOID, constraintRelationOID, constraintIndexOID, true, nil
}

func triggerReferencedRelationOID(ctx context.Context, snapshot Snapshot, id model.ObjectID) (model.OID, error) {
	if id == 0 {
		return 0, nil
	}
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return 0, &model.Error{Code: "XX000", Message: "constraint trigger relation descriptor is unavailable"}
	}
	switch published.Descriptor.Kind() {
	case model.ObjectTable, model.ObjectView, model.ObjectMaterializedView:
		return published.Descriptor.OID(), nil
	default:
		return 0, &model.Error{Code: "XX000", Message: "constraint trigger relation descriptor has invalid kind"}
	}
}

func triggerReferencedIndexOID(ctx context.Context, snapshot Snapshot, id model.ObjectID) (model.OID, error) {
	if id == 0 {
		return 0, nil
	}
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectIndex {
		return 0, &model.Error{Code: "XX000", Message: "constraint trigger index descriptor is unavailable"}
	}
	return published.Descriptor.OID(), nil
}

func triggerType(trigger model.TriggerDescriptor) int16 {
	value := int16(0)
	if trigger.ForEachRow {
		value |= 1
	}
	switch strings.ToLower(trigger.Timing) {
	case "before":
		value |= 2
	case "instead":
		value |= 64
	}
	for _, event := range trigger.Events {
		switch strings.ToLower(event) {
		case "insert":
			value |= 4
		case "delete":
			value |= 8
		case "update":
			value |= 16
		case "truncate":
			value |= 32
		}
	}
	return value
}

func triggerEnabled(mode model.TriggerEnableMode) byte {
	switch mode {
	case model.TriggerEnabledOrigin:
		return 'O'
	case model.TriggerEnabledReplica:
		return 'R'
	case model.TriggerEnabledAlways:
		return 'A'
	default:
		return 'D'
	}
}

func triggerArguments(arguments []string) []byte {
	var buffer bytes.Buffer
	for _, argument := range arguments {
		buffer.WriteString(argument)
		buffer.WriteByte(0)
	}
	return buffer.Bytes()
}

func emitInherits(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		if object.spec.Table == nil {
			return nil
		}
		detachPending := object.spec.Table.PartitionBound != nil && object.spec.Table.PartitionState == model.PartitionDetachPending
		for i, parent := range object.spec.Table.Parents {
			published, err := snapshot.Descriptor(ctx, parent)
			if err != nil {
				return err
			}
			if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectTable || published.Descriptor.OID() == 0 {
				return &model.Error{Code: "XX000", Message: "inheritance parent descriptor is unavailable"}
			}
			if err := emitCatalogRow(relation, emit, fv("inhrelid", oidValue(object.spec.OID)), fv("inhparent", oidValue(published.Descriptor.OID())), fv("inhseqno", intValue(int64(i+1))), fv("inhdetachpending", boolValue(detachPending))); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitPartitions(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		table := object.spec.Table
		if table == nil || table.PartitionStrategy == 0 {
			return nil
		}
		attributes := make([]int64, len(table.PartitionKeys))
		classes := make([]int64, len(table.PartitionKeys))
		collations := make([]int64, len(table.PartitionKeys))
		var expressions []string
		for i, key := range table.PartitionKeys {
			attributes[i] = int64(key.Column)
			classes[i] = int64(key.OperatorClass)
			collations[i] = int64(key.Collation)
			if key.Expression != nil {
				attributes[i] = 0
				expressions = append(expressions, key.Expression.SQL)
			}
		}
		defaultOID := model.OID(0)
		if table.PartitionDefault != 0 {
			published, err := snapshot.Descriptor(ctx, table.PartitionDefault)
			if err != nil {
				return err
			}
			if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectTable || published.Descriptor.OID() == 0 {
				return &model.Error{Code: "XX000", Message: "partition default descriptor is not a public table"}
			}
			defaultOID = published.Descriptor.OID()
		}
		fields := []fieldValue{
			fv("partrelid", oidValue(object.spec.OID)), fv("partstrat", charValue(table.PartitionStrategy)), fv("partnatts", intValue(int64(len(table.PartitionKeys)))),
			fv("partdefid", oidValue(defaultOID)), fv("partattrs", intArray(attributes)), fv("partclass", intArray(classes)), fv("partcollation", intArray(collations)),
		}
		if len(expressions) != 0 {
			fields = append(fields, fv("partexprs", stringValue(strings.Join(expressions, ", "))))
		}
		return emitCatalogRow(relation, emit, fields...)
	})
}

func emitSequences(ctx context.Context, snapshot Snapshot, view ReadView, relation Relation, options SourceOptions, emit rowEmitter) error {
	if relation.ID == IDPgSequences && options.Sequences == nil {
		return &model.Error{Code: "55000", Message: "sequence catalog state is unavailable"}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectSequence}, func(object objectDescriptor) error {
		sequence := object.spec.Sequence
		if sequence == nil {
			return nil
		}
		if relation.ID == IDPgSequence {
			return emitCatalogRow(relation, emit,
				fv("seqrelid", oidValue(object.spec.OID)), fv("seqtypid", intValue(int64(sequence.Type.ID))), fv("seqstart", intValue(sequence.Start)),
				fv("seqincrement", intValue(sequence.Increment)), fv("seqmax", intValue(sequence.Max)), fv("seqmin", intValue(sequence.Min)), fv("seqcache", intValue(sequence.Cache)), fv("seqcycle", boolValue(sequence.Cycle)))
		}
		owner, err := principalName(sequence.Owner)
		if err != nil {
			return err
		}
		state, err := options.Sequences.Get(ctx, view, schema.SequenceIncarnation(sequence.Incarnation))
		if err != nil {
			return err
		}
		fields := []fieldValue{
			fv("schemaname", stringValue(object.schema.name)), fv("sequencename", stringValue(object.spec.Name.Name)), fv("sequenceowner", stringValue(owner)),
			fv("data_type", intValue(int64(sequence.Type.ID))), fv("start_value", intValue(sequence.Start)), fv("min_value", intValue(sequence.Min)), fv("max_value", intValue(sequence.Max)),
			fv("increment_by", intValue(sequence.Increment)), fv("cycle", boolValue(sequence.Cycle)), fv("cache_size", intValue(sequence.Cache)),
		}
		if state.Called {
			fields = append(fields, fv("last_value", intValue(state.Last)))
		}
		return emitCatalogRow(relation, emit, fields...)
	})
}

func emitLogicalPGTables(relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, logical := range options.Registry.All() {
		if logicalRelationDeclaration(logical) != DeclarationTable {
			continue
		}
		if err := emitCatalogRow(relation, emit,
			fv("schemaname", stringValue(logical.Namespace)),
			fv("tablename", stringValue(logical.Name)),
			fv("tableowner", stringValue(options.Database.RoleName)),
			fv("hasindexes", boolValue(false)),
			fv("hasrules", boolValue(false)),
			fv("hastriggers", boolValue(false)),
			fv("rowsecurity", boolValue(false))); err != nil {
			return err
		}
	}
	return nil
}
func emitLogicalPGViews(relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, logical := range options.Registry.All() {
		if logicalRelationDeclaration(logical) != DeclarationView {
			continue
		}
		if err := emitCatalogRow(relation, emit,
			fv("schemaname", stringValue(logical.Namespace)),
			fv("viewname", stringValue(logical.Name)),
			fv("viewowner", stringValue(options.Database.RoleName)),
			fv("definition", stringValue(logical.ViewDefinition.SQL))); err != nil {
			return err
		}
	}
	return nil
}


func emitPGViews(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	var kinds []model.ObjectKind
	switch relation.ID {
	case IDPgTables:
		if err := emitLogicalPGTables(relation, options, emit); err != nil {
			return err
		}
		kinds = []model.ObjectKind{model.ObjectTable}
	case IDPgViews:
		if err := emitLogicalPGViews(relation, options, emit); err != nil {
			return err
		}
		kinds = []model.ObjectKind{model.ObjectView}
	case IDPgMatviews:
		kinds = []model.ObjectKind{model.ObjectMaterializedView}
	}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		if object.spec.Table != nil && object.spec.View != nil && object.spec.Table.Owner != object.spec.View.Owner {
			return &model.Error{Code: "XX000", Message: "materialized view owners do not match"}
		}
		var ownerOID model.OID
		switch {
		case object.spec.Table != nil:
			ownerOID = object.spec.Table.Owner
		case object.spec.View != nil:
			ownerOID = object.spec.View.Owner
		default:
			return &model.Error{Code: "XX000", Message: "relation metadata is unavailable"}
		}
		owner, err := principalName(ownerOID)
		if err != nil {
			return err
		}
		switch relation.ID {
		case IDPgTables:
			hasIndexes, hasTriggers := false, false
			if object.spec.Table != nil {
				hasIndexes = len(object.spec.Table.Indexes) != 0
				hasTriggers = len(object.spec.Table.Triggers) != 0
			}
			return emitCatalogRow(relation, emit, fv("schemaname", stringValue(object.schema.name)), fv("tablename", stringValue(object.spec.Name.Name)), fv("tableowner", stringValue(owner)), fv("hasindexes", boolValue(hasIndexes)), fv("hasrules", boolValue(false)), fv("hastriggers", boolValue(hasTriggers)), fv("rowsecurity", boolValue(false)))
		case IDPgViews:
			return emitCatalogRow(relation, emit, fv("schemaname", stringValue(object.schema.name)), fv("viewname", stringValue(object.spec.Name.Name)), fv("viewowner", stringValue(owner)), fv("definition", stringValue(object.spec.View.Definition)))
		default:
			if object.spec.Table == nil || object.spec.View == nil {
				return &model.Error{Code: "XX000", Message: "materialized view metadata is unavailable"}
			}
			return emitCatalogRow(relation, emit, fv("schemaname", stringValue(object.schema.name)), fv("matviewname", stringValue(object.spec.Name.Name)), fv("matviewowner", stringValue(owner)), fv("hasindexes", boolValue(len(object.spec.Table.Indexes) != 0)), fv("ispopulated", boolValue(object.spec.View.Populated)), fv("definition", stringValue(object.spec.View.Definition)))
		}
	})
}

func catalogIndexDefinition(index objectDescriptor, table model.DescriptorSpec, metadata *model.IndexMetadata) (string, error) {
	if table.Table == nil || metadata == nil || len(metadata.Keys) == 0 {
		return "", &model.Error{Code: "XX000", Message: "index definition metadata is unavailable"}
	}
	method := ""
	keys := make([]string, len(metadata.Keys))
	for i, key := range metadata.Keys {
		operatorClass, ok := builtin.LookupOperatorClass(key.OperatorClass)
		if !ok {
			return "", &model.Error{Code: "55000", Message: "index operator class metadata is unavailable"}
		}
		if method == "" {
			method = operatorClass.Method
		} else if method != operatorClass.Method {
			return "", &model.Error{Code: "XX000", Message: "index keys use incompatible access methods"}
		}
		text := ""
		if key.Expression != nil {
			text = "(" + key.Expression.SQL + ")"
		} else {
			text = catalogColumnIdentifier(table.Table.Columns, key.Column)
			if text == "" {
				return "", &model.Error{Code: "XX000", Message: "index key column is unavailable"}
			}
		}
		if key.Collation != 0 {
			collation, ok := builtin.LookupCollation(key.Collation)
			if !ok {
				return "", &model.Error{Code: "55000", Message: "index collation metadata is unavailable"}
			}
			text += " COLLATE pg_catalog." + quoteIdentifier(collation.Name)
		}
		if !operatorClass.Default {
			text += " pg_catalog." + quoteIdentifier(operatorClass.Name)
		}
		if key.Descending {
			text += " DESC"
		}
		if key.NullsFirst {
			text += " NULLS FIRST"
		} else {
			text += " NULLS LAST"
		}
		keys[i] = text
	}
	unique := ""
	if metadata.Unique {
		unique = "UNIQUE "
	}
	definition := "CREATE " + unique + "INDEX " + quoteIdentifier(index.spec.Name.Name) + " ON " + quoteIdentifier(index.schema.name) + "." + quoteIdentifier(table.Name.Name) + " USING " + quoteIdentifier(method) + " (" + strings.Join(keys, ", ") + ")"
	if len(metadata.Include) != 0 {
		include := make([]string, len(metadata.Include))
		for i, id := range metadata.Include {
			include[i] = catalogColumnIdentifier(table.Table.Columns, id)
			if include[i] == "" {
				return "", &model.Error{Code: "XX000", Message: "included index column is unavailable"}
			}
		}
		definition += " INCLUDE (" + strings.Join(include, ", ") + ")"
	}
	if metadata.NullsNotDistinct {
		definition += " NULLS NOT DISTINCT"
	}
	if metadata.Predicate != nil {
		definition += " WHERE " + metadata.Predicate.SQL
	}
	return definition, nil
}

func catalogColumnIdentifier(columns []model.ColumnDescriptor, id model.ColumnID) string {
	for _, column := range columns {
		if column.ID == id && column.State == model.StatePublic {
			return quoteIdentifier(column.Name)
		}
	}
	return ""
}
