package catalog

import (
	"cmp"
	"context"
	"errors"
	"math"
	"reflect"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type TypeUpdate struct {
	Base *model.Descriptor
	Next *model.Descriptor
}

type TypeChangeSet struct {
	Creates                []*model.Descriptor
	Updates                []TypeUpdate
	Drops                  []*model.Descriptor
	CascadeDrops           []*model.Descriptor
	TypedTableUpdates      []ObjectUpdate
	DependencyTableUpdates []ObjectUpdate
	TypedIndexUpdates      []ObjectUpdate
	TypedIndexDrops        []*model.Descriptor
	DependencyDrops        []*model.Descriptor
	Cascade                bool
	MaxDescriptors         uint32
}

type TypeDependencyCascade struct {
	TypedTableUpdates      []ObjectUpdate
	DependencyTableUpdates []ObjectUpdate
	IndexDrops             []*model.Descriptor
	ObjectDrops            []*model.Descriptor
	Dependencies           []txn.ReadDependency
}


func (s *Store) ApplyTypeChanges(ctx context.Context, snapshot *CatalogSnapshot, set TypeChangeSet) ([]Change, []txn.ReadDependency, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	if set.MaxDescriptors == 0 {
		return nil, nil, &model.Error{Code: "22023", Message: "type change descriptor limit is zero"}
	}
	dropCount := len(set.CascadeDrops)
	if len(set.Drops) == 0 {
		dropCount = 0
	}
	count := uint64(len(set.Creates)) + uint64(len(set.Updates)) + uint64(dropCount) + uint64(len(set.TypedTableUpdates)) + uint64(len(set.DependencyTableUpdates)) + uint64(len(set.TypedIndexUpdates)) + uint64(len(set.TypedIndexDrops)) + uint64(len(set.DependencyDrops))
	if count > uint64(set.MaxDescriptors) {
		return nil, nil, &model.Error{Code: "54000", Message: "type change descriptor limit exceeded"}
	}
	if err := s.validateCompositeTypeUpdates(ctx, snapshot, set); err != nil {
		return nil, nil, err
	}
	dependencies := make([]txn.ReadDependency, 0, count*3)
	if len(set.Drops) != 0 || len(set.Updates) != 0 || (set.Cascade && len(set.TypedTableUpdates) != 0) {
		scanDependencies, err := s.TypeCascadeScanDependencies(ctx, snapshot)
		if err != nil {
			return nil, nil, err
		}
		dependencies = append(dependencies, scanDependencies...)
	}
	changes := make([]Change, 0, count)
	for _, descriptor := range set.Creates {
		if descriptor == nil || (descriptor.Kind() != model.ObjectType && descriptor.Kind() != model.ObjectCompositeRelation) || descriptor.Version() != 1 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		reads, err := s.CreateDependencies(ctx, descriptor)
		if err != nil {
			return nil, nil, err
		}
		change, err := s.Create(ctx, snapshot, descriptor)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, reads...)
	}
	for _, update := range set.Updates {
		if update.Base == nil || update.Next == nil || (update.Base.Kind() != model.ObjectType && update.Base.Kind() != model.ObjectCompositeRelation) || update.Base.Kind() != update.Next.Kind() || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(update.Base.ID()))
	}
	for _, update := range set.TypedTableUpdates {
		if update.Base == nil || update.Next == nil || update.Base.Kind() != model.ObjectTable || update.Next.Kind() != model.ObjectTable || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(update.Base.ID()))
	}
	for _, update := range set.DependencyTableUpdates {
		if update.Base == nil || update.Next == nil || update.Base.Kind() != model.ObjectTable || update.Next.Kind() != model.ObjectTable || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 || update.Base.Physical() != update.Next.Physical() {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(update.Base.ID()))
	}
	for _, update := range set.TypedIndexUpdates {
		if update.Base == nil || update.Next == nil || update.Base.Kind() != model.ObjectIndex || update.Next.Kind() != model.ObjectIndex || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(update.Base.ID()))
	}
	for _, descriptor := range set.TypedIndexDrops {
		if descriptor == nil || descriptor.Kind() != model.ObjectIndex {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Drop(ctx, snapshot, descriptor.ID(), descriptor.Version())
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(descriptor.ID()))
	}
	for _, descriptor := range set.DependencyDrops {
		if descriptor == nil {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Drop(ctx, snapshot, descriptor.ID(), descriptor.Version())
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(descriptor.ID()))
	}
	ordered, err := s.TypeDropOrder(ctx, snapshot, set.Drops, set.Cascade, set.MaxDescriptors)
	if err != nil {
		return nil, nil, err
	}
	if len(ordered) != len(set.CascadeDrops) {
		return nil, nil, model.ErrInvalidDescriptor
	}
	for i, descriptor := range ordered {
		planned := set.CascadeDrops[i]
		if planned == nil || descriptor.ID() != planned.ID() || descriptor.Version() != planned.Version() {
			return nil, nil, model.ErrInvalidDescriptor
		}
	}
	for _, descriptor := range ordered {
		change, err := s.Drop(ctx, snapshot, descriptor.ID(), descriptor.Version())
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(descriptor.ID()))
	}
	return changes, dependencies, ctx.Err()
}

type CompositeTypeDependent struct {
	Table  *model.Descriptor
	Before *model.Descriptor
	After  *model.Descriptor
	Parents []model.ObjectID
}

func (s *Store) CompositeTypeDependents(ctx context.Context, snapshot *CatalogSnapshot, updates []TypeUpdate, maximum uint32) ([]CompositeTypeDependent, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, err
	}
	if maximum == 0 {
		return nil, &model.Error{Code: "22023", Message: "type dependency descriptor limit is zero"}
	}
	byObject := make(map[model.ObjectID]TypeUpdate)
	byOID := make(map[model.TypeID]TypeUpdate)
	for _, update := range updates {
		if update.Base == nil || update.Next == nil {
			continue
		}
		before, beforeOK := update.Base.DeclaredType()
		after, afterOK := update.Next.DeclaredType()
		if !beforeOK || !afterOK || before.DeclaredKind() != model.TypeKindComposite || sameCompositeTypeDeclaration(before, after) {
			continue
		}
		byObject[update.Base.ID()] = update
		byOID[model.TypeID(update.Base.OID())] = update
	}
	if len(byObject) == 0 {
		return nil, nil
	}
	tables := make([]*model.Descriptor, 0)
	err := scanDescriptors(ctx, snapshot, maximum, catalogScanKinds, "type dependency scan limit exceeded", func(descriptor *model.Descriptor) error {
		if descriptor == nil {
			return model.ErrInvalidDescriptor
		}
		if descriptor.Kind() != model.ObjectTable && descriptor.Kind() != model.ObjectMaterializedView {
			return nil
		}
		spec, err := descriptor.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil {
			return err
		}
		if spec.Table == nil {
			return model.ErrInvalidDescriptor
		}
		for _, column := range spec.Table.Columns {
			affected, err := s.layoutReferencesCompositeChange(ctx, snapshot, column.Layout, byOID, make(map[model.TypeID]bool), 0)
			if err != nil {
				return err
			}
			if affected {
				return &model.Error{Code: "2BP01", Message: "cannot alter type because a table column uses it"}
			}
		}
		tables = append(tables, descriptor)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(tables, func(left, right *model.Descriptor) int {
		switch {
		case left.ID() < right.ID():
			return -1
		case left.ID() > right.ID():
			return 1
		default:
			return 0
		}
	})
	dependents := make([]CompositeTypeDependent, 0)
	roots := make(map[model.ObjectID]TypeUpdate)
	remaining := make(map[model.ObjectID]*model.Descriptor)
	for _, table := range tables {
		spec, err := table.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil {
			return nil, err
		}
		if update, typed := byObject[spec.Table.TypedOf]; typed {
			roots[table.ID()] = update
			dependents = append(dependents, CompositeTypeDependent{Table: table, Before: update.Base, After: update.Next})
		} else {
			remaining[table.ID()] = table
		}
	}
	for {
		var layer []CompositeTypeDependent
		for _, table := range tables {
			if remaining[table.ID()] == nil {
				continue
			}
			spec, err := table.SpecWithin(ctx, s.limits.Descriptor)
			if err != nil {
				return nil, err
			}
			var parents []model.ObjectID
			var root TypeUpdate
			for _, parent := range spec.Table.Parents {
				update, affected := roots[parent]
				if !affected {
					continue
				}
				if root.Base != nil && root.Base.ID() != update.Base.ID() {
					return nil, model.ErrInvalidDescriptor
				}
				root = update
				parents = append(parents, parent)
			}
			if len(parents) == 0 {
				continue
			}
			slices.Sort(parents)
			layer = append(layer, CompositeTypeDependent{Table: table, Before: root.Base, After: root.Next, Parents: parents})
		}
		if len(layer) == 0 {
			break
		}
		slices.SortFunc(layer, func(left, right CompositeTypeDependent) int {
			switch {
			case left.Table.ID() < right.Table.ID():
				return -1
			case left.Table.ID() > right.Table.ID():
				return 1
			default:
				return 0
			}
		})
		for _, dependent := range layer {
			if uint64(len(dependents)) >= uint64(maximum) {
				return nil, &model.Error{Code: "54000", Message: "typed table cascade descriptor limit exceeded"}
			}
			dependents = append(dependents, dependent)
			roots[dependent.Table.ID()] = TypeUpdate{Base: dependent.Before, Next: dependent.After}
			delete(remaining, dependent.Table.ID())
		}
	}
	if uint64(len(dependents)) > uint64(maximum) {
		return nil, &model.Error{Code: "54000", Message: "typed table cascade descriptor limit exceeded"}
	}
	return dependents, nil
}

type DomainColumnReference struct {
	Table   *model.Descriptor
	Columns []model.ColumnDescriptor
}

func (s *Store) DomainColumnReferences(ctx context.Context, snapshot *CatalogSnapshot, domain model.TypeID, maximum uint32) ([]DomainColumnReference, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, err
	}
	if domain == 0 || maximum == 0 {
		return nil, &model.Error{Code: "22023", Message: "invalid domain validation target"}
	}
	targets := map[model.TypeID]TypeUpdate{domain: {}}
	var references []DomainColumnReference
	err := scanDescriptors(ctx, snapshot, maximum, catalogScanKinds, "type dependency scan limit exceeded", func(descriptor *model.Descriptor) error {
		if descriptor == nil || descriptor.Kind() != model.ObjectTable && descriptor.Kind() != model.ObjectMaterializedView {
			return nil
		}
		spec, err := descriptor.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil || spec.Table == nil {
			return errors.Join(model.ErrInvalidDescriptor, err)
		}
		var columns []model.ColumnDescriptor
		for _, column := range spec.Table.Columns {
			affected, err := s.layoutReferencesCompositeChange(ctx, snapshot, column.Layout, targets, make(map[model.TypeID]bool), 0)
			if err != nil {
				return err
			}
			if affected {
				columns = append(columns, column)
			}
		}
		if len(columns) != 0 {
			if uint64(len(references)) >= uint64(maximum) {
				return &model.Error{Code: "54000", Message: "domain validation descriptor limit exceeded"}
			}
			references = append(references, DomainColumnReference{Table: descriptor, Columns: columns})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(references, func(left, right DomainColumnReference) int {
		switch {
		case left.Table.ID() < right.Table.ID():
			return -1
		case left.Table.ID() > right.Table.ID():
			return 1
		default:
			return 0
		}
	})
	return references, nil
}

func sameCompositeTypeDeclaration(left, right model.TypeMetadata) bool {
	if len(left.Fields) != len(right.Fields) {
		return false
	}
	for i := range left.Fields {
		a, b := left.Fields[i], right.Fields[i]
		if a.ID != b.ID || a.Name != b.Name || !sameTypeServiceLayout(a.Layout, b.Layout) {
			return false
		}
	}
	return true
}

func (s *Store) layoutReferencesCompositeChange(ctx context.Context, snapshot *CatalogSnapshot, layout model.TypeLayout, targets map[model.TypeID]TypeUpdate, visiting map[model.TypeID]bool, depth int) (bool, error) {
	if _, affected := targets[layout.Type.ID]; affected {
		return true, nil
	}
	if depth >= s.limits.Descriptor.MaxDepth {
		return false, model.ErrDescriptorLimit
	}
	for _, child := range layout.Children {
		affected, err := s.layoutReferencesCompositeChange(ctx, snapshot, child, targets, visiting, depth+1)
		if affected || err != nil {
			return affected, err
		}
	}
	id := layout.Type.ID
	if id == 0 || visiting[id] {
		return false, nil
	}
	published, err := snapshot.ResolveOID(ctx, model.OID(id))
	if errors.Is(err, ErrUnknownObject) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	metadata, ok := published.Descriptor.DeclaredType()
	if !ok {
		return false, nil
	}
	visiting[id] = true
	defer delete(visiting, id)
	var nested []model.TypeLayout
	switch metadata.DeclaredKind() {
	case model.TypeKindDomain, model.TypeKindArray:
		nested = []model.TypeLayout{metadata.BaseLayout}
	case model.TypeKindComposite:
		nested = make([]model.TypeLayout, len(metadata.Fields))
		for i := range metadata.Fields {
			nested[i] = metadata.Fields[i].Layout
		}
	case model.TypeKindRange, model.TypeKindMultirange:
		if metadata.Range != nil {
			nested = []model.TypeLayout{metadata.Range.Subtype}
		}
	}
	for _, child := range nested {
		affected, err := s.layoutReferencesCompositeChange(ctx, snapshot, child, targets, visiting, depth+1)
		if affected || err != nil {
			return affected, err
		}
	}
	return false, nil
}

type typeCascadeDependencyKey struct {
	object model.ObjectID
	column model.ColumnID
}

func (s *Store) PlanTypeDependencyCascade(ctx context.Context, snapshot *CatalogSnapshot, typed []ObjectUpdate, indexUpdates []ObjectUpdate, indexes []*model.Descriptor, maximum uint32) (TypeDependencyCascade, error) {
	result := TypeDependencyCascade{TypedTableUpdates: slices.Clone(typed)}
	if err := s.checkSnapshot(snapshot); err != nil {
		return result, err
	}
	if maximum == 0 {
		return result, &model.Error{Code: "22023", Message: "type dependency descriptor limit is zero"}
	}
	targets := make(map[model.ObjectID]*model.Descriptor, len(typed))
	affectedColumns := make(map[model.ObjectID]map[model.ColumnID]bool)
	targetVersions := make(map[model.ObjectID]uint64, len(typed)+len(indexUpdates))
	droppedConstraints := make(map[model.ObjectID]bool)
	droppedIndexes := make(map[model.ObjectID]*model.Descriptor)
	for _, descriptor := range indexes {
		if descriptor == nil || descriptor.Kind() != model.ObjectIndex {
			return result, model.ErrInvalidDescriptor
		}
		droppedIndexes[descriptor.ID()] = descriptor
	}
	for _, update := range indexUpdates {
		if update.Base == nil || update.Next == nil || update.Base.Kind() != model.ObjectIndex || update.Next.Kind() != model.ObjectIndex || update.Base.ID() != update.Next.ID() {
			return result, model.ErrInvalidDescriptor
		}
		targetVersions[update.Base.ID()] = update.Next.Version()
	}
	for _, update := range typed {
		if update.Base == nil || update.Next == nil || update.Base.Kind() != model.ObjectTable || update.Next.Kind() != model.ObjectTable || update.Base.ID() != update.Next.ID() {
			return result, model.ErrInvalidDescriptor
		}
		base, baseErr := update.Base.SpecWithin(ctx, s.limits.Descriptor)
		next, nextErr := update.Next.SpecWithin(ctx, s.limits.Descriptor)
		if baseErr != nil || nextErr != nil || base.Table == nil || next.Table == nil {
			return result, errors.Join(model.ErrInvalidDescriptor, baseErr, nextErr)
		}
		targets[update.Base.ID()] = update.Next
		targetVersions[update.Base.ID()] = update.Next.Version()
		nextColumns := make(map[model.ColumnID]model.ColumnDescriptor, len(next.Table.Columns))
		for _, column := range next.Table.Columns {
			nextColumns[column.ID] = column
		}
		for _, column := range base.Table.Columns {
			nextColumn, exists := nextColumns[column.ID]
			if exists && sameTypeServiceLayout(column.Layout, nextColumn.Layout) {
				continue
			}
			if affectedColumns[update.Base.ID()] == nil {
				affectedColumns[update.Base.ID()] = make(map[model.ColumnID]bool)
			}
			affectedColumns[update.Base.ID()][column.ID] = true
		}
		nextConstraints := make(map[model.ObjectID]bool, len(next.Table.Constraints))
		for _, constraint := range next.Table.Constraints {
			nextConstraints[constraint.ID] = true
		}
		for _, constraint := range base.Table.Constraints {
			if !nextConstraints[constraint.ID] {
				droppedConstraints[constraint.ID] = true
				if constraint.Index != 0 {
					droppedIndexes[constraint.Index] = nil
				}
			}
		}
	}
	if len(affectedColumns) == 0 && len(droppedIndexes) == 0 && len(targetVersions) == 0 {
		return result, nil
	}
	dependencies, err := s.TypeCascadeScanDependencies(ctx, snapshot)
	if err != nil {
		return result, err
	}
	result.Dependencies = dependencies
	descriptors := make(map[model.ObjectID]*model.Descriptor)
	var tables []*model.Descriptor
	if err := scanDescriptors(ctx, snapshot, maximum, objectScanKinds, "object dependency scan limit exceeded", func(descriptor *model.Descriptor) error {
		if descriptor == nil {
			return model.ErrInvalidDescriptor
		}
		descriptors[descriptor.ID()] = descriptor
		if descriptor.Kind() == model.ObjectTable || descriptor.Kind() == model.ObjectMaterializedView {
			tables = append(tables, descriptor)
		}
		return nil
	}); err != nil {
		return result, err
	}
	for id, descriptor := range droppedIndexes {
		if descriptor != nil {
			continue
		}
		index := descriptors[id]
		if index == nil || index.Kind() != model.ObjectIndex {
			return result, model.ErrInvalidDescriptor
		}
		droppedIndexes[id] = index
	}
	for changed := true; changed; {
		changed = false
		for _, table := range tables {
			effective := table
			if target := targets[table.ID()]; target != nil {
				effective = target
			}
			spec, err := effective.SpecWithin(ctx, s.limits.Descriptor)
			if err != nil || spec.Table == nil {
				return result, errors.Join(model.ErrInvalidDescriptor, err)
			}
			for _, constraint := range spec.Table.Constraints {
				if droppedConstraints[constraint.ID] || !typeCascadeConstraintAffected(constraint, spec.ID, affectedColumns, droppedConstraints, droppedIndexes) {
					continue
				}
				droppedConstraints[constraint.ID] = true
				changed = true
				if constraint.Index != 0 {
					if _, known := droppedIndexes[constraint.Index]; !known {
						droppedIndexes[constraint.Index] = nil
						changed = true
					}
				}
			}
		}
		for id, descriptor := range droppedIndexes {
			if descriptor != nil {
				continue
			}
			index := descriptors[id]
			if index == nil || index.Kind() != model.ObjectIndex {
				return result, model.ErrInvalidDescriptor
			}
			droppedIndexes[id] = index
		}
	}
	adjusted := make(map[model.ObjectID]*model.Descriptor, len(targets))
	for id, target := range targets {
		adjusted[id] = target
	}
	for _, table := range tables {
		if table.Kind() != model.ObjectTable {
			continue
		}
		effective := table
		typedTarget := false
		if target := targets[table.ID()]; target != nil {
			effective, typedTarget = target, true
		}
		spec, err := effective.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil || spec.Table == nil {
			return result, errors.Join(model.ErrInvalidDescriptor, err)
		}
		candidates := make(map[typeCascadeDependencyKey]bool)
		changed := false
		keptConstraints := spec.Table.Constraints[:0]
		for _, constraint := range spec.Table.Constraints {
			if !droppedConstraints[constraint.ID] {
				keptConstraints = append(keptConstraints, constraint)
				continue
			}
			changed = true
			typeCascadeConstraintCandidates(candidates, constraint)
		}
		spec.Table.Constraints = keptConstraints
		keptTriggers := spec.Table.Triggers[:0]
		for _, trigger := range spec.Table.Triggers {
			if !droppedConstraints[trigger.ConstraintID] && droppedIndexes[trigger.ConstraintIndex] == nil {
				keptTriggers = append(keptTriggers, trigger)
				continue
			}
			changed = true
			typeCascadeTriggerCandidates(candidates, trigger)
		}
		spec.Table.Triggers = keptTriggers
		keptIndexes := spec.Table.Indexes[:0]
		for _, index := range spec.Table.Indexes {
			if _, drop := droppedIndexes[index]; !drop {
				keptIndexes = append(keptIndexes, index)
				continue
			}
			changed = true
			candidates[typeCascadeDependencyKey{object: index}] = true
		}
		spec.Table.Indexes = keptIndexes
		for object, columns := range affectedColumns {
			for column := range columns {
				candidates[typeCascadeDependencyKey{object: object, column: column}] = true
			}
			candidates[typeCascadeDependencyKey{object: object}] = true
		}
		for i := range spec.Dependencies {
			if version, ok := targetVersions[spec.Dependencies[i].Object]; ok && spec.Dependencies[i].Version != version {
				spec.Dependencies[i].Version = version
				changed = true
			}
		}
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			key := typeCascadeDependencyKey{object: dependency.Object, column: dependency.Column}
			if !candidates[key] || typeCascadeTableUsesDependency(spec.Table, dependency) {
				return false
			}
			changed = true
			return true
		})
		if !changed {
			continue
		}
		if !typedTarget {
			if spec.Version == math.MaxUint64 {
				return result, model.ErrDescriptorLimit
			}
			spec.Version++
		}
		next, err := model.NewDescriptor(ctx, spec, s.limits.Descriptor)
		if err != nil {
			return result, err
		}
		adjusted[table.ID()] = next
		if !typedTarget {
			result.DependencyTableUpdates = append(result.DependencyTableUpdates, ObjectUpdate{Base: table, Next: next})
		}
	}
	for i := range result.TypedTableUpdates {
		if target := adjusted[result.TypedTableUpdates[i].Base.ID()]; target != nil {
			result.TypedTableUpdates[i].Next = target
		}
	}
	for id, descriptor := range droppedIndexes {
		if descriptor == nil {
			descriptor = descriptors[id]
		}
		if descriptor == nil || descriptor.Kind() != model.ObjectIndex {
			return result, model.ErrInvalidDescriptor
		}
		result.IndexDrops = append(result.IndexDrops, descriptor)
	}
	slices.SortFunc(result.IndexDrops, func(left, right *model.Descriptor) int {
		return cmp.Compare(left.ID(), right.ID())
	})
	dropTargets := make(map[model.ObjectID]*model.Descriptor)
	for _, descriptor := range descriptors {
		if descriptor == nil || droppedIndexes[descriptor.ID()] != nil || targets[descriptor.ID()] != nil {
			continue
		}
		effective := descriptor
		if target := adjusted[descriptor.ID()]; target != nil {
			effective = target
		}
		spec, err := effective.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil {
			return result, err
		}
		if slices.ContainsFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			return typeCascadeDependencyAffected(dependency, affectedColumns, droppedIndexes)
		}) {
			dropTargets[descriptor.ID()] = descriptor
		}
	}
	for changed := true; changed; {
		changed = false
		for _, descriptor := range descriptors {
			if descriptor == nil || dropTargets[descriptor.ID()] != nil || targets[descriptor.ID()] != nil {
				continue
			}
			effective := descriptor
			if target := adjusted[descriptor.ID()]; target != nil {
				effective = target
			}
			spec, err := effective.SpecWithin(ctx, s.limits.Descriptor)
			if err != nil {
				return result, err
			}
			if !slices.ContainsFunc(spec.Dependencies, func(dependency model.Dependency) bool { return dropTargets[dependency.Object] != nil }) {
				continue
			}
			if uint64(len(dropTargets)) >= uint64(maximum) {
				return result, &model.Error{Code: "54000", Message: "type dependency cascade descriptor limit exceeded"}
			}
			dropTargets[descriptor.ID()] = descriptor
			changed = true
		}
	}
	slices.SortFunc(result.DependencyTableUpdates, func(left, right ObjectUpdate) int {
		return cmp.Compare(left.Base.ID(), right.Base.ID())
	})
	result.DependencyTableUpdates = slices.DeleteFunc(result.DependencyTableUpdates, func(update ObjectUpdate) bool {
		return dropTargets[update.Base.ID()] != nil
	})
	for _, descriptor := range result.IndexDrops {
		if dropTargets[descriptor.ID()] != nil {
			delete(droppedIndexes, descriptor.ID())
		}
	}
	result.IndexDrops = slices.DeleteFunc(result.IndexDrops, func(descriptor *model.Descriptor) bool {
		return dropTargets[descriptor.ID()] != nil
	})
	ordered, err := typeCascadeDropOrder(ctx, dropTargets, s.limits.Descriptor)
	if err != nil {
		return result, err
	}
	result.ObjectDrops = ordered
	if uint64(len(result.TypedTableUpdates))+uint64(len(result.DependencyTableUpdates))+uint64(len(result.IndexDrops))+uint64(len(result.ObjectDrops)) > uint64(maximum) {
		return result, &model.Error{Code: "54000", Message: "type dependency cascade descriptor limit exceeded"}
	}
	return result, ctx.Err()
}

// TypeCascadeScanDependencies returns one read dependency per distinct
// namespace object reachable from the schema root, plus the namespace scan
// spans that protect the walk.
//
// The walk is bounded by the population it enumerates. A counting pre-pass
// over the same scans runs first and streams rows without retaining them; its
// row count is the largest number of distinct ids the walk can admit, so it
// is the derived allowance. That allowance is checked against the store's
// configured descriptor item ceiling (Limits.Descriptor.MaxItems, the same
// limit that bounds every descriptor collection and that NewDeclaredTypeResolver
// uses to bound its population). A catalog larger than that ceiling fails with
// 54000 before any dependency is retained, so a pathological namespace cannot
// exhaust memory. The caller's per-statement descriptor budget is deliberately
// not charged: it bounds the statement's mutations, not the catalog it must scan.
func (s *Store) TypeCascadeScanDependencies(ctx context.Context, snapshot *CatalogSnapshot) ([]txn.ReadDependency, error) {
	kinds := typeCascadeKinds
	population, err := s.typeCascadePopulation(ctx, snapshot, kinds)
	if err != nil {
		return nil, err
	}
	if population > uint64(s.limits.Descriptor.MaxItems) {
		return nil, &model.Error{Code: "54000", Message: "type dependency namespace limit exceeded"}
	}
	root, err := s.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return nil, err
	}
	dependencies := make([]txn.ReadDependency, 0, 1+population)
	dependencies = append(dependencies, root)
	seen := make(map[model.ObjectID]bool, population)
	addDescriptor := func(id model.ObjectID) {
		if seen[id] {
			return
		}
		seen[id] = true
		dependencies = append(dependencies, s.DescriptorDependency(id))
	}
	err = snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference NamespaceReference) error {
		addDescriptor(reference.ID)
		for _, kind := range kinds {
			dependency, dependencyErr := s.NamespaceScanDependency(ctx, reference.ID, kind)
			if dependencyErr != nil {
				return dependencyErr
			}
			dependencies = append(dependencies, dependency)
			if err := snapshot.ScanNamespaceReferences(ctx, reference.ID, kind, func(object NamespaceReference) error {
				addDescriptor(object.ID)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dependencies, nil
}

// typeCascadeKinds is the per-schema namespace kind inventory the cascade walk
// scans. It is shared by the population pre-pass and the walk so both count the
// same rows.
var typeCascadeKinds = []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectType, model.ObjectCompositeRelation, model.ObjectTextSearchDictionary, model.ObjectTextSearchConfiguration, model.ObjectTextSearchTemplate, model.ObjectCast, model.ObjectOperator}

// typeCascadePopulation counts, without retaining, the namespace rows the
// cascade walk will visit: the schema rows plus every child row under each
// schema for each kind. Each row is counted once per scan and the count is
// overflow-checked, so the result is a safe upper bound on the distinct ids the
// walk can admit.
func (s *Store) typeCascadePopulation(ctx context.Context, snapshot *CatalogSnapshot, kinds []model.ObjectKind) (uint64, error) {
	var count uint64
	add := func() error {
		if count == math.MaxUint64 {
			return &model.Error{Code: "54000", Message: "type dependency namespace limit exceeded"}
		}
		count++
		return nil
	}
	err := snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(schema NamespaceReference) error {
		if err := add(); err != nil {
			return err
		}
		for _, kind := range kinds {
			if err := snapshot.ScanNamespaceReferences(ctx, schema.ID, kind, func(NamespaceReference) error { return add() }); err != nil {
				return err
			}
		}
		return nil
	})
	return count, err
}

func typeCascadeConstraintAffected(constraint model.ConstraintDescriptor, table model.ObjectID, columns map[model.ObjectID]map[model.ColumnID]bool, constraints map[model.ObjectID]bool, indexes map[model.ObjectID]*model.Descriptor) bool {
	if local := columns[table]; local != nil && slices.ContainsFunc(constraint.Columns, func(column model.ColumnID) bool { return local[column] }) {
		return true
	}
	if referenced := columns[constraint.ReferencedObject]; referenced != nil && slices.ContainsFunc(constraint.ReferencedColumns, func(column model.ColumnID) bool { return referenced[column] }) {
		return true
	}
	if constraint.Expression != nil && slices.ContainsFunc(constraint.Expression.Dependencies, func(dependency model.Dependency) bool {
		return typeCascadeDependencyAffected(dependency, columns, indexes)
	}) {
		return true
	}
	if constraints[constraint.ReferencedConstraint] || constraints[constraint.ParentConstraint] {
		return true
	}
	_, referencedIndex := indexes[constraint.ReferencedIndex]
	_, ownedIndex := indexes[constraint.Index]
	return referencedIndex || ownedIndex
}

func typeCascadeDependencyAffected(dependency model.Dependency, columns map[model.ObjectID]map[model.ColumnID]bool, indexes map[model.ObjectID]*model.Descriptor) bool {
	if _, dropped := indexes[dependency.Object]; dropped {
		return true
	}
	return dependency.Column != 0 && columns[dependency.Object][dependency.Column]
}

func typeCascadeConstraintCandidates(candidates map[typeCascadeDependencyKey]bool, constraint model.ConstraintDescriptor) {
	if constraint.ReferencedObject != 0 {
		candidates[typeCascadeDependencyKey{object: constraint.ReferencedObject}] = true
		for _, column := range constraint.ReferencedColumns {
			candidates[typeCascadeDependencyKey{object: constraint.ReferencedObject, column: column}] = true
		}
	}
	for _, object := range []model.ObjectID{constraint.ReferencedIndex, constraint.Index, constraint.ParentTable} {
		if object != 0 {
			candidates[typeCascadeDependencyKey{object: object}] = true
		}
	}
	if constraint.Expression != nil {
		for _, dependency := range constraint.Expression.Dependencies {
			candidates[typeCascadeDependencyKey{object: dependency.Object, column: dependency.Column}] = true
		}
	}
}

func typeCascadeTriggerCandidates(candidates map[typeCascadeDependencyKey]bool, trigger model.TriggerDescriptor) {
	for _, object := range []model.ObjectID{trigger.Routine, trigger.ConstraintTable, trigger.ConstraintRelation, trigger.ConstraintIndex} {
		if object != 0 {
			candidates[typeCascadeDependencyKey{object: object}] = true
		}
	}
	if trigger.When != nil {
		for _, dependency := range trigger.When.Dependencies {
			candidates[typeCascadeDependencyKey{object: dependency.Object, column: dependency.Column}] = true
		}
	}
}

func typeCascadeTableUsesDependency(table *model.TableMetadata, dependency model.Dependency) bool {
	if table == nil {
		return false
	}
	usesExpression := func(expression *model.ExpressionDefinition) bool {
		return expression != nil && slices.ContainsFunc(expression.Dependencies, func(candidate model.Dependency) bool {
			return candidate.Object == dependency.Object && candidate.Column == dependency.Column
		})
	}
	if dependency.Column == 0 {
		if table.TypedOf == dependency.Object || table.RowType == dependency.Object || table.PartitionDefault == dependency.Object || slices.Contains(table.Parents, dependency.Object) || slices.Contains(table.Indexes, dependency.Object) {
			return true
		}
		for _, column := range table.Columns {
			if column.IdentitySequence == dependency.Object {
				return true
			}
		}
	}
	for _, column := range table.Columns {
		for _, inherited := range column.InheritedFrom {
			if inherited.Table == dependency.Object && (dependency.Column == 0 || inherited.Column == dependency.Column) {
				return true
			}
		}
		if usesExpression(column.Default) || usesExpression(column.Generated) {
			return true
		}
	}
	if usesExpression(table.Partition) {
		return true
	}
	for _, key := range append(slices.Clone(table.PrimaryKey), table.PartitionKeys...) {
		if usesExpression(key.Expression) {
			return true
		}
	}
	for _, constraint := range table.Constraints {
		if constraint.ReferencedObject == dependency.Object && (dependency.Column == 0 || slices.Contains(constraint.ReferencedColumns, dependency.Column)) {
			return true
		}
		if dependency.Column == 0 && (constraint.ReferencedIndex == dependency.Object || constraint.Index == dependency.Object || constraint.ParentTable == dependency.Object) {
			return true
		}
		if usesExpression(constraint.Expression) {
			return true
		}
	}
	for _, trigger := range table.Triggers {
		if dependency.Column == 0 && (trigger.Routine == dependency.Object || trigger.ConstraintTable == dependency.Object || trigger.ConstraintRelation == dependency.Object || trigger.ConstraintIndex == dependency.Object) {
			return true
		}
		if usesExpression(trigger.When) {
			return true
		}
	}
	for _, statistic := range table.ExtendedStatistics {
		for _, expression := range statistic.Expressions {
			if usesExpression(expression) {
				return true
			}
		}
	}
	return false
}

func typeCascadeDropOrder(ctx context.Context, targets map[model.ObjectID]*model.Descriptor, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	ids := make([]model.ObjectID, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	visiting := make(map[model.ObjectID]bool, len(ids))
	visited := make(map[model.ObjectID]bool, len(ids))
	ordered := make([]*model.Descriptor, 0, len(ids))
	var visit func(model.ObjectID) error
	visit = func(id model.ObjectID) error {
		if visited[id] {
			return nil
		}
		if visiting[id] {
			return &model.Error{Code: "2BP01", Message: "type dependency cycle prevents cascade"}
		}
		visiting[id] = true
		for _, candidateID := range ids {
			if candidateID == id {
				continue
			}
			spec, err := targets[candidateID].SpecWithin(ctx, limits)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(spec.Dependencies, func(dependency model.Dependency) bool { return dependency.Object == id }) {
				if err := visit(candidateID); err != nil {
					return err
				}
			}
		}
		visiting[id] = false
		visited[id] = true
		ordered = append(ordered, targets[id])
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func (s *Store) validateCompositeTypeUpdates(ctx context.Context, snapshot *CatalogSnapshot, set TypeChangeSet) error {
	dependents, err := s.CompositeTypeDependents(ctx, snapshot, set.Updates, set.MaxDescriptors)
	if err != nil {
		return err
	}
	if len(dependents) == 0 {
		if len(set.TypedTableUpdates) != 0 || len(set.DependencyTableUpdates) != 0 || len(set.TypedIndexUpdates) != 0 || len(set.TypedIndexDrops) != 0 || len(set.DependencyDrops) != 0 {
			return model.ErrInvalidDescriptor
		}
		return nil
	}
	if !set.Cascade {
		return &model.Error{Code: "2BP01", Message: "cannot alter type because it is the type of a typed table"}
	}
	expected := make(map[model.ObjectID]CompositeTypeDependent, len(dependents))
	for _, dependent := range dependents {
		expected[dependent.Table.ID()] = dependent
	}
	targets := make(map[model.ObjectID]*model.Descriptor, len(set.TypedTableUpdates))
	for _, update := range set.TypedTableUpdates {
		if update.Base == nil || update.Next == nil || targets[update.Base.ID()] != nil {
			return model.ErrInvalidDescriptor
		}
		targets[update.Base.ID()] = update.Next
	}
	for _, update := range set.TypedTableUpdates {
		dependent, ok := expected[update.Base.ID()]
		if !ok || update.Base.Version() != dependent.Table.Version() {
			return model.ErrInvalidDescriptor
		}
		if len(dependent.Parents) == 0 {
			if !typedTableMatchesComposite(ctx, update.Next, dependent.After, s.limits.Descriptor) {
				return model.ErrInvalidDescriptor
			}
		} else if !s.inheritedTableMatchesParents(ctx, snapshot, update.Next, dependent.Parents, targets) {
			return model.ErrInvalidDescriptor
		}
		delete(expected, update.Base.ID())
	}
	if len(expected) != 0 {
		return &model.Error{Code: "0A000", Message: "typed table cascade was not staged"}
	}
	planned, err := s.PlanTypeDependencyCascade(ctx, snapshot, set.TypedTableUpdates, set.TypedIndexUpdates, set.TypedIndexDrops, set.MaxDescriptors)
	if err != nil {
		return err
	}
	if !sameTypeDependencyCascade(ctx, planned, set, s.limits.Descriptor) {
		return model.ErrInvalidDescriptor
	}
	expectedUpdates := make(map[model.ObjectID]*model.Descriptor)
	expectedDrops := make(map[model.ObjectID]*model.Descriptor)
	for _, update := range set.TypedTableUpdates {
		baseSpec, baseErr := update.Base.SpecWithin(ctx, s.limits.Descriptor)
		nextSpec, nextErr := update.Next.SpecWithin(ctx, s.limits.Descriptor)
		if baseErr != nil || nextErr != nil || baseSpec.Table == nil || nextSpec.Table == nil {
			return model.ErrInvalidDescriptor
		}
		nextIndexes := make(map[model.ObjectID]bool, len(nextSpec.Table.Indexes))
		for _, index := range nextSpec.Table.Indexes {
			nextIndexes[index] = true
		}
		for _, index := range baseSpec.Table.Indexes {
			if !nextIndexes[index] {
				expectedDrops[index] = update.Next
				continue
			}
			if update.Base.Physical().Generation != update.Next.Physical().Generation {
				expectedUpdates[index] = update.Next
			}
			delete(nextIndexes, index)
		}
		if len(nextIndexes) != 0 {
			return model.ErrInvalidDescriptor
		}
	}
	for _, update := range set.DependencyTableUpdates {
		if update.Base == nil || update.Next == nil || targets[update.Base.ID()] != nil {
			return model.ErrInvalidDescriptor
		}
		baseSpec, baseErr := update.Base.SpecWithin(ctx, s.limits.Descriptor)
		nextSpec, nextErr := update.Next.SpecWithin(ctx, s.limits.Descriptor)
		if baseErr != nil || nextErr != nil || baseSpec.Table == nil || nextSpec.Table == nil {
			return model.ErrInvalidDescriptor
		}
		allowed := baseSpec
		allowed.Version = nextSpec.Version
		allowed.Dependencies = nextSpec.Dependencies
		allowed.Table.Constraints = nextSpec.Table.Constraints
		allowed.Table.Triggers = nextSpec.Table.Triggers
		allowed.Table.Indexes = nextSpec.Table.Indexes
		if !reflect.DeepEqual(allowed, nextSpec) {
			return model.ErrInvalidDescriptor
		}
		nextIndexes := make(map[model.ObjectID]bool, len(nextSpec.Table.Indexes))
		for _, index := range nextSpec.Table.Indexes {
			nextIndexes[index] = true
		}
		for _, index := range baseSpec.Table.Indexes {
			if !nextIndexes[index] {
				expectedDrops[index] = update.Next
			}
		}
	}
	for _, update := range set.TypedIndexUpdates {
		table := expectedUpdates[update.Base.ID()]
		if table == nil || !typedIndexMatchesTable(ctx, update.Base, update.Next, table, s.limits.Descriptor) {
			return model.ErrInvalidDescriptor
		}
		delete(expectedUpdates, update.Base.ID())
	}
	for _, descriptor := range set.TypedIndexDrops {
		table := expectedDrops[descriptor.ID()]
		spec, err := descriptor.SpecWithin(ctx, s.limits.Descriptor)
		if table == nil || err != nil || spec.Index == nil || spec.Index.Table != table.ID() {
			return model.ErrInvalidDescriptor
		}
		delete(expectedDrops, descriptor.ID())
	}
	if len(expectedUpdates) != 0 || len(expectedDrops) != 0 {
		return model.ErrInvalidDescriptor
	}
	return nil
}

func sameTypeDependencyCascade(ctx context.Context, planned TypeDependencyCascade, set TypeChangeSet, limits model.DescriptorLimits) bool {
	if len(planned.TypedTableUpdates) != len(set.TypedTableUpdates) || len(planned.DependencyTableUpdates) != len(set.DependencyTableUpdates) || len(planned.IndexDrops) != len(set.TypedIndexDrops) || len(planned.ObjectDrops) != len(set.DependencyDrops) {
		return false
	}
	sameUpdateSet := func(left, right []ObjectUpdate) bool {
		expected := make(map[model.ObjectID]ObjectUpdate, len(left))
		for _, update := range left {
			if update.Base == nil || expected[update.Base.ID()].Base != nil {
				return false
			}
			expected[update.Base.ID()] = update
		}
		for _, update := range right {
			if update.Base == nil || update.Next == nil {
				return false
			}
			candidate, ok := expected[update.Base.ID()]
			if !ok || candidate.Base.Version() != update.Base.Version() {
				return false
			}
			leftSpec, leftErr := candidate.Next.SpecWithin(ctx, limits)
			rightSpec, rightErr := update.Next.SpecWithin(ctx, limits)
			if leftErr != nil || rightErr != nil || !reflect.DeepEqual(leftSpec, rightSpec) {
				return false
			}
			delete(expected, update.Base.ID())
		}
		return len(expected) == 0
	}
	if !sameUpdateSet(planned.TypedTableUpdates, set.TypedTableUpdates) || !sameUpdateSet(planned.DependencyTableUpdates, set.DependencyTableUpdates) {
		return false
	}
	for i, descriptor := range planned.IndexDrops {
		actual := set.TypedIndexDrops[i]
		if descriptor == nil || actual == nil || descriptor.ID() != actual.ID() || descriptor.Version() != actual.Version() {
			return false
		}
	}
	for i, descriptor := range planned.ObjectDrops {
		actual := set.DependencyDrops[i]
		if descriptor == nil || actual == nil || descriptor.ID() != actual.ID() || descriptor.Version() != actual.Version() {
			return false
		}
	}
	return true
}

func typedTableMatchesComposite(ctx context.Context, table, composite *model.Descriptor, limits model.DescriptorLimits) bool {
	if table == nil || composite == nil {
		return false
	}
	tableSpec, tableErr := table.SpecWithin(ctx, limits)
	typeSpec, typeErr := composite.SpecWithin(ctx, limits)
	if tableErr != nil || typeErr != nil || tableSpec.Table == nil || typeSpec.Type == nil || tableSpec.Table.TypedOf != composite.ID() {
		return false
	}
	if !slices.ContainsFunc(tableSpec.Dependencies, func(dependency model.Dependency) bool {
		return dependency.Object == composite.ID() && dependency.Version == composite.Version() && dependency.Column == 0
	}) {
		return false
	}
	if len(tableSpec.Table.TypedColumns) != len(typeSpec.Type.Fields) {
		return false
	}
	columns := make(map[model.ColumnID]model.ColumnDescriptor, len(tableSpec.Table.Columns))
	for _, column := range tableSpec.Table.Columns {
		columns[column.ID] = column
	}
	fields := make(map[model.ColumnID]model.ColumnDescriptor, len(typeSpec.Type.Fields))
	for _, field := range typeSpec.Type.Fields {
		fields[field.ID] = field
	}
	for _, mapping := range tableSpec.Table.TypedColumns {
		field, fieldOK := fields[mapping.Field]
		column, columnOK := columns[mapping.Column]
		if !fieldOK || !columnOK || column.SystemIdentity || field.Name != column.Name || !sameTypeServiceLayout(field.Layout, column.Layout) {
			return false
		}
	}
	return true
}

func typedIndexMatchesTable(ctx context.Context, base, next, table *model.Descriptor, limits model.DescriptorLimits) bool {
	if base == nil || next == nil || table == nil || base.Kind() != model.ObjectIndex || next.Kind() != model.ObjectIndex || base.ID() != next.ID() || next.Version() != base.Version()+1 || next.Physical().Generation <= base.Physical().Generation {
		return false
	}
	spec, err := next.SpecWithin(ctx, limits)
	if err != nil || spec.Index == nil || spec.Index.Table != table.ID() {
		return false
	}
	return slices.ContainsFunc(spec.Dependencies, func(dependency model.Dependency) bool {
		return dependency.Object == table.ID() && dependency.Version == table.Version() && dependency.Column == 0
	})
}

func (s *Store) inheritedTableMatchesParents(ctx context.Context, snapshot *CatalogSnapshot, table *model.Descriptor, parents []model.ObjectID, targets map[model.ObjectID]*model.Descriptor) bool {
	if table == nil {
		return false
	}
	tableSpec, err := table.SpecWithin(ctx, s.limits.Descriptor)
	if err != nil || tableSpec.Table == nil || tableSpec.Table.TypedOf != 0 {
		return false
	}
	for _, parentID := range parents {
		target := targets[parentID]
		if target == nil || !slices.Contains(tableSpec.Table.Parents, parentID) {
			return false
		}
		if !slices.ContainsFunc(tableSpec.Dependencies, func(dependency model.Dependency) bool {
			return dependency.Object == parentID && dependency.Version == target.Version() && dependency.Column == 0
		}) {
			return false
		}
		targetSpec, err := target.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil || targetSpec.Table == nil {
			return false
		}
		parentColumns := make(map[model.ColumnID]model.ColumnDescriptor, len(targetSpec.Table.Columns))
		for _, column := range targetSpec.Table.Columns {
			parentColumns[column.ID] = column
		}
		matched := make(map[model.ColumnID]bool, len(parentColumns))
		for _, child := range tableSpec.Table.Columns {
			for _, inherited := range child.InheritedFrom {
				if inherited.Table != parentID {
					continue
				}
				parent, exists := parentColumns[inherited.Column]
				if !exists || parent.Name != child.Name || !sameTypeServiceLayout(parent.Layout, child.Layout) {
					return false
				}
				matched[parent.ID] = true
			}
		}
		for _, parent := range targetSpec.Table.Columns {
			if !parent.SystemIdentity && !matched[parent.ID] {
				return false
			}
		}
	}
	return true
}

func sameTypeServiceLayout(left, right model.TypeLayout) bool {
	if left.Type != right.Type || len(left.Children) != len(right.Children) {
		return false
	}
	for i := range left.Children {
		if !sameTypeServiceLayout(left.Children[i], right.Children[i]) {
			return false
		}
	}
	return true
}

func (s *Store) TypeDropOrder(ctx context.Context, snapshot *CatalogSnapshot, requested []*model.Descriptor, cascade bool, maximum uint32) ([]*model.Descriptor, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, err
	}
	if maximum == 0 {
		return nil, &model.Error{Code: "22023", Message: "type dependency descriptor limit is zero"}
	}
	return s.typeDropOrder(ctx, snapshot, requested, cascade, maximum)
}

func (s *Store) typeDropOrder(ctx context.Context, snapshot *CatalogSnapshot, requested []*model.Descriptor, cascade bool, maximum uint32) ([]*model.Descriptor, error) {
	allowed := func(kind model.ObjectKind) bool { return kind == model.ObjectType || kind == model.ObjectCompositeRelation }
	return s.dropOrder(ctx, snapshot, requested, cascade, maximum, catalogScanKinds, "type dependency scan limit exceeded", allowed,
		"type dependency cycle prevents drop", "cannot drop type because other objects depend on it", "type cascade descriptor limit exceeded")
}

// catalogScanKinds is the descriptor kind inventory used by the type lifecycle.
var catalogScanKinds = []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectType, model.ObjectCompositeRelation, model.ObjectCast, model.ObjectOperator}
