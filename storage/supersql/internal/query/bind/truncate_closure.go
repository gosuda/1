package bind

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type truncateForeignKeyEdge struct {
	child      model.ObjectID
	constraint model.ObjectID
	name       string
}

// BindTruncateClosure resolves the enforced incoming-foreign-key closure from
// one catalog snapshot. Explicit already reflects the statement's hierarchy
// semantics; this helper adds only FK-referencing tables.
func BindTruncateClosure(ctx context.Context, b *binder, snapshot CatalogSnapshot, explicit []*model.Descriptor, cascade bool, maximum int) ([]*model.Descriptor, error) {
	if b == nil || snapshot == nil || len(explicit) == 0 || maximum <= 0 || len(explicit) > maximum {
		return nil, model.ErrInvalidDescriptor
	}
	if b.dependencySet == nil {
		b.dependencySet = make(map[model.Dependency]struct{})
	}
	explicitIDs := make(map[model.ObjectID]*model.Descriptor, len(explicit))
	for _, descriptor := range explicit {
		if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectTable && descriptor.Kind() != model.ObjectMaterializedView {
			return nil, model.ErrInvalidDescriptor
		}
		if _, duplicate := explicitIDs[descriptor.ID()]; duplicate {
			return nil, model.ErrInvalidDescriptor
		}
		explicitIDs[descriptor.ID()] = descriptor
	}

	var visitedSchemas, visitedTables, visitedIndexes int
	admit := func(visited *int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *visited >= maximum {
			return bindError("54000", "TRUNCATE dependency scan limit exceeded", 0)
		}
		*visited++
		return nil
	}
	var schemas []*model.Descriptor
	schemaIDs := make(map[model.ObjectID]struct{})
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		if err := admit(&visitedSchemas); err != nil {
			return err
		}
		descriptor := published.Descriptor
		if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectSchema || descriptor.Name().Parent != 0 {
			return model.ErrInvalidDescriptor
		}
		if _, duplicate := schemaIDs[descriptor.ID()]; duplicate {
			return model.ErrInvalidDescriptor
		}
		schemaIDs[descriptor.ID()] = struct{}{}
		schemas = append(schemas, descriptor)
		return nil
	}); err != nil {
		return nil, err
	}
	slices.SortFunc(schemas, func(left, right *model.Descriptor) int { return cmp.Compare(left.ID(), right.ID()) })

	tables := make(map[model.ObjectID]*model.Descriptor)
	for _, schema := range schemas {
		if err := snapshot.ScanNamespace(ctx, schema.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if err := admit(&visitedTables); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectTable || descriptor.Name().Parent != schema.ID() {
				return model.ErrInvalidDescriptor
			}
			if _, duplicate := tables[descriptor.ID()]; duplicate {
				return model.ErrInvalidDescriptor
			}
			if _, ok := descriptor.Table(); !ok {
				return model.ErrInvalidDescriptor
			}
			tables[descriptor.ID()] = descriptor
			return nil
		}); err != nil {
			return nil, err
		}
	}

	for id := range explicitIDs {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil || published.Descriptor == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, err)
		}
		canonical := published.Descriptor
		provided := explicitIDs[id]
		if canonical.State() != model.StatePublic || canonical.Kind() != provided.Kind() || canonical.Version() != provided.Version() {
			return nil, model.ErrInvalidDescriptor
		}
		if canonical.Kind() == model.ObjectTable {
			if tables[id] == nil || tables[id].Version() != canonical.Version() {
				return nil, model.ErrInvalidDescriptor
			}
		} else {
			if _, ok := canonical.Table(); !ok {
				return nil, model.ErrInvalidDescriptor
			}
			tables[id] = canonical
		}
	}
	tableOrder := make([]*model.Descriptor, 0, len(tables))
	for _, descriptor := range tables {
		tableOrder = append(tableOrder, descriptor)
	}
	slices.SortFunc(tableOrder, func(left, right *model.Descriptor) int { return cmp.Compare(left.ID(), right.ID()) })
	for _, schema := range schemas {
		b.dependency(schema, 0)
	}
	for _, descriptor := range tableOrder {
		b.dependency(descriptor, 0)
	}


	edges := make(map[model.ObjectID][]truncateForeignKeyEdge)
	referencedIndexes := make(map[model.ObjectID]*model.Descriptor)
	for _, child := range tableOrder {
		metadata, ok := child.Table()
		if !ok {
			return nil, model.ErrInvalidDescriptor
		}
		for _, constraint := range metadata.Constraints {
			if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced {
				continue
			}
			parent := tables[constraint.ReferencedObject]
			if parent == nil || parent.Kind() != model.ObjectTable || parent.State() != model.StatePublic {
				published, err := snapshot.Descriptor(ctx, constraint.ReferencedObject)
				if err != nil || published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectTable {
					return nil, errors.Join(model.ErrInvalidDescriptor, err)
				}
				return nil, model.ErrInvalidDescriptor
			}
			parentMetadata, ok := parent.Table()
			if !ok {
				return nil, model.ErrInvalidDescriptor
			}
			position := slices.IndexFunc(parentMetadata.Constraints, func(candidate model.ConstraintDescriptor) bool {
				return candidate.ID == constraint.ReferencedConstraint
			})
			if position < 0 {
				return nil, model.ErrInvalidDescriptor
			}
			referenced := parentMetadata.Constraints[position]
			validReference := (referenced.Kind == model.ConstraintPrimaryKey || referenced.Kind == model.ConstraintUnique) &&
				referenced.Enforced && referenced.Validated && !referenced.Deferrable && referenced.Index == constraint.ReferencedIndex &&
				slices.Equal(referenced.Columns, constraint.ReferencedColumns)
			if !validReference {
				return nil, model.ErrInvalidDescriptor
			}
			if referencedIndexes[constraint.ReferencedIndex] == nil {
				if err := admit(&visitedIndexes); err != nil {
					return nil, err
				}
				published, err := snapshot.Descriptor(ctx, constraint.ReferencedIndex)
				if err != nil || published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectIndex {
					return nil, errors.Join(model.ErrInvalidDescriptor, err)
				}
				index, ok := published.Descriptor.Index()
				if !ok || index.Table != parent.ID() {
					return nil, model.ErrInvalidDescriptor
				}
				referencedIndexes[constraint.ReferencedIndex] = published.Descriptor
			}
			b.dependency(child, 0)
			b.dependency(parent, 0)
			for _, column := range constraint.Columns {
				b.dependency(child, column)
			}
			for _, column := range constraint.ReferencedColumns {
				b.dependency(parent, column)
			}
			edges[parent.ID()] = append(edges[parent.ID()], truncateForeignKeyEdge{child: child.ID(), constraint: constraint.ID, name: constraint.Name})
		}
	}
	indexOrder := make([]*model.Descriptor, 0, len(referencedIndexes))
	for _, index := range referencedIndexes {
		indexOrder = append(indexOrder, index)
	}
	slices.SortFunc(indexOrder, func(left, right *model.Descriptor) int { return cmp.Compare(left.ID(), right.ID()) })
	for _, index := range indexOrder {
		b.dependency(index, 0)
	}
	for parent := range edges {
		slices.SortFunc(edges[parent], func(left, right truncateForeignKeyEdge) int {
			if order := cmp.Compare(left.child, right.child); order != 0 {
				return order
			}
			if order := cmp.Compare(left.constraint, right.constraint); order != 0 {
				return order
			}
			return cmp.Compare(left.name, right.name)
		})
	}

	included := make(map[model.ObjectID]struct{}, len(explicitIDs))
	queue := make([]model.ObjectID, 0, len(explicitIDs))
	for id := range explicitIDs {
		included[id] = struct{}{}
		queue = append(queue, id)
	}
	slices.Sort(queue)
	for len(queue) != 0 {
		parentID := queue[0]
		queue = queue[1:]
		for _, edge := range edges[parentID] {
			if _, internal := included[edge.child]; internal {
				continue
			}
			if !cascade {
				parent, child := tables[parentID], tables[edge.child]
				return nil, bindError("2BP01", fmt.Sprintf("cannot truncate table %q because table %q references it through constraint %q", parent.Name().Name, child.Name().Name, edge.name), 0)
			}
			if len(included) >= maximum {
				return nil, bindError("54000", "TRUNCATE dependency closure limit exceeded", 0)
			}
			included[edge.child] = struct{}{}
			queue = append(queue, edge.child)
		}
	}

	result := make([]*model.Descriptor, 0, len(included))
	for id := range included {
		descriptor := tables[id]
		if descriptor == nil {
			return nil, model.ErrInvalidDescriptor
		}
		result = append(result, descriptor)
	}
	slices.SortFunc(result, func(left, right *model.Descriptor) int { return cmp.Compare(left.ID(), right.ID()) })
	return result, ctx.Err()
}
