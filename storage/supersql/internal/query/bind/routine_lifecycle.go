package bind

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func routineObjectKind(kind zpgparse.ObjectType) bool {
	return kind == zpgparse.ObjectType_OBJECT_FUNCTION || kind == zpgparse.ObjectType_OBJECT_PROCEDURE || kind == zpgparse.ObjectType_OBJECT_ROUTINE
}
// IsRoutineLifecycleCommand identifies descriptor mutations executed by RoutineDDL.
func IsRoutineLifecycleCommand(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_AlterFunctionStmt:
		return statement.AlterFunctionStmt != nil && routineObjectKind(statement.AlterFunctionStmt.Objtype)
	case *zpgparse.Node_DropStmt:
		return statement.DropStmt != nil && (routineObjectKind(statement.DropStmt.RemoveType) || statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_TRIGGER)
	case *zpgparse.Node_RenameStmt:
		return statement.RenameStmt != nil && (routineObjectKind(statement.RenameStmt.RenameType) || statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_TRIGGER)
	case *zpgparse.Node_AlterObjectSchemaStmt:
		return statement.AlterObjectSchemaStmt != nil && routineObjectKind(statement.AlterObjectSchemaStmt.ObjectType)
	default:
		return false
	}
}


func routineKindMatches(metadata *model.RoutineMetadata, kind zpgparse.ObjectType) bool {
	if metadata == nil {
		return false
	}
	switch kind {
	case zpgparse.ObjectType_OBJECT_FUNCTION:
		return !metadata.Procedure
	case zpgparse.ObjectType_OBJECT_PROCEDURE:
		return metadata.Procedure
	case zpgparse.ObjectType_OBJECT_ROUTINE:
		return true
	default:
		return false
	}
}

func resolveRoutineReference(ctx context.Context, b *binder, snapshot CatalogSnapshot, object *zpgparse.ObjectWithArgs, kind zpgparse.ObjectType) (*model.Descriptor, error) {
	if object == nil || !routineObjectKind(kind) {
		return nil, bindError("42601", "invalid routine reference", 0)
	}
	parts, err := names(object.Objname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid routine reference", 0)
	}
	var signature string
	if !object.ArgsUnspecified {
		parameters := make([]model.RoutineParameter, len(object.Objargs))
		for i, node := range object.Objargs {
			layout, typeErr := b.typeName(node.GetTypeName())
			if typeErr != nil {
				return nil, typeErr
			}
			parameters[i] = model.RoutineParameter{Mode: "in", Layout: layout}
		}
		signature = routineSignature(parameters)
	}
	for _, schemaName := range b.functionPath(parts) {
		namespace, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(resolveErr) {
			continue
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		var selected *model.Descriptor
		if object.ArgsUnspecified {
			resolveErr = snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(published catalog.PublishedDescriptor) error {
				descriptor := published.Descriptor
				if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Name().Name != parts[len(parts)-1] {
					return nil
				}
				spec, specErr := descriptor.Spec(ctx)
				if specErr != nil {
					return specErr
				}
				if !routineKindMatches(spec.Routine, kind) {
					return nil
				}
				if selected != nil {
					return bindError("42725", "routine reference is ambiguous", 0)
				}
				selected = descriptor
				return nil
			})
		} else {
			var published catalog.PublishedDescriptor
			published, resolveErr = snapshot.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectRoutine, Name: parts[len(parts)-1], Signature: signature})
			if resolveErr == nil {
				spec, specErr := published.Descriptor.Spec(ctx)
				if specErr != nil {
					return nil, specErr
				}
				if routineKindMatches(spec.Routine, kind) {
					selected = published.Descriptor
				}
			}
		}
		if resolveErr != nil && !isMissingObject(resolveErr) {
			return nil, resolveErr
		}
		if selected != nil {
			b.dependency(namespace.Descriptor, 0)
			b.dependency(selected, 0)
			return selected, nil
		}
		if len(parts) == 2 {
			break
		}
	}
	return nil, bindError("42883", "routine does not exist", 0)
}

func routineExpressionDependencies(metadata *model.RoutineMetadata) map[model.Dependency]struct{} {
	retained := make(map[model.Dependency]struct{}, len(metadata.Body.Dependencies))
	for _, dependency := range metadata.Body.Dependencies {
		retained[dependency] = struct{}{}
	}
	for _, parameter := range metadata.Parameters {
		if parameter.Default == nil {
			continue
		}
		for _, dependency := range parameter.Default.Dependencies {
			retained[dependency] = struct{}{}
		}
	}
	return retained
}

func appendDependency(dependencies []model.Dependency, dependency model.Dependency) []model.Dependency {
	if slices.Contains(dependencies, dependency) {
		return dependencies
	}
	return append(dependencies, dependency)
}

func newRoutineMutation(ctx context.Context, base *model.Descriptor, spec model.DescriptorSpec, options CreateDeclarationOptions, b *binder, tag string) (*BoundObjectCommand, error) {
	if base == nil || base.Version() == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	spec.Version = base.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: tag, updates: []ObjectMutation{{Base: base, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, nil
}

func bindDropRoutines(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || !routineObjectKind(statement.RemoveType) {
		return nil, bindError("42601", "invalid DROP routine", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := &BoundObjectCommand{tag: objectCommandTag("DROP", statement.RemoveType), cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE}
	seen := make(map[model.ObjectID]struct{}, len(statement.Objects))
	for _, node := range statement.Objects {
		descriptor, resolveErr := resolveRoutineReference(ctx, b, snapshot, node.GetObjectWithArgs(), statement.RemoveType)
		if resolveErr != nil {
			var diagnostic *model.Error
			if statement.MissingOk && errors.As(resolveErr, &diagnostic) && diagnostic.Code == "42883" {
				command.warnings = append(command.warnings, model.Error{Code: diagnostic.Code, Message: "routine does not exist, skipping"})
				continue
			}
			return nil, resolveErr
		}
		if _, duplicate := seen[descriptor.ID()]; duplicate {
			continue
		}
		seen[descriptor.ID()] = struct{}{}
		command.drops = append(command.drops, descriptor)
	}
	command.dependencies = slices.Clone(b.dependencies)
	command.warnings = append(command.warnings, b.warnings...)
	return command, ctx.Err()
}

func bindRenameRoutine(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newname == "" {
		return nil, bindError("42601", "invalid routine rename", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveRoutineReference(ctx, b, snapshot, statement.Object.GetObjectWithArgs(), statement.RenameType)
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	identity := spec.Name
	identity.Name = statement.Newname
	if published, conflictErr := snapshot.Resolve(ctx, identity); conflictErr == nil && published.Descriptor.ID() != base.ID() {
		return nil, bindError("42723", "routine already exists", 0)
	} else if conflictErr != nil && !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	spec.Name = identity
	return newRoutineMutation(ctx, base, spec, options, b, objectCommandTag("ALTER", statement.RenameType))
}

func bindSetRoutineSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid routine SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveRoutineReference(ctx, b, snapshot, statement.Object.GetObjectWithArgs(), statement.ObjectType)
	if err != nil {
		return nil, err
	}
	target, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	b.dependency(target.Descriptor, 0)
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	oldParent := spec.Name.Parent
	spec.Name.Parent = target.Descriptor.ID()
	if published, conflictErr := snapshot.Resolve(ctx, spec.Name); conflictErr == nil && published.Descriptor.ID() != base.ID() {
		return nil, bindError("42723", "routine already exists", 0)
	} else if conflictErr != nil && !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	retained := routineExpressionDependencies(spec.Routine)
	spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
		_, needed := retained[dependency]
		return dependency.Object == oldParent && !needed
	})
	spec.Dependencies = appendDependency(spec.Dependencies, model.Dependency{Object: target.Descriptor.ID(), Version: target.Descriptor.Version()})
	return newRoutineMutation(ctx, base, spec, options, b, objectCommandTag("ALTER", statement.ObjectType))
}


func bindAlterRoutine(ctx context.Context, statement *zpgparse.AlterFunctionStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || !routineObjectKind(statement.Objtype) {
		return nil, bindError("42601", "invalid ALTER routine", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterFunctionStmt{AlterFunctionStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveRoutineReference(ctx, b, snapshot, statement.Func, statement.Objtype)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Routine == nil {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	metadata := spec.Routine
	oldSupport := metadata.Support
	for _, node := range statement.Actions {
		option := node.GetDefElem()
		if option == nil {
			return nil, bindError("42601", "invalid ALTER routine option", 0)
		}
		switch option.Defname {
		case "volatility":
			metadata.Volatility = strings.ToLower(option.Arg.GetString_().GetSval())
		case "strict":
			metadata.Strict = option.Arg.GetBoolean().GetBoolval()
		case "security":
			if option.Arg.GetBoolean().GetBoolval() {
				return nil, unsupported(Authorization, int(option.Location)+1)
			}
			metadata.SecurityDefiner = false
		case "leakproof":
			if option.Arg.GetBoolean().GetBoolval() {
				return nil, unsupported(Authorization, int(option.Location)+1)
			}
			metadata.Leakproof = false
		case "parallel":
			value := strings.ToLower(option.Arg.GetString_().GetSval())
			if value != "unsafe" && value != "restricted" && value != "safe" {
				return nil, bindError("22023", "invalid PARALLEL option", int(option.Location)+1)
			}
			metadata.Parallel = value
		case "cost":
			value, valueErr := routineNumericOption(option)
			if valueErr != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, bindError("22023", "COST must be greater than zero", int(option.Location)+1)
			}
			metadata.Cost = value
		case "rows":
			value, valueErr := routineNumericOption(option)
			if valueErr != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || !metadata.ReturnsSet {
				return nil, bindError("22023", "ROWS is only valid for a set-returning routine", int(option.Location)+1)
			}
			metadata.Rows = value
		case "support":
			support, supportErr := resolveSupportRoutine(ctx, b, snapshot, routineOptionText(option.Arg))
			if supportErr != nil {
				return nil, supportErr
			}
			metadata.Support, metadata.SupportFunction = support.ID(), model.FunctionID(support.OID())
		case "set":
			settingStmt := option.Arg.GetVariableSetStmt()
			if settingStmt != nil && settingStmt.Kind == zpgparse.VariableSetKind_VAR_RESET_ALL {
				metadata.Settings = nil
				continue
			}
			setting, settingErr := bindRoutineSetting(b, settingStmt, options.CurrentSetting)
			if settingErr != nil {
				return nil, settingErr
			}
			metadata.Settings = slices.DeleteFunc(metadata.Settings, func(existing model.RoutineSetting) bool { return strings.EqualFold(existing.Name, setting.Name) })
			if !setting.Reset {
				metadata.Settings = append(metadata.Settings, setting)
			}
		default:
			return nil, bindError("42601", "unrecognized ALTER routine option", int(option.Location)+1)
		}
	}
	retained := routineExpressionDependencies(metadata)
	if oldSupport != 0 && oldSupport != metadata.Support {
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			_, needed := retained[dependency]
			return dependency.Object == oldSupport && !needed
		})
	}
	if metadata.Support != 0 && metadata.Support != oldSupport {
		support, resolveErr := snapshot.Descriptor(ctx, metadata.Support)
		if resolveErr != nil {
			return nil, resolveErr
		}
		spec.Dependencies = appendDependency(spec.Dependencies, model.Dependency{Object: support.Descriptor.ID(), Version: support.Descriptor.Version()})
	}
	spec.Routine = metadata
	return newRoutineMutation(ctx, base, spec, options, b, objectCommandTag("ALTER", statement.Objtype))
}

func triggerTarget(ctx context.Context, b *binder, relation *zpgparse.RangeVar) (*model.Descriptor, model.DescriptorSpec, *[]model.TriggerDescriptor, error) {
	if relation == nil || relation.Relname == "" || relation.Catalogname != "" {
		return nil, model.DescriptorSpec{}, nil, bindError("42601", "invalid trigger relation", 0)
	}
	parts := []string{relation.Relname}
	if relation.Schemaname != "" {
		parts = []string{relation.Schemaname, relation.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectView}, "42P01", int(relation.Location)+1)
	if err != nil {
		return nil, model.DescriptorSpec{}, nil, err
	}
	spec, err := table.Spec(ctx)
	if err != nil {
		return nil, model.DescriptorSpec{}, nil, err
	}
	var triggers *[]model.TriggerDescriptor
	if spec.Table != nil {
		triggers = &spec.Table.Triggers
	} else if spec.View != nil {
		triggers = &spec.View.Triggers
	} else {
		return nil, model.DescriptorSpec{}, nil, model.ErrInvalidDescriptor
	}
	b.dependency(table, 0)
	return table, spec, triggers, nil
}

func removeTriggerDependencies(spec *model.DescriptorSpec, removed model.TriggerDescriptor, remaining []model.TriggerDescriptor) {
	old := make(map[model.Dependency]struct{})
	if removed.Routine != 0 {
		for _, dependency := range spec.Dependencies {
			if dependency.Object == removed.Routine {
				old[dependency] = struct{}{}
			}
		}
	}
	if removed.ConstraintRelation != 0 {
		for _, dependency := range spec.Dependencies {
			if dependency.Object == removed.ConstraintRelation {
				old[dependency] = struct{}{}
			}
		}
	}
	if removed.When != nil {
		for _, dependency := range removed.When.Dependencies {
			old[dependency] = struct{}{}
		}
	}
	retainedObjects := nonTriggerObjectDependencies(*spec)
	retained := nonTriggerExpressionDependencies(*spec)
	for _, trigger := range remaining {
		if trigger.Routine != 0 {
			retainedObjects[trigger.Routine] = struct{}{}
		}
		if trigger.ConstraintRelation != 0 {
			retainedObjects[trigger.ConstraintRelation] = struct{}{}
		}
		if trigger.When != nil {
			for _, dependency := range trigger.When.Dependencies {
				retained[dependency] = struct{}{}
			}
		}
	}
	spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
		if _, candidate := old[dependency]; !candidate {
			return false
		}
		_, objectNeeded := retainedObjects[dependency.Object]
		_, exactNeeded := retained[dependency]
		return !objectNeeded && !exactNeeded
	})
}

func finishTriggerMutation(ctx context.Context, b *binder, table *model.Descriptor, spec model.DescriptorSpec, options CreateDeclarationOptions, tag string) (*BoundObjectCommand, error) {
	if table.Version() == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	spec.Version = table.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: tag, updates: []ObjectMutation{{Base: table, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, nil
}

func bindDropTrigger(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || len(statement.Objects) != 1 {
		return nil, bindError("42601", "invalid DROP TRIGGER", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	items := statement.Objects[0].GetList().GetItems()
	if len(items) < 2 {
		return nil, bindError("42601", "invalid DROP TRIGGER", 0)
	}
	triggerNode := items[len(items)-1].GetString_()
	if triggerNode == nil || triggerNode.Sval == "" {
		return nil, bindError("42601", "invalid DROP TRIGGER name", 0)
	}
	triggerName := triggerNode.Sval
	relationParts, err := names(items[:len(items)-1])
	if err != nil || len(relationParts) == 0 || len(relationParts) > 2 {
		return nil, bindError("42601", "invalid DROP TRIGGER relation", 0)
	}
	relation := &zpgparse.RangeVar{Relname: relationParts[len(relationParts)-1]}
	if len(relationParts) == 2 {
		relation.Schemaname = relationParts[0]
	}
	table, spec, triggers, err := triggerTarget(ctx, b, relation)
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && (diagnostic.Code == "42P01" || diagnostic.Code == "3F000") {
				return &BoundObjectCommand{tag: "DROP TRIGGER", warnings: []model.Error{{Code: diagnostic.Code, Message: "trigger does not exist, skipping"}}}, nil
			}
		}
		return nil, err
	}
	index := slices.IndexFunc(*triggers, func(trigger model.TriggerDescriptor) bool { return trigger.Name == triggerName })
	if index < 0 {
		if statement.MissingOk {
			return &BoundObjectCommand{tag: "DROP TRIGGER", warnings: []model.Error{{Code: "42704", Message: "trigger does not exist, skipping"}}, dependencies: slices.Clone(b.dependencies)}, nil
		}
		return nil, bindError("42704", "trigger does not exist", 0)
	}
	removed := (*triggers)[index]
	*triggers = slices.Delete(*triggers, index, index+1)
	if spec.Table != nil && removed.ConstraintID != 0 {
		spec.Table.Constraints = slices.DeleteFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.ID == removed.ConstraintID && constraint.Kind == model.ConstraintTrigger })
	}
	removeTriggerDependencies(&spec, removed, *triggers)
	return finishTriggerMutation(ctx, b, table, spec, options, "DROP TRIGGER")
}

func bindRenameTrigger(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Subname == "" || statement.Newname == "" {
		return nil, bindError("42601", "invalid ALTER TRIGGER RENAME", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	table, spec, triggers, err := triggerTarget(ctx, b, statement.Relation)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(*triggers, func(trigger model.TriggerDescriptor) bool { return trigger.Name == statement.Newname }) {
		return nil, bindError("42710", "trigger already exists", 0)
	}
	index := slices.IndexFunc(*triggers, func(trigger model.TriggerDescriptor) bool { return trigger.Name == statement.Subname })
	if index < 0 {
		return nil, bindError("42704", "trigger does not exist", 0)
	}
	trigger := &(*triggers)[index]
	trigger.Name = statement.Newname
	if spec.Table != nil && trigger.ConstraintID != 0 {
		for i := range spec.Table.Constraints {
			if spec.Table.Constraints[i].ID == trigger.ConstraintID && spec.Table.Constraints[i].Kind == model.ConstraintTrigger {
				spec.Table.Constraints[i].Name = statement.Newname
			}
		}
	}
	return finishTriggerMutation(ctx, b, table, spec, options, "ALTER TRIGGER")
}
