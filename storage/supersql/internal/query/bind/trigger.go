package bind

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func BindCreateTrigger(ctx context.Context, statement *zpgparse.CreateTrigStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil || statement.Relation == nil {
		return nil, bindError("42601", "CREATE TRIGGER requires a relation", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateTrigStmt{CreateTrigStmt: statement}})
	if err != nil {
		return nil, err
	}
	if statement.Trigname == "" || statement.Relation.Catalogname != "" || statement.Relation.Alias != nil {
		return nil, bindError("42601", "invalid CREATE TRIGGER declaration", int(statement.Relation.Location)+1)
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectView}, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		return nil, err
	}
	spec, err := table.Spec(ctx)
	if err != nil {
		return nil, err
	}
	var columns []model.ColumnDescriptor
	var triggers *[]model.TriggerDescriptor
	switch {
	case spec.Table != nil:
		columns, triggers = spec.Table.Columns, &spec.Table.Triggers
	case spec.View != nil:
		columns, triggers = spec.View.Columns, &spec.View.Triggers
	default:
		return nil, bindError("42809", "trigger target is not a table or view", int(statement.Relation.Location)+1)
	}
	b.dependency(table, 0)
	routine, function, err := resolveTriggerRoutine(ctx, b, snapshot, statement.Funcname)
	if err != nil {
		return nil, err
	}
	trigger := model.TriggerDescriptor{Name: statement.Trigname, Function: function, ForEachRow: statement.Row, EnableMode: model.TriggerEnabledOrigin, Constraint: statement.Isconstraint, Deferrable: statement.Deferrable, InitiallyDeferred: statement.Initdeferred}
	schema, err := snapshot.Descriptor(ctx, table.Name().Parent)
	if err != nil {
		return nil, err
	}
	if schema.Descriptor.Kind() != model.ObjectSchema {
		return nil, model.ErrInvalidDescriptor
	}
	trigger.SchemaName = schema.Descriptor.Name().Name
	b.dependency(schema.Descriptor, 0)
	if routine != nil {
		b.dependency(routine, 0)
		trigger.Routine = routine.ID()
	}
	switch statement.Timing {
	case 0:
		trigger.Timing = "after"
	case 2:
		trigger.Timing = "before"
	case 64:
		trigger.Timing = "instead"
	default:
		return nil, bindError("42601", "invalid trigger timing", 0)
	}
	for bit, name := range map[int32]string{4: "insert", 8: "delete", 16: "update", 32: "truncate"} {
		if statement.Events&bit != 0 {
			trigger.Events = append(trigger.Events, name)
		}
	}
	if statement.Events &^ int32(4|8|16|32) != 0 || len(trigger.Events) == 0 {
		return nil, bindError("42601", "invalid trigger events", 0)
	}
	columnIDs := make(map[string]model.ColumnID, len(columns))
	for _, column := range columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			columnIDs[column.Name] = column.ID
		}
	}
	for _, node := range statement.Columns {
		name := node.GetString_().GetSval()
		id := columnIDs[name]
		if id == 0 {
			return nil, bindError("42703", "trigger column does not exist", 0)
		}
		if slices.Contains(trigger.Columns, id) {
			return nil, bindError("42701", "trigger column specified more than once", 0)
		}
		trigger.Columns = append(trigger.Columns, id)
	}
	for _, node := range statement.Args {
		value := node.GetString_()
		if value == nil {
			return nil, bindError("42601", "trigger arguments must be string literals", 0)
		}
		trigger.Arguments = append(trigger.Arguments, value.Sval)
	}
	for _, node := range statement.TransitionRels {
		transition := node.GetTriggerTransition()
		if transition == nil || !transition.IsTable || transition.Name == "" {
			return nil, bindError("42601", "invalid trigger transition relation", 0)
		}
		if transition.IsNew {
			if trigger.NewTransitionTable != "" {
				return nil, bindError("42712", "NEW TABLE transition relation specified more than once", 0)
			}
			trigger.NewTransitionTable = transition.Name
		} else {
			if trigger.OldTransitionTable != "" {
				return nil, bindError("42712", "OLD TABLE transition relation specified more than once", 0)
			}
			trigger.OldTransitionTable = transition.Name
		}
	}
	if spec.View != nil {
		if trigger.ForEachRow && trigger.Timing != "instead" {
			return nil, bindError("42809", "row-level triggers on views must be INSTEAD OF", 0)
		}
		if trigger.Timing == "instead" && (!trigger.ForEachRow || trigger.Constraint || statement.WhenClause != nil || len(statement.TransitionRels) != 0 || slices.Contains(trigger.Events, "truncate")) {
			return nil, bindError("42P17", "invalid INSTEAD OF trigger declaration", 0)
		}
	} else if trigger.Timing == "instead" {
		return nil, bindError("42809", "INSTEAD OF triggers require a view", 0)
	}
	if trigger.Constraint && (trigger.Timing != "after" || !trigger.ForEachRow || slices.Contains(trigger.Events, "truncate")) {
		return nil, bindError("42P17", "constraint triggers must be AFTER ROW triggers", 0)
	}
	if !trigger.Constraint && (trigger.Deferrable || trigger.InitiallyDeferred || statement.Constrrel != nil) {
		return nil, bindError("42P17", "deferrability and constraint relations require a constraint trigger", 0)
	}
	if trigger.Constraint && len(trigger.Columns) != 0 {
		return nil, bindError("42P17", "constraint triggers cannot use UPDATE OF columns", 0)
	}
	if len(statement.TransitionRels) != 0 && (spec.View != nil || trigger.Timing != "after" || trigger.Constraint || slices.Contains(trigger.Events, "truncate")) {
		return nil, bindError("42P17", "transition relations require an AFTER table trigger", 0)
	}
	if trigger.NewTransitionTable != "" && !slices.Contains(trigger.Events, "insert") && !slices.Contains(trigger.Events, "update") {
		return nil, bindError("42P17", "NEW TABLE is only valid for INSERT or UPDATE triggers", 0)
	}
	if trigger.OldTransitionTable != "" && !slices.Contains(trigger.Events, "delete") && !slices.Contains(trigger.Events, "update") {
		return nil, bindError("42P17", "OLD TABLE is only valid for DELETE or UPDATE triggers", 0)
	}
	if len(statement.Columns) != 0 && (trigger.OldTransitionTable != "" || trigger.NewTransitionTable != "") {
		return nil, bindError("42P17", "transition tables are not allowed with UPDATE OF triggers", 0)
	}
	if statement.WhenClause != nil {
		trigger.When, err = bindTriggerWhen(ctx, b, snapshot, options.Bind, columns, statement.WhenClause, trigger.Events, trigger.ForEachRow)
		if err != nil {
			return nil, err
		}
	}
	var replaced *model.TriggerDescriptor
	for i := range *triggers {
		if (*triggers)[i].Name == trigger.Name {
			existing := (*triggers)[i]
			replaced = &existing
		}
	}
	if replaced != nil {
		if !statement.Replace || statement.Isconstraint {
			return nil, bindError("42710", "trigger already exists", 0)
		}
		trigger.ID, trigger.OID, trigger.Description = replaced.ID, replaced.OID, replaced.Description
	} else {
		trigger.ID, trigger.OID, err = options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, err
		}
	}
	var constraintRelation *model.Descriptor
	if statement.Constrrel != nil {
		if !trigger.Constraint {
			return nil, bindError("42P17", "constraint relation requires a constraint trigger", 0)
		}
		constraintParts := []string{statement.Constrrel.Relname}
		if statement.Constrrel.Schemaname != "" {
			constraintParts = []string{statement.Constrrel.Schemaname, statement.Constrrel.Relname}
		}
		referenced, _, resolveErr := b.resolve(constraintParts, []model.ObjectKind{model.ObjectTable}, "42P01", int(statement.Constrrel.Location)+1)
		if resolveErr != nil {
			return nil, resolveErr
		}
		constraintRelation = referenced
		b.dependency(referenced, 0)
	}
	if trigger.Constraint {
		if spec.Table == nil {
			return nil, bindError("42809", "constraint triggers require a table", 0)
		}
		for _, constraint := range spec.Table.Constraints {
			if constraint.Name == trigger.Name {
				return nil, bindError("42710", "constraint already exists", 0)
			}
		}
		constraintID, constraintOID, identityErr := options.Identities.AllocateDeclarationIdentity(ctx)
		if identityErr != nil {
			return nil, identityErr
		}
		trigger.ConstraintTable = table.ID()
		trigger.ConstraintID = constraintID
		if constraintRelation != nil {
			trigger.ConstraintRelation = constraintRelation.ID()
		}
		spec.Table.Constraints = append(spec.Table.Constraints, model.ConstraintDescriptor{ID: constraintID, OID: constraintOID, Name: trigger.Name, Kind: model.ConstraintTrigger, Deferrable: trigger.Deferrable, InitiallyDeferred: trigger.InitiallyDeferred, Validated: true, Enforced: true})
	}
	if replaced == nil {
		*triggers = append(*triggers, trigger)
	} else {
		for i := range *triggers {
			if (*triggers)[i].Name == trigger.Name {
				(*triggers)[i] = trigger
			}
		}
	}
	if replaced != nil {
		oldDependencies := make(map[model.Dependency]struct{})
		if replaced.Routine != 0 {
			for _, dependency := range spec.Dependencies {
				if dependency.Object == replaced.Routine {
					oldDependencies[dependency] = struct{}{}
				}
			}
		}
		if replaced.ConstraintRelation != 0 {
			for _, dependency := range spec.Dependencies {
				if dependency.Object == replaced.ConstraintRelation {
					oldDependencies[dependency] = struct{}{}
				}
			}
		}
		if replaced.When != nil {
			for _, dependency := range replaced.When.Dependencies {
				oldDependencies[dependency] = struct{}{}
			}
		}
		retainedObjects := nonTriggerObjectDependencies(spec)
		retainedDependencies := nonTriggerExpressionDependencies(spec)
		for _, descriptor := range *triggers {
			if descriptor.Routine != 0 {
				retainedObjects[descriptor.Routine] = struct{}{}
			}
			if descriptor.ConstraintRelation != 0 {
				retainedObjects[descriptor.ConstraintRelation] = struct{}{}
			}
			if descriptor.When != nil {
				for _, dependency := range descriptor.When.Dependencies {
					retainedDependencies[dependency] = struct{}{}
				}
			}
		}
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			if _, old := oldDependencies[dependency]; !old {
				return false
			}
			_, objectNeeded := retainedObjects[dependency.Object]
			_, exactNeeded := retainedDependencies[dependency]
			return !objectNeeded && !exactNeeded
		})
	}
	spec.Version = table.Version() + 1
	if routine != nil {
		spec.Dependencies = append(spec.Dependencies, model.Dependency{Object: routine.ID(), Version: routine.Version()})
	}
	if trigger.When != nil {
		spec.Dependencies = append(spec.Dependencies, trigger.When.Dependencies...)
	}
	if constraintRelation != nil && constraintRelation.ID() != table.ID() {
		spec.Dependencies = append(spec.Dependencies, model.Dependency{Object: constraintRelation.ID(), Version: constraintRelation.Version()})
	}
	slices.SortFunc(spec.Dependencies, func(a, b model.Dependency) int {
		if a.Object != b.Object {
			if a.Object < b.Object { return -1 }; return 1
		}
		if a.Column != b.Column {
			if a.Column < b.Column { return -1 }; return 1
		}
		if a.Version < b.Version { return -1 }; if a.Version > b.Version { return 1 }; return 0
	})
	spec.Dependencies = slices.Compact(spec.Dependencies)
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{next}, base: table, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, nil
}

func nonTriggerExpressionDependencies(spec model.DescriptorSpec) map[model.Dependency]struct{} {
	dependencies := make(map[model.Dependency]struct{})
	add := func(definition *model.ExpressionDefinition) {
		if definition == nil {
			return
		}
		for _, dependency := range definition.Dependencies {
			dependencies[dependency] = struct{}{}
		}
	}
	if spec.Table != nil {
		for i := range spec.Table.Columns {
			add(spec.Table.Columns[i].Default)
			add(spec.Table.Columns[i].Generated)
		}
		for i := range spec.Table.PrimaryKey {
			add(spec.Table.PrimaryKey[i].Expression)
		}
		for i := range spec.Table.PartitionKeys {
			add(spec.Table.PartitionKeys[i].Expression)
		}
		for i := range spec.Table.Constraints {
			add(spec.Table.Constraints[i].Expression)
		}
		for i := range spec.Table.ExtendedStatistics {
			for _, expression := range spec.Table.ExtendedStatistics[i].Expressions {
				add(expression)
			}
		}
		add(spec.Table.Partition)
	}
	if spec.View != nil {
		add(&spec.View.Query)
	}
	return dependencies
}

func nonTriggerObjectDependencies(spec model.DescriptorSpec) map[model.ObjectID]struct{} {
	dependencies := make(map[model.ObjectID]struct{})
	add := func(object model.ObjectID) {
		if object != 0 {
			dependencies[object] = struct{}{}
		}
	}
	if spec.Table != nil {
		for _, object := range spec.Table.Indexes {
			add(object)
		}
		for _, object := range spec.Table.Parents {
			add(object)
		}
		add(spec.Table.PartitionDefault)
		add(spec.Table.TypedOf)
		add(spec.Table.RowType)
		for _, constraint := range spec.Table.Constraints {
			add(constraint.ReferencedObject)
			add(constraint.ReferencedIndex)
			add(constraint.Index)
			add(constraint.ParentTable)
		}
	}
	if spec.View != nil {
		add(spec.View.RowType)
	}
	return dependencies
}

func resolveTriggerRoutine(ctx context.Context, b *binder, snapshot CatalogSnapshot, nodes []*zpgparse.Node) (*model.Descriptor, model.FunctionID, error) {
	parts, err := names(nodes)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, 0, bindError("42601", "invalid trigger routine name", 0)
	}
	name := parts[len(parts)-1]
	if len(parts) == 1 || parts[0] == "pg_catalog" {
		switch name {
		case "tsvector_update_trigger":
			return nil, 3752, nil
		case "tsvector_update_trigger_column":
			return nil, 3753, nil
		}
	}
	for _, schemaName := range b.functionPath(parts) {
		namespace, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(resolveErr) { continue }
		if resolveErr != nil { return nil, 0, resolveErr }
		var selected *model.Descriptor
		err = snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(p catalog.PublishedDescriptor) error {
			if p.Descriptor.Name().Name != name { return nil }
			spec, specErr := p.Descriptor.Spec(ctx)
			if specErr != nil { return specErr }
			if spec.Routine != nil && !spec.Routine.Procedure && len(routineInputs(spec.Routine.Parameters)) == 0 && spec.Routine.Result.Type.ID == 2279 {
				if selected != nil { return bindError("42725", "trigger routine is ambiguous", 0) }
				selected = p.Descriptor
			}
			return nil
		})
		if err != nil { return nil, 0, err }
		if selected != nil { b.dependency(namespace.Descriptor, 0); return selected, model.FunctionID(selected.OID()), nil }
	}
	return nil, 0, bindError("42883", "trigger function does not exist", 0)
}

func bindTriggerWhen(ctx context.Context, b *binder, snapshot CatalogSnapshot, options Options, columns []model.ColumnDescriptor, source *zpgparse.Node, events []string, row bool) (*model.ExpressionDefinition, error) {
	sql, err := zpgparse.DeparseExpression(source)
	if err != nil {
		return nil, err
	}
	owned := proto.Clone(source).(*zpgparse.Node)
	layouts := make([]model.TypeLayout, 0, len(columns)*2)
	columnIndex := make(map[string]int, len(columns))
	for _, column := range columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			columnIndex[column.Name] = len(layouts)
			layouts = append(layouts, column.Layout)
		}
	}
	count := len(layouts)
	layouts = append(layouts, slices.Clone(layouts)...)
	allowNew := row && (slices.Contains(events, "insert") || slices.Contains(events, "update"))
	allowOld := row && (slices.Contains(events, "update") || slices.Contains(events, "delete"))
	if err := rewriteTriggerColumns(owned, columnIndex, count, allowNew, allowOld); err != nil {
		return nil, err
	}
	statement := &zpgparse.RawStmt{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{TargetList: []*zpgparse.Node{{Node: &zpgparse.Node_ResTarget{ResTarget: &zpgparse.ResTarget{Val: owned}}}}}}}}
	bound, err := Bind(ctx, statement, snapshot, options, layouts)
	if err != nil { return nil, err }
	if len(bound.Columns()) != 1 || bound.Columns()[0].Layout.Type.Storage != model.TypeBoolean {
		return nil, bindError("42804", "trigger WHEN condition must be boolean", 0)
	}
	for _, dependency := range bound.Dependencies() {
		published, depErr := snapshot.Descriptor(ctx, dependency.Object)
		if depErr != nil { return nil, depErr }
		b.dependency(published.Descriptor, dependency.Column)
	}
	encoding, err := proto.Marshal(statement)
	if err != nil { return nil, err }
	return &model.ExpressionDefinition{SemanticVersion: 1, SQL: sql, Encoding: encoding, Dependencies: bound.Dependencies()}, nil
}

func rewriteTriggerColumns(node *zpgparse.Node, columns map[string]int, oldOffset int, allowNew, allowOld bool) error {
	if node == nil { return nil }
	if node.GetSubLink() != nil {
		return bindError("0A000", "trigger WHEN condition cannot contain a subquery", 0)
	}
	if reference := node.GetColumnRef(); reference != nil {
		if len(reference.Fields) != 2 || reference.Fields[0].GetString_() == nil || reference.Fields[1].GetString_() == nil {
			return bindError("42703", "trigger WHEN references must use OLD.column or NEW.column", int(reference.Location)+1)
		}
		row, name := strings.ToLower(reference.Fields[0].GetString_().Sval), reference.Fields[1].GetString_().Sval
		index, ok := columns[name]
		if !ok { return bindError("42703", "trigger WHEN column does not exist", int(reference.Location)+1) }
		switch row {
		case "new":
			if !allowNew { return bindError("42P17", "NEW row is not available for this trigger", int(reference.Location)+1) }
		case "old":
			if !allowOld { return bindError("42P17", "OLD row is not available for this trigger", int(reference.Location)+1) }
			index += oldOffset
		default:
			return bindError("42P17", "trigger WHEN can only reference OLD or NEW", int(reference.Location)+1)
		}
		node.Node = &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: int32(index + 1), Location: reference.Location}}
		return nil
	}
	return walkTriggerNodes(node.ProtoReflect(), func(child *zpgparse.Node) error {
		return rewriteTriggerColumns(child, columns, oldOffset, allowNew, allowOld)
	})
}

func walkTriggerNodes(message protoreflect.Message, visit func(*zpgparse.Node) error) error {
	fields := message.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if !message.Has(field) { continue }
		value := message.Get(field)
		if field.IsList() {
			list := value.List()
			for j := 0; j < list.Len(); j++ {
				if field.Kind() == protoreflect.MessageKind {

					child := list.Get(j).Message()
					if node, ok := child.Interface().(*zpgparse.Node); ok { if err := visit(node); err != nil { return err } } else if err := walkTriggerNodes(child, visit); err != nil { return err }
				}
			}
		} else if field.Kind() == protoreflect.MessageKind {
			child := value.Message()
			if node, ok := child.Interface().(*zpgparse.Node); ok { if err := visit(node); err != nil { return err } } else if err := walkTriggerNodes(child, visit); err != nil { return err }
		}
	}
	return nil
}
func rebindTriggerWhen(ctx context.Context, definition *model.ExpressionDefinition, sourceColumns, targetColumns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	if definition == nil {
		return nil, nil
	}
	if definition.SemanticVersion != 1 || definition.SQL != "" || len(definition.Encoding) == 0 || uint64(len(definition.Encoding)) > options.Limits.MaxBytes {
		return nil, model.ErrInvalidDescriptor
	}
	statement := &zpgparse.RawStmt{}
	if err := proto.Unmarshal(definition.Encoding, statement); err != nil || statement.Stmt == nil {
		return nil, model.ErrInvalidDescriptor
	}
	source := make([]model.ColumnDescriptor, 0, len(sourceColumns))
	for _, column := range sourceColumns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			source = append(source, column)
		}
	}
	target := make([]model.ColumnDescriptor, 0, len(targetColumns))
	targetPositions := make(map[model.ColumnID]int, len(targetColumns))
	for _, column := range targetColumns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			targetPositions[column.ID] = len(target)
			target = append(target, column)
		}
	}
	var rewrite func(*zpgparse.Node) error
	rewrite = func(node *zpgparse.Node) error {
		if node == nil {
			return nil
		}
		if parameter := node.GetParamRef(); parameter != nil {
			position := int(parameter.Number) - 1
			if position < 0 || position >= 2*len(source) {
				return model.ErrInvalidDescriptor
			}
			old := position >= len(source)
			if old {
				position -= len(source)
			}
			targetPosition, exists := targetPositions[source[position].ID]
			if !exists {
				return bindError("2BP01", "cannot drop column because a trigger WHEN condition depends on it", 0)
			}
			if old {
				targetPosition += len(target)
			}
			parameter.Number = int32(targetPosition + 1)
			return nil
		}
		return walkTriggerNodes(node.ProtoReflect(), rewrite)
	}
	if err := rewrite(statement.Stmt); err != nil {
		return nil, err
	}
	layouts := make([]model.TypeLayout, 0, len(target)*2)
	for _, column := range target {
		layouts = append(layouts, column.Layout)
	}
	layouts = append(layouts, slices.Clone(layouts)...)
	bound, err := Bind(ctx, statement, snapshot, options, layouts)
	if err != nil {
		return nil, err
	}
	if len(bound.Columns()) != 1 || bound.Columns()[0].Layout.Type.Storage != model.TypeBoolean {
		return nil, bindError("42804", "trigger WHEN condition must be boolean", 0)
	}
	encoding, err := (proto.MarshalOptions{Deterministic: true}).Marshal(statement)
	if err != nil {
		return nil, err
	}
	return &model.ExpressionDefinition{SemanticVersion: 1, Encoding: encoding, Dependencies: bound.Dependencies()}, nil
}
