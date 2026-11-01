package bind

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// TextSearchLifecycleKind reports whether a statement mutates the lifecycle of
// an already-modeled text search dictionary or configuration. Parsers and
// templates are external code and remain excluded.
func TextSearchLifecycleKind(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_DropStmt:
		return statement.DropStmt != nil && textSearchObjectKind(statement.DropStmt.RemoveType)
	case *zpgparse.Node_RenameStmt:
		return statement.RenameStmt != nil && textSearchObjectKind(statement.RenameStmt.RenameType)
	case *zpgparse.Node_AlterObjectSchemaStmt:
		return statement.AlterObjectSchemaStmt != nil && textSearchObjectKind(statement.AlterObjectSchemaStmt.ObjectType)
	default:
		return false
	}
}

func textSearchObjectKind(kind zpgparse.ObjectType) bool {
	return kind == zpgparse.ObjectType_OBJECT_TSDICTIONARY || kind == zpgparse.ObjectType_OBJECT_TSCONFIGURATION
}

func textSearchObjectModelKind(kind zpgparse.ObjectType) model.ObjectKind {
	if kind == zpgparse.ObjectType_OBJECT_TSCONFIGURATION {
		return model.ObjectTextSearchConfiguration
	}
	return model.ObjectTextSearchDictionary
}

// BindTextSearchLifecycle binds DROP/RENAME/SET SCHEMA for text search
// dictionaries and configurations. Deletion and rename stage real descriptor
// updates through the ordinary object change set; a configuration that still
// references a dictionary holds an explicit dependency, so RESTRICT fails and
// CASCADE removes the dependent configuration as well.
func BindTextSearchLifecycle(ctx context.Context, node *zpgparse.Node, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, bool, error) {
	if node == nil || !TextSearchLifecycleKind(node) {
		return nil, false, nil
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_DropStmt:
		command, err := bindDropTextSearch(ctx, statement.DropStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_RenameStmt:
		command, err := bindRenameTextSearch(ctx, statement.RenameStmt, snapshot, options)
		return command, true, err
	default:
		command, err := bindSetTextSearchSchema(ctx, node.GetAlterObjectSchemaStmt(), snapshot, options)
		return command, true, err
	}
}

func bindDropTextSearch(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || !textSearchObjectKind(statement.RemoveType) || statement.Concurrent {
		return nil, bindError("42601", "invalid DROP TEXT SEARCH", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := &BoundObjectCommand{
		tag:     "DROP TEXT SEARCH DICTIONARY",
		cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE,
	}
	if statement.RemoveType == zpgparse.ObjectType_OBJECT_TSCONFIGURATION {
		command.tag = "DROP TEXT SEARCH CONFIGURATION"
	}
	kind := textSearchObjectModelKind(statement.RemoveType)
	seen := make(map[model.ObjectID]bool, len(statement.Objects))
	for _, node := range statement.Objects {
		parts, err := objectNodeParts(node)
		if err != nil {
			return nil, err
		}
		descriptor, _, err := b.resolve(parts, []model.ObjectKind{kind}, "42704", 0)
		if err != nil {
			var diagnostic *model.Error
			if statement.MissingOk && errors.As(err, &diagnostic) && diagnostic.Code == "42704" {
				command.warnings = append(command.warnings, model.Error{Code: diagnostic.Code, Message: "text search object does not exist, skipping"})
				continue
			}
			return nil, err
		}
		if seen[descriptor.ID()] {
			continue
		}
		seen[descriptor.ID()] = true
		command.drops = append(command.drops, descriptor)
		b.dependency(descriptor, 0)
	}
	command.dependencies = b.dependencies
	command.warnings = append(command.warnings, b.warnings...)
	return command, ctx.Err()
}

func bindRenameTextSearch(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Object == nil || statement.Newname == "" {
		return nil, bindError("42601", "invalid ALTER TEXT SEARCH RENAME", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	base, err := resolveTextSearchLifecycleObject(ctx, b, statement.Object, statement.RenameType)
	if err != nil {
		return nil, err
	}
	if _, conflictErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: base.Name().Parent, Kind: base.Kind(), Name: statement.Newname}); conflictErr == nil {
		return nil, bindError("42710", "text search object already exists", 0)
	} else if !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Name.Name = statement.Newname
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "ALTER TEXT SEARCH",
	}, ctx.Err()
}

func bindSetTextSearchSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Object == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid ALTER TEXT SEARCH SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveTextSearchLifecycleObject(ctx, b, statement.Object, statement.ObjectType)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundObjectCommand{tag: "ALTER TEXT SEARCH", warnings: []model.Error{{Code: "42704", Message: "text search object does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	destination, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	if destination.Descriptor.State() != model.StatePublic {
		return nil, bindError("3F000", "schema does not exist", 0)
	}
	if _, conflictErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: destination.Descriptor.ID(), Kind: base.Kind(), Name: base.Name().Name}); conflictErr == nil {
		return nil, bindError("42710", "text search object already exists in destination schema", 0)
	} else if !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error {
		previous := spec.Name.Parent
		spec.Name.Parent = destination.Descriptor.ID()
		spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: destination.Descriptor.ID(), Version: destination.Descriptor.Version()})
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			return dependency.Object == previous && dependency.Column == 0
		})
		slices.SortFunc(spec.Dependencies, compareDependency)
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.dependency(destination.Descriptor, 0)
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "ALTER TEXT SEARCH",
	}, ctx.Err()
}

func resolveTextSearchLifecycleObject(ctx context.Context, b *binder, object *zpgparse.Node, kind zpgparse.ObjectType) (*model.Descriptor, error) {
	parts, err := objectNodeParts(object)
	if err != nil {
		return nil, err
	}
	descriptor, _, err := b.resolve(parts, []model.ObjectKind{textSearchObjectModelKind(kind)}, "42704", 0)
	return descriptor, err
}
