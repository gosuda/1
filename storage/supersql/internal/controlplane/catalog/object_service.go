package catalog

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type ObjectUpdate struct {
	Base *model.Descriptor
	Next *model.Descriptor
}

type ObjectChangeSet struct {
	Creates        []*model.Descriptor
	Updates        []ObjectUpdate
	Drops          []*model.Descriptor
	Cascade        bool
	MaxDescriptors uint32
}

func (s *Store) ApplyObjectChanges(ctx context.Context, snapshot *CatalogSnapshot, set ObjectChangeSet) ([]Change, []txn.ReadDependency, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	if set.MaxDescriptors == 0 {
		return nil, nil, &model.Error{Code: "22023", Message: "object change descriptor limit is zero"}
	}
	count := uint64(len(set.Creates)) + uint64(len(set.Updates)) + uint64(len(set.Drops))
	if count > uint64(set.MaxDescriptors) {
		return nil, nil, &model.Error{Code: "54000", Message: "object change descriptor limit exceeded"}
	}
	changes := make([]Change, 0, count)
	dependencies := make([]txn.ReadDependency, 0, count*3)
	for _, descriptor := range set.Creates {
		if descriptor == nil || !ordinaryObjectKind(descriptor.Kind()) || descriptor.Version() != 1 {
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
		if update.Base == nil || update.Next == nil || !ordinaryObjectKind(update.Base.Kind()) || update.Base.Kind() != update.Next.Kind() || update.Base.ID() != update.Next.ID() || update.Next.Version() != update.Base.Version()+1 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		change, err := s.Update(ctx, snapshot, update.Base.Version(), update.Next)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
		dependencies = append(dependencies, s.DescriptorDependency(update.Base.ID()))
	}
	ordered, err := s.objectDropOrder(ctx, snapshot, set.Drops, set.Cascade, set.MaxDescriptors)
	if err != nil {
		return nil, nil, err
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
func (s *Store) PlanObjectDrops(ctx context.Context, snapshot *CatalogSnapshot, requested []*model.Descriptor, cascade bool, maximum uint32) ([]*model.Descriptor, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return nil, err
	}
	if maximum == 0 {
		return nil, &model.Error{Code: "22023", Message: "object change descriptor limit is zero"}
	}
	return s.objectDropOrder(ctx, snapshot, requested, cascade, maximum)
}


// ordinaryObjectKind lists the descriptor kinds this generic lifecycle accepts.
// Triggers are nested metadata and aggregates are ObjectRoutine descriptors, so
// neither appears here; their dependents are reached through the parent
// descriptor's dependencies instead.
func ordinaryObjectKind(kind model.ObjectKind) bool {
	switch kind {
	case model.ObjectSchema, model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation, model.ObjectType, model.ObjectRoutine, model.ObjectOperator, model.ObjectCast, model.ObjectTextSearchDictionary, model.ObjectTextSearchConfiguration, model.ObjectRole, model.ObjectCollation:
		return true
	default:
		return false
	}
}

func (s *Store) catalogScanLimit() uint32 {
	if s.limits.Descriptor.MaxItems <= 0 {
		return 0
	}
	return uint32(min(uint64(s.limits.Descriptor.MaxItems), uint64(math.MaxUint32)))
}

func (s *Store) objectDropOrder(ctx context.Context, snapshot *CatalogSnapshot, requested []*model.Descriptor, cascade bool, maximum uint32) ([]*model.Descriptor, error) {
	return s.dropOrder(ctx, snapshot, requested, cascade, maximum, objectScanKinds, "object dependency scan limit exceeded", ordinaryObjectKind,
		"object dependency cycle prevents drop", "cannot drop object because other objects depend on it", "object cascade descriptor limit exceeded")
}

// dropOrder is the shared closure-expansion plus topological ordering used by
// the generic object and type drop planners. Callers differ only in the kinds
// they scan, the kind predicate they accept, and the error text they report.
// The full-catalog scan is bounded by the store's descriptor population limit
// (Limits.Descriptor.MaxItems), while maximum bounds only the affected targets.
func (s *Store) dropOrder(ctx context.Context, snapshot *CatalogSnapshot, requested []*model.Descriptor, cascade bool, maximum uint32, kinds []model.ObjectKind, scanMsg string, allowed func(model.ObjectKind) bool, cycleMsg, depMsg, limitMsg string) ([]*model.Descriptor, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	targets := make(map[model.ObjectID]*model.Descriptor, len(requested))
	for _, descriptor := range requested {
		if descriptor == nil || !allowed(descriptor.Kind()) {
			return nil, model.ErrInvalidDescriptor
		}
		targets[descriptor.ID()] = descriptor
	}
	if uint64(len(targets)) > uint64(maximum) {
		return nil, &model.Error{Code: "54000", Message: limitMsg}
	}
	dependents := make(map[model.ObjectID][]*model.Descriptor)
	depsByID := make(map[model.ObjectID][]model.Dependency)
	err := scanDescriptors(ctx, snapshot, s.catalogScanLimit(), kinds, scanMsg, func(descriptor *model.Descriptor) error {
		if descriptor == nil {
			return nil
		}
		spec, err := descriptor.SpecWithin(ctx, s.limits.Descriptor)
		if err != nil {
			return err
		}
		id := descriptor.ID()
		depsByID[id] = spec.Dependencies
		inTargets := targets[id] != nil
		var seenDep map[model.ObjectID]struct{}
		for _, dependency := range spec.Dependencies {
			if dependency.Object == id || dependency.Object == 0 {
				continue
			}
			if !inTargets && !cascade && targets[dependency.Object] != nil {
				return &model.Error{Code: "2BP01", Message: depMsg}
			}
			if len(spec.Dependencies) > 1 {
				if seenDep == nil {
					seenDep = make(map[model.ObjectID]struct{}, len(spec.Dependencies))
				}
				if _, dup := seenDep[dependency.Object]; dup {
					continue
				}
				seenDep[dependency.Object] = struct{}{}
			}
			dependents[dependency.Object] = append(dependents[dependency.Object], descriptor)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cascade {
		queue := make([]model.ObjectID, 0, len(targets))
		for id := range targets {
			queue = append(queue, id)
		}
		slices.Sort(queue)
		for head := 0; head < len(queue); head++ {
			current := queue[head]
			children := dependents[current]
			slices.SortFunc(children, func(left, right *model.Descriptor) int {
				return cmp.Compare(left.ID(), right.ID())
			})
			for _, child := range children {
				if targets[child.ID()] != nil {
					continue
				}
				if uint64(len(targets)) >= uint64(maximum) {
					return nil, &model.Error{Code: "54000", Message: limitMsg}
				}
				targets[child.ID()] = child
				queue = append(queue, child.ID())
			}
		}
	}
	ids := make([]model.ObjectID, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	targetDependents := make(map[model.ObjectID][]model.ObjectID, len(ids))
	for _, candidateID := range ids {
		deps, known := depsByID[candidateID]
		if !known {
			spec, err := targets[candidateID].SpecWithin(ctx, s.limits.Descriptor)
			if err != nil {
				return nil, err
			}
			deps = spec.Dependencies
		}
		var seen map[model.ObjectID]struct{}
		for _, dependency := range deps {
			if dependency.Object == candidateID || targets[dependency.Object] == nil {
				continue
			}
			if len(deps) > 1 {
				if seen == nil {
					seen = make(map[model.ObjectID]struct{}, len(deps))
				}
				if _, dup := seen[dependency.Object]; dup {
					continue
				}
				seen[dependency.Object] = struct{}{}
			}
			targetDependents[dependency.Object] = append(targetDependents[dependency.Object], candidateID)
		}
	}
	ordered := make([]*model.Descriptor, 0, len(targets))
	visiting := make(map[model.ObjectID]bool, len(targets))
	visited := make(map[model.ObjectID]bool, len(targets))
	var visit func(model.ObjectID) error
	visit = func(id model.ObjectID) error {
		if visited[id] {
			return nil
		}
		if visiting[id] {
			return &model.Error{Code: "2BP01", Message: cycleMsg}
		}
		visiting[id] = true
		for _, dependentID := range targetDependents[id] {
			if err := visit(dependentID); err != nil {
				return err
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

// objectScanKinds is the complete inventory of standalone descriptor kinds.
// Triggers are nested metadata and aggregates are ObjectRoutine descriptors, so
// neither is scanned: a nested trigger's routine dependency is recorded on its
// parent descriptor, which is scanned here, so CASCADE closure still reaches
// every dependent.
var objectScanKinds = []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectType, model.ObjectCompositeRelation, model.ObjectTextSearchDictionary, model.ObjectTextSearchConfiguration, model.ObjectTextSearchTemplate, model.ObjectCast, model.ObjectOperator}

// scanDescriptors walks the requested standalone descriptor kinds under every
// published schema, enforcing the shared scan budget and reporting limitMsg on
// overflow so each caller keeps its own error text.
func scanDescriptors(ctx context.Context, snapshot *CatalogSnapshot, maximum uint32, kinds []model.ObjectKind, limitMsg string, visit func(*model.Descriptor) error) error {
	var count uint32
	return snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema PublishedDescriptor) error {
		for _, kind := range kinds {
			if err := snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), kind, func(published PublishedDescriptor) error {
				if count == maximum {
					return &model.Error{Code: "54000", Message: limitMsg}
				}
				count++
				return visit(published.Descriptor)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
