package bind

import (
	"context"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TriggerStateAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) == 0 {
		return false
	}
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command == nil || !triggerStateSubtype(command.Subtype) {
			return false
		}
	}
	return true
}

func triggerStateSubtype(subtype zpgparse.AlterTableType) bool {
	switch subtype {
	case zpgparse.AlterTableType_AT_EnableTrig,
		zpgparse.AlterTableType_AT_EnableAlwaysTrig,
		zpgparse.AlterTableType_AT_EnableReplicaTrig,
		zpgparse.AlterTableType_AT_DisableTrig,
		zpgparse.AlterTableType_AT_EnableTrigAll,
		zpgparse.AlterTableType_AT_DisableTrigAll,
		zpgparse.AlterTableType_AT_EnableTrigUser,
		zpgparse.AlterTableType_AT_DisableTrigUser:
		return true
	default:
		return false
	}
}

func bindTriggerStateAlter(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if !TriggerStateAlter(statement) {
		return nil, bindError("42601", "invalid ALTER TABLE trigger state command", 0)
	}
	node := &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}}
	b, err := triggerMutationBinder(ctx, snapshot, options, node)
	if err != nil {
		return nil, err
	}
	root, err := b.resolveTableRange(statement.Relation)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundObjectCommand{tag: "ALTER TABLE", warnings: []model.Error{{Code: "42P01", Message: "relation does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	tables, err := triggerStateTables(ctx, b, snapshot, root, statement.Relation.Inh, options)
	if err != nil {
		return nil, err
	}
	type pendingTable struct {
		base *model.Descriptor
		spec model.DescriptorSpec
	}
	pending := make([]pendingTable, len(tables))
	for i, table := range tables {
		spec, specErr := table.SpecWithin(ctx, options.Descriptors)
		if specErr != nil {
			return nil, specErr
		}
		if spec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		if spec.Table.Owner != options.Owner {
			return nil, bindError("42501", "must be owner of table", int(statement.Relation.Location)+1)
		}
		metadata := cloneTableMetadata(spec.Table)
		metadata.Triggers = slices.Clone(spec.Table.Triggers)
		spec.Table = metadata
		pending[i] = pendingTable{base: table, spec: spec}
	}
	rootIndex := slices.IndexFunc(pending, func(candidate pendingTable) bool { return candidate.base.ID() == root.ID() })
	if rootIndex < 0 {
		return nil, model.ErrInvalidDescriptor
	}
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command.Def != nil || command.Newowner != nil || command.MissingOk || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
			return nil, bindError("42601", "invalid ALTER TABLE trigger state command", 0)
		}
		mode, all, user, modeErr := triggerStateCommand(command)
		if modeErr != nil {
			return nil, modeErr
		}
		rootMatched := false
		for tableIndex := range pending {
			triggers := pending[tableIndex].spec.Table.Triggers
			for triggerIndex := range triggers {
				trigger := &triggers[triggerIndex]
				selected := all || user && !trigger.Internal || command.Name != "" && trigger.Name == command.Name
				if !selected {
					continue
				}
				if trigger.Internal {
					return nil, bindError("42501", "permission denied: trigger is a system trigger", 0)
				}
				trigger.EnableMode = mode
				if tableIndex == rootIndex {
					rootMatched = true
				}
			}
		}
		if command.Name != "" && !rootMatched {
			return nil, bindError("42704", "trigger does not exist", 0)
		}
	}
	updates := make([]ObjectMutation, 0, len(pending))
	for _, table := range pending {
		if table.base.Version() == math.MaxUint64 {
			return nil, model.ErrDescriptorLimit
		}
		table.spec.Version = table.base.Version() + 1
		next, nextErr := model.NewDescriptor(ctx, table.spec, options.Descriptors)
		if nextErr != nil {
			return nil, nextErr
		}
		if nextErr = table.base.ValidateSuccessor(ctx, next); nextErr != nil {
			return nil, nextErr
		}
		updates = append(updates, ObjectMutation{Base: table.base, Next: next})
	}
	return &BoundObjectCommand{tag: "ALTER TABLE", updates: updates, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func triggerMutationBinder(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node) (*binder, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	if snapshot == nil || options.Owner == 0 || node == nil {
		return nil, bindError("22023", "trigger mutation binding requires catalog and owner", 0)
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	remaining, payload := options.Bind.Limits.MaxNodes, options.Bind.Limits.MaxBytes
	if err := b.checkAST(node.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	if err := ValidateStatementFeatures(ctx, node); err != nil {
		return nil, err
	}
	return b, nil
}

func triggerStateCommand(command *zpgparse.AlterTableCmd) (model.TriggerEnableMode, bool, bool, error) {
	if command == nil {
		return 0, false, false, bindError("42601", "invalid ALTER TABLE trigger state command", 0)
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_EnableTrig:
		if command.Name == "" {
			return 0, false, false, bindError("42601", "ENABLE TRIGGER requires a trigger name", 0)
		}
		return model.TriggerEnabledOrigin, false, false, nil
	case zpgparse.AlterTableType_AT_EnableAlwaysTrig:
		if command.Name == "" {
			return 0, false, false, bindError("42601", "ENABLE ALWAYS TRIGGER requires a trigger name", 0)
		}
		return model.TriggerEnabledAlways, false, false, nil
	case zpgparse.AlterTableType_AT_EnableReplicaTrig:
		if command.Name == "" {
			return 0, false, false, bindError("42601", "ENABLE REPLICA TRIGGER requires a trigger name", 0)
		}
		return model.TriggerEnabledReplica, false, false, nil
	case zpgparse.AlterTableType_AT_DisableTrig:
		if command.Name == "" {
			return 0, false, false, bindError("42601", "DISABLE TRIGGER requires a trigger name", 0)
		}
		return model.TriggerDisabled, false, false, nil
	case zpgparse.AlterTableType_AT_EnableTrigAll:
		return model.TriggerEnabledOrigin, true, false, nil
	case zpgparse.AlterTableType_AT_DisableTrigAll:
		return model.TriggerDisabled, true, false, nil
	case zpgparse.AlterTableType_AT_EnableTrigUser:
		return model.TriggerEnabledOrigin, false, true, nil
	case zpgparse.AlterTableType_AT_DisableTrigUser:
		return model.TriggerDisabled, false, true, nil
	default:
		return 0, false, false, bindError("42601", "invalid ALTER TABLE trigger state command", 0)
	}
}

func triggerStateTables(ctx context.Context, b *binder, snapshot CatalogSnapshot, root *model.Descriptor, recurse bool, options CreateDeclarationOptions) ([]*model.Descriptor, error) {
	if !recurse {
		return []*model.Descriptor{root}, nil
	}
	all := make(map[model.ObjectID]*model.Descriptor)
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
				all[published.Descriptor.ID()] = published.Descriptor
			}
			return nil
		})
	}); err != nil {
		return nil, err
	}
	selected := map[model.ObjectID]*model.Descriptor{root.ID(): root}
	queue := []model.ObjectID{root.ID()}
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		children := make([]*model.Descriptor, 0)
		for id, candidate := range all {
			if _, exists := selected[id]; exists {
				continue
			}
			metadata, ok := candidate.Table()
			if ok && metadata.PartitionState != model.PartitionDetachPending && slices.Contains(metadata.Parents, parent) {
				children = append(children, candidate)
			}
		}
		slices.SortFunc(children, func(left, right *model.Descriptor) int {
			if left.ID() < right.ID() {
				return -1
			}
			if left.ID() > right.ID() {
				return 1
			}
			return 0
		})
		for _, child := range children {
			if uint64(len(selected)) >= uint64(options.Bind.Limits.MaxOperands) {
				return nil, bindError("54000", "trigger inheritance expansion limit exceeded", 0)
			}
			selected[child.ID()] = child
			queue = append(queue, child.ID())
			b.dependency(child, 0)
		}
	}
	result := make([]*model.Descriptor, 0, len(selected))
	for _, table := range selected {
		result = append(result, table)
	}
	slices.SortFunc(result, func(left, right *model.Descriptor) int {
		if left.ID() < right.ID() {
			return -1
		}
		if left.ID() > right.ID() {
			return 1
		}
		return 0
	})
	return result, nil
}
