package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// DescribedObjectCommentKind reports whether COMMENT ON targets an
// already-modeled object whose description is stored on a descriptor or its
// nested metadata outside the ordinary relation/column/schema/sequence family
// handled by bindCommentObject. Types, domains, routines, triggers,
// constraints, extended statistics, and text search objects qualify. Aggregate,
// cast, and operator descriptions belong to the extensible definition family
// and are not admitted here.
func DescribedObjectCommentKind(kind zpgparse.ObjectType) bool {
	switch kind {
	case zpgparse.ObjectType_OBJECT_TYPE, zpgparse.ObjectType_OBJECT_DOMAIN,
		zpgparse.ObjectType_OBJECT_DOMCONSTRAINT, zpgparse.ObjectType_OBJECT_FUNCTION,
		zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE,
		zpgparse.ObjectType_OBJECT_TRIGGER, zpgparse.ObjectType_OBJECT_TABCONSTRAINT,
		zpgparse.ObjectType_OBJECT_STATISTIC_EXT, zpgparse.ObjectType_OBJECT_TSCONFIGURATION,
		zpgparse.ObjectType_OBJECT_TSDICTIONARY:
		return true
	default:
		return false
	}
}

// bindCommentDescribedObject stages one descriptor or nested-metadata
// description. An empty comment removes the description, matching PostgreSQL's
// NULL handling; pg_description emission already skips empty text.
func bindCommentDescribedObject(ctx context.Context, statement *zpgparse.CommentStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Object == nil || !DescribedObjectCommentKind(statement.Objtype) {
		return nil, bindError("42601", "invalid COMMENT ON", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CommentStmt{CommentStmt: statement}})
	if err != nil {
		return nil, err
	}
	switch statement.Objtype {
	case zpgparse.ObjectType_OBJECT_TYPE, zpgparse.ObjectType_OBJECT_DOMAIN:
		return commentOnType(ctx, b, statement, options)
	case zpgparse.ObjectType_OBJECT_FUNCTION, zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE:
		return commentOnRoutine(ctx, b, statement, snapshot, options)
	case zpgparse.ObjectType_OBJECT_TRIGGER:
		return commentOnTrigger(ctx, b, statement, options)
	case zpgparse.ObjectType_OBJECT_TABCONSTRAINT:
		return commentOnTableConstraint(ctx, b, statement, options)
	case zpgparse.ObjectType_OBJECT_DOMCONSTRAINT:
		return commentOnDomainConstraint(ctx, b, statement, options)
	case zpgparse.ObjectType_OBJECT_STATISTIC_EXT:
		return commentOnStatistics(ctx, b, statement, options)
	default:
		return commentOnTextSearch(ctx, b, statement, options)
	}
}

func commentOnType(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	parts, err := typeNameFromNode(statement.Object)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Type == nil {
		return nil, bindError("42809", "object is not a type", 0)
	}
	if statement.Objtype == zpgparse.ObjectType_OBJECT_DOMAIN && spec.Type.DeclaredKind() != model.TypeKindDomain {
		return nil, bindError("42809", "object is not a domain", 0)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

func commentOnRoutine(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	object := statement.Object.GetObjectWithArgs()
	if object == nil {
		return nil, bindError("42601", "invalid COMMENT ON routine", 0)
	}
	base, err := resolveRoutineReference(ctx, b, snapshot, object, statement.Objtype)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Routine == nil {
		return nil, bindError("42809", "object is not a routine", 0)
	}
	spec.Description = statement.Comment
	return newRoutineMutation(ctx, base, spec, options, b, "COMMENT")
}

func commentOnTrigger(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	name, table, err := nestedObjectTarget(statement.Object)
	if err != nil {
		return nil, err
	}
	descriptor, _, triggers, err := triggerTarget(ctx, b, table)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(*triggers, func(trigger model.TriggerDescriptor) bool { return trigger.Name == name })
	if index < 0 {
		return nil, bindError("42704", "trigger does not exist", 0)
	}
	next, err := nextObjectDescriptor(ctx, descriptor, options, func(next *model.DescriptorSpec) error {
		var target *[]model.TriggerDescriptor
		switch {
		case next.Table != nil:
			target = &next.Table.Triggers
		case next.View != nil:
			target = &next.View.Triggers
		default:
			return model.ErrInvalidDescriptor
		}
		position := slices.IndexFunc(*target, func(trigger model.TriggerDescriptor) bool { return trigger.Name == name })
		if position < 0 {
			return bindError("42704", "trigger does not exist", 0)
		}
		(*target)[position].Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: descriptor, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

func commentOnTableConstraint(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	name, table, err := nestedObjectTarget(statement.Object)
	if err != nil {
		return nil, err
	}
	descriptor, spec, _, err := triggerTarget(ctx, b, table)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, bindError("42809", "relation is not a table", 0)
	}
	index := constraintIndexByName(spec.Table.Constraints, name)
	if index < 0 {
		return nil, bindError("42704", "constraint does not exist", 0)
	}
	next, err := nextObjectDescriptor(ctx, descriptor, options, func(next *model.DescriptorSpec) error {
		position := constraintIndexByName(next.Table.Constraints, name)
		if position < 0 {
			return model.ErrInvalidDescriptor
		}
		next.Table.Constraints[position].Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: descriptor, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

func commentOnDomainConstraint(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	list := statement.Object.GetList()
	if list == nil || len(list.Items) != 2 || list.Items[0].GetTypeName() == nil {
		return nil, bindError("42601", "invalid COMMENT ON CONSTRAINT for domain", 0)
	}
	name := list.Items[1].GetString_()
	if name == nil || name.Sval == "" {
		return nil, bindError("42601", "invalid domain constraint name", 0)
	}
	parts, err := typeNameFromNode(list.Items[0])
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain {
		return nil, bindError("42809", "object is not a domain", 0)
	}
	if constraintIndexByName(spec.Type.Constraints, name.Sval) < 0 {
		return nil, bindError("42704", "domain constraint does not exist", 0)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		position := constraintIndexByName(next.Type.Constraints, name.Sval)
		if position < 0 {
			return model.ErrInvalidDescriptor
		}
		next.Type.Constraints[position].Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

func commentOnStatistics(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	parts, err := objectNodeParts(statement.Object)
	if err != nil {
		return nil, err
	}
	found, err := resolveStatistics(ctx, b, parts, options.Descriptors)
	if err != nil {
		return nil, err
	}
	next, err := mutateStatisticsDescriptor(ctx, found, options, func(statistic *model.ExtendedStatisticDescriptor, _ *model.DescriptorSpec) {
		statistic.Description = statement.Comment
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: found.table, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

func commentOnTextSearch(ctx context.Context, b *binder, statement *zpgparse.CommentStmt, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	kinds := []model.ObjectKind{model.ObjectTextSearchDictionary}
	if statement.Objtype == zpgparse.ObjectType_OBJECT_TSCONFIGURATION {
		kinds = []model.ObjectKind{model.ObjectTextSearchConfiguration}
	}
	parts, err := objectNodeParts(statement.Object)
	if err != nil {
		return nil, err
	}
	base, _, err := b.resolve(parts, kinds, "42704", 0)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	matched := spec.TextSearchDictionary != nil
	if statement.Objtype == zpgparse.ObjectType_OBJECT_TSCONFIGURATION {
		matched = spec.TextSearchConfiguration != nil
	}
	if !matched {
		return nil, bindError("42809", "object is not a text search dictionary or configuration", 0)
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "COMMENT",
	}, ctx.Err()
}

// nestedObjectTarget splits a COMMENT/DROP-style object list whose final item is
// a nested object name and whose leading items qualify its relation.
func nestedObjectTarget(node *zpgparse.Node) (string, *zpgparse.RangeVar, error) {
	list := node.GetList()
	if list == nil || len(list.Items) < 2 || len(list.Items) > 3 {
		return "", nil, bindError("42601", "invalid COMMENT ON object", 0)
	}
	items := list.Items
	last := items[len(items)-1].GetString_()
	if last == nil || last.Sval == "" {
		return "", nil, bindError("42601", "invalid nested object name", 0)
	}
	parts, err := names(items[:len(items)-1])
	if err != nil {
		return "", nil, err
	}
	relation := &zpgparse.RangeVar{Relname: parts[len(parts)-1]}
	if len(parts) == 2 {
		relation.Schemaname = parts[0]
	}
	return last.Sval, relation, nil
}

func constraintIndexByName(constraints []model.ConstraintDescriptor, name string) int {
	return slices.IndexFunc(constraints, func(constraint model.ConstraintDescriptor) bool {
		return !constraint.Mutation && constraint.Name == name
	})
}
