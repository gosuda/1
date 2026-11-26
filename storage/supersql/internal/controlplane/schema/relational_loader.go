package schema

import (
	"context"
	"errors"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type relationalCandidate struct {
	descriptor *model.Descriptor
	spec       model.DescriptorSpec
}

// relationalGraphCache caches the undirected enforced-FK connected components
// for one CatalogReadIdentity so DML on tables with no incoming or outgoing
// foreign keys pays an O(1) lookup instead of scanning the catalog.
type relationalGraphCache struct {
	mu         sync.RWMutex
	valid      bool
	identity   txn.CatalogReadIdentity
	components map[model.ObjectID][]model.ObjectID
}

func (c *relationalGraphCache) component(ctx context.Context, snapshot *catalog.CatalogSnapshot, identity txn.CatalogReadIdentity, tableID model.ObjectID) ([]model.ObjectID, error) {
	c.mu.RLock()
	if c.valid && c.identity == identity {
		others := c.components[tableID]
		c.mu.RUnlock()
		return others, ctx.Err()
	}
	c.mu.RUnlock()
	components, err := scanRelationalComponents(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.identity = identity
	c.components = components
	c.valid = true
	others := components[tableID]
	c.mu.Unlock()
	return others, ctx.Err()
}

// TableDescriptor returns the base table descriptor bound to this IndexSet.
func (s *IndexSet) TableDescriptor() *model.Descriptor {
	if s == nil {
		return nil
	}
	return s.table
}

// LoadDeclarationRelationalConstraints links an unpublished table expansion to
// its snapshot-visible FK component. The returned support sets are read-only for
// this declaration; the seed remains the caller's foreground expansion.
func LoadDeclarationRelationalConstraints(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) ([]*IndexSet, error) {
	return loadRelationalIndexSetsWithCache(ctx, snapshot, nil, seed, encoder, expressions, limits)
}

// DeclarationRelationalInventoryRequired reports whether an admitted table must
// discover other tables to freeze its complete enforced foreign-key component.
func DeclarationRelationalInventoryRequired(descriptor *model.Descriptor) bool {
	table, ok := descriptor.Table()
	if !ok {
		return false
	}
	return slices.ContainsFunc(table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Enforced && constraint.Kind == model.ConstraintForeignKey
	})
}

func tableHasLocalRelationalConstraints(table *model.TableMetadata) bool {
	if table == nil {
		return false
	}
	for _, constraint := range table.Constraints {
		if !constraint.Enforced {
			continue
		}
		if constraint.Kind == model.ConstraintForeignKey || constraint.Kind == model.ConstraintExclude {
			return true
		}
		if constraint.Deferrable && (constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique) {
			return true
		}
	}
	return false
}

func addRelationalEdges(adj map[model.ObjectID][]model.ObjectID, tableID model.ObjectID, table *model.TableMetadata) {
	if table == nil {
		return
	}
	for _, constraint := range table.Constraints {
		if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced {
			continue
		}
		ref := constraint.ReferencedObject
		if ref == 0 || ref == tableID {
			continue
		}
		adj[tableID] = append(adj[tableID], ref)
		adj[ref] = append(adj[ref], tableID)
	}
}

func buildRelationalComponents(adj map[model.ObjectID][]model.ObjectID) map[model.ObjectID][]model.ObjectID {
	if len(adj) == 0 {
		return nil
	}
	components := make(map[model.ObjectID][]model.ObjectID, len(adj))
	visited := make(map[model.ObjectID]bool, len(adj))
	queue := make([]model.ObjectID, 0, len(adj))
	for start := range adj {
		if visited[start] {
			continue
		}
		visited[start] = true
		queue = append(queue[:0], start)
		for head := 0; head < len(queue); head++ {
			current := queue[head]
			for _, neighbor := range adj[current] {
				if !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		if len(queue) <= 1 {
			continue
		}
		comp := slices.Clone(queue)
		slices.Sort(comp)
		for i, id := range comp {
			others := make([]model.ObjectID, 0, len(comp)-1)
			others = append(others, comp[:i]...)
			others = append(others, comp[i+1:]...)
			components[id] = others
		}
	}
	return components
}

func componentForSeed(adj map[model.ObjectID][]model.ObjectID, seedID model.ObjectID) []model.ObjectID {
	if len(adj[seedID]) == 0 {
		return nil
	}
	visited := map[model.ObjectID]bool{seedID: true}
	queue := []model.ObjectID{seedID}
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		for _, neighbor := range adj[current] {
			if !visited[neighbor] {
				visited[neighbor] = true
				queue = append(queue, neighbor)
			}
		}
	}
	if len(queue) <= 1 {
		return nil
	}
	others := slices.Clone(queue[1:])
	slices.Sort(others)
	return others
}

func scanRelationalComponents(ctx context.Context, snapshot *catalog.CatalogSnapshot) (map[model.ObjectID][]model.ObjectID, error) {
	adj := make(map[model.ObjectID][]model.ObjectID)
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(table catalog.PublishedDescriptor) error {
			if table.Descriptor == nil || table.Descriptor.Kind() != model.ObjectTable || table.Descriptor.State() != model.StatePublic {
				return nil
			}
			meta, ok := table.Descriptor.Table()
			if !ok {
				return nil
			}
			addRelationalEdges(adj, table.Descriptor.ID(), &meta)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return buildRelationalComponents(adj), ctx.Err()
}

func scanRelationalComponentUncached(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, seedSpec model.DescriptorSpec) ([]model.ObjectID, error) {
	adj := make(map[model.ObjectID][]model.ObjectID)
	addRelationalEdges(adj, seed.target.ObjectID, seedSpec.Table)
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(table catalog.PublishedDescriptor) error {
			if table.Descriptor == nil || table.Descriptor.Kind() != model.ObjectTable || table.Descriptor.State() != model.StatePublic {
				return nil
			}
			if table.Descriptor.ID() == seed.target.ObjectID {
				return nil
			}
			meta, ok := table.Descriptor.Table()
			if !ok {
				return nil
			}
			addRelationalEdges(adj, table.Descriptor.ID(), &meta)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return componentForSeed(adj, seed.target.ObjectID), ctx.Err()
}

// scanTemporaryRelationalComponent resolves a temporary table's FK component
// within its session-temporary schema overlay without reading durable catalog
// namespaces. It reports needsGlobal=true only if the temporary component
// references a table outside the temporary schema (e.g. a permanent parent).
func scanTemporaryRelationalComponent(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, seedSpec model.DescriptorSpec) ([]model.ObjectID, bool, error) {
	adj := make(map[model.ObjectID][]model.ObjectID)
	local := map[model.ObjectID]bool{seed.target.ObjectID: true}
	addRelationalEdges(adj, seed.target.ObjectID, seedSpec.Table)
	err := snapshot.ScanNamespace(ctx, seed.table.Name().Parent, model.ObjectTable, func(table catalog.PublishedDescriptor) error {
		if table.Descriptor == nil || table.Descriptor.Kind() != model.ObjectTable || table.Descriptor.State() != model.StatePublic {
			return nil
		}
		id := table.Descriptor.ID()
		local[id] = true
		if id == seed.target.ObjectID {
			return nil
		}
		meta, ok := table.Descriptor.Table()
		if !ok {
			return nil
		}
		addRelationalEdges(adj, id, &meta)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	others := componentForSeed(adj, seed.target.ObjectID)
	for _, id := range others {
		if !local[id] {
			return nil, true, nil
		}
	}
	return others, false, ctx.Err()
}

func seedHasUncachedForeignTarget(seedID model.ObjectID, table *model.TableMetadata, others []model.ObjectID) bool {
	if table == nil {
		return false
	}
	for _, constraint := range table.Constraints {
		if constraint.Kind != model.ConstraintForeignKey || !constraint.Enforced {
			continue
		}
		ref := constraint.ReferencedObject
		if ref != 0 && ref != seedID && !slices.Contains(others, ref) {
			return true
		}
	}
	return false
}

// loadRelationalIndexSets discovers the complete enforced-FK component around
// seed from one catalog snapshot, then binds each component index against the
// same snapshot and codec.
func loadRelationalIndexSets(ctx context.Context, snapshot *catalog.CatalogSnapshot, seed *IndexSet, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) ([]*IndexSet, error) {
	return loadRelationalIndexSetsWithCache(ctx, snapshot, nil, seed, encoder, expressions, limits)
}

func loadRelationalIndexSetsWithCache(ctx context.Context, snapshot *catalog.CatalogSnapshot, cache *relationalGraphCache, seed *IndexSet, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) ([]*IndexSet, error) {
	if snapshot == nil || seed == nil || encoder == nil {
		return nil, errRelationalPolicy
	}
	seedSpec, _, err := seed.table.SpecWithinBytes(ctx, model.DescriptorLimits{
		MaxBytes: limits.MetadataBytes,
		MaxItems: max(limits.MaxColumns, len(seed.indexes)),
		MaxDepth: 64,
	})
	if err != nil || seedSpec.Table == nil {
		return nil, errors.Join(errRelationalPolicy, err)
	}
	localLink := tableHasLocalRelationalConstraints(seedSpec.Table)
	var others []model.ObjectID
	resolved := false
	if cache != nil {
		if identity := snapshot.CatalogIdentity(); identity != (txn.CatalogReadIdentity{}) {
			others, err = cache.component(ctx, snapshot, identity, seed.target.ObjectID)
			if err != nil {
				return nil, err
			}
			if !seedHasUncachedForeignTarget(seed.target.ObjectID, seedSpec.Table, others) {
				resolved = true
			}
		}
	}
	if !resolved {
		if seedSpec.Table.Persistence == "t" {
			var needsGlobal bool
			others, needsGlobal, err = scanTemporaryRelationalComponent(ctx, snapshot, seed, seedSpec)
			if err != nil {
				return nil, err
			}
			resolved = !needsGlobal
		}
	}
	if !resolved {
		others, err = scanRelationalComponentUncached(ctx, snapshot, seed, seedSpec)
		if err != nil {
			return nil, err
		}
	}
	if len(others) == 0 && !localLink {
		return []*IndexSet{seed}, ctx.Err()
	}
	if len(others) == 0 {
		published, err := snapshot.Descriptor(ctx, seed.table.Name().Parent)
		if err != nil || published.Descriptor == nil || published.Descriptor.Kind() != model.ObjectSchema || published.Descriptor.State() != model.StatePublic {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		seed.schemaName = published.Descriptor.Name().Name
		if err := LinkRelationalConstraints(ctx, []*IndexSet{seed}, encoder, limits.MetadataBytes); err != nil {
			return nil, err
		}
		return []*IndexSet{seed}, ctx.Err()
	}
	remaining := limits.MetadataBytes
	fixed := uint64(unsafe.Sizeof(relationalCandidate{})) + uint64(unsafe.Sizeof((*IndexSet)(nil)))
	if fixed > remaining {
		return nil, errRelationalLimit
	}
	candidates := make([]relationalCandidate, 0, len(others))
	for _, id := range others {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil || published.Descriptor == nil {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		descriptor := published.Descriptor
		if descriptor.Kind() != model.ObjectTable {
			return nil, errRelationalPolicy
		}
		if descriptor.State() != model.StatePublic {
			continue
		}
		if remaining <= fixed {
			return nil, errRelationalLimit
		}
		spec, used, err := descriptor.SpecWithinBytes(ctx, model.DescriptorLimits{MaxBytes: remaining - fixed, MaxItems: max(limits.MaxColumns, int((remaining-fixed)/uint64(unsafe.Sizeof(model.ConstraintDescriptor{}))+1)), MaxDepth: 64})
		if err != nil || spec.Table == nil || used > remaining-fixed {
			return nil, errors.Join(errRelationalLimit, err)
		}
		remaining -= fixed + used
		candidates = append(candidates, relationalCandidate{descriptor: descriptor, spec: spec})
	}
	sets := make([]*IndexSet, 0, 1+len(candidates))
	sets = append(sets, seed)
	for _, candidate := range candidates {
		ids := slices.Clone(candidate.spec.Table.Indexes)
		slices.Sort(ids)
		plans := make([]*IndexPlan, len(ids))
		for i, id := range ids {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil || published.Descriptor == nil {
				return nil, errors.Join(errRelationalPolicy, err)
			}
			plans[i], err = NewIndexPlan(ctx, candidate.descriptor, published.Descriptor, encoder, expressions, limits)
			if err != nil {
				return nil, err
			}
		}
		set, err := NewIndexSet(ctx, candidate.descriptor, plans, limits)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set)
	}
	for _, set := range sets {
		schema, err := snapshot.Descriptor(ctx, set.table.Name().Parent)
		if err != nil || schema.Descriptor == nil || schema.Descriptor.Kind() != model.ObjectSchema || schema.Descriptor.State() != model.StatePublic {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		set.schemaName = schema.Descriptor.Name().Name
	}
	if err := LinkRelationalConstraints(ctx, sets, encoder, limits.MetadataBytes); err != nil {
		return nil, err
	}
	return sets, ctx.Err()
}
