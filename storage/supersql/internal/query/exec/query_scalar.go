package exec

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type queryScalarEntry struct {
	factory *scalarOnceFactory
	claim   columnar.Allocation
}

func (c *invocation) domainContains(ancestor, domain physical.NodeID) (bool, error) {
	for remaining := len(c.prepared.nodes); remaining > 0; remaining-- {
		if err := c.portal.ctx.Err(); err != nil {
			return false, err
		}
		if domain == ancestor {
			return true, nil
		}
		if domain == 0 {
			return false, nil
		}
		domain = c.prepared.nodes[domain].parentDomain
	}
	return false, ErrInvalid
}

func (c *invocation) prepareQueryScalars() error {
	var seen, localDomains, dependencies []physical.NodeID
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		if err := c.portal.ctx.Err(); err != nil {
			return err
		}
		n := &c.prepared.nodes[id]
		if !n.scalarBody || len(n.captureSlots) != 0 {
			continue
		}
		right := id
		if seen == nil {
			if err := c.charge(uint64(len(c.prepared.nodes)) * 3 * uint64(unsafe.Sizeof(physical.NodeID(0)))); err != nil {
				return err
			}
			seen = make([]physical.NodeID, len(c.prepared.nodes))
			localDomains = make([]physical.NodeID, len(c.prepared.nodes))
			dependencies = make([]physical.NodeID, 0, len(c.prepared.nodes))
		}
		dependencies = dependencies[:0]
		push := func(dependency physical.NodeID) {
			if seen[dependency] != id {
				seen[dependency] = id
				dependencies = append(dependencies, dependency)
			}
		}
		localDomains[right] = id
		push(right)
		for index := 0; index < len(dependencies); index++ {
			if err := c.portal.ctx.Err(); err != nil {
				return err
			}
			d := &c.prepared.nodes[dependencies[index]]
			if d.node.Algorithm == physical.Apply || d.node.Algorithm == physical.RecursiveUnion {
				localDomains[d.node.Children[1]] = id
			}
			for _, child := range d.node.Children {
				push(child)
			}
			for _, dependency := range d.node.StatementDependencies {
				push(dependency)
			}
			for _, subplan := range d.node.ScalarSubplans {
				localDomains[subplan.Root] = id
				push(subplan.Root)
			}
			if d.node.Algorithm == physical.CTEScan {
				push(d.node.Worktable.Producer)
			}
		}
		// Discover invocation entries before classifying dependencies: a CTE
		// producer can itself open local Apply or recursive step invocations.
		for _, dependency := range dependencies {
			if err := c.portal.ctx.Err(); err != nil {
				return err
			}
			d := &c.prepared.nodes[dependency]
			var owner physical.NodeID
			switch d.node.Algorithm {
			case physical.CTEScan:
				owner = c.prepared.nodes[d.node.Worktable.Producer].domain
			case physical.WorktableScan:
				var err error
				owner, err = c.worktableDomain(d.domain, d.node.Worktable.Producer)
				if err != nil {
					return err
				}
			}
			if owner == 0 || localDomains[owner] == id {
				continue
			}
			ancestor, err := c.domainContains(owner, n.parentDomain)
			if err != nil {
				return err
			}
			if !ancestor {
				return &RequiredExecutionError{Node: id, Algorithm: physical.Apply, Reason: "scalar dependency crosses lexical invocation scopes"}
			}
			deeper, err := c.domainContains(n.scalarOwner, owner)
			if err != nil {
				return err
			}
			if deeper {
				n.scalarOwner = owner
			}
		}
		n.queryScalar = true
	}
	return nil
}

type scalarCacheFactory struct {
	owner *invocation
	id    physical.NodeID
}

func (c *invocation) scalarOwner(id physical.NodeID) (*invocation, error) {
	domain := c.prepared.nodes[id].scalarOwner
	for owner := c; owner != nil; owner = owner.parent {
		if owner.domain == domain {
			return owner, nil
		}
	}
	return nil, ErrInvalid
}

func (c *invocation) scalarFactory(id physical.NodeID) (*scalarCacheFactory, error) {
	owner, err := c.scalarOwner(id)
	if err != nil {
		return nil, err
	}
	if err := c.charge(uint64(unsafe.Sizeof(scalarCacheFactory{}))); err != nil {
		return nil, err
	}
	return &scalarCacheFactory{owner: owner, id: id}, nil
}

func (f *scalarCacheFactory) Open(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	once, err := f.owner.queryScalar(f.id)
	if err != nil {
		return nil, err
	}
	return once.Open(ctx, nil)
}

func (c *invocation) queryScalar(id physical.NodeID) (*scalarOnceFactory, error) {
	p := c.portal
	if p.queryScalars != nil && p.queryScalars[id].factory != nil {
		return p.queryScalars[id].factory, nil
	}
	right := id
	layouts := c.prepared.nodes[right].layouts
	bytes := scalarOnceBytes(layouts) + uint64(unsafe.Sizeof(applyFactory{}))
	var arrayBytes uint64
	if p.queryScalars == nil {
		arrayBytes = uint64(len(c.prepared.nodes)) * uint64(unsafe.Sizeof(queryScalarEntry{}))
	}
	if bytes+arrayBytes > p.env.Limits.MetadataBytes-c.used-p.queryScalarBytes {
		return nil, columnar.ErrBatchBudget
	}
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(bytes + arrayBytes)
	if err != nil {
		return nil, err
	}
	p.queryScalarBytes += bytes + arrayBytes
	if p.queryScalars == nil {
		p.queryScalars = make([]queryScalarEntry, len(c.prepared.nodes))
	}
	entry := &p.queryScalars[id]
	factory := &applyFactory{owner: c, root: right}
	entry.factory = newScalarOnce(factory, layouts, &p.env)
	entry.claim = claim
	return entry.factory, nil
}

func (p *Portal) closeQueryScalars() error {
	var err error
	// Physical dependencies precede consumers, including CTE producer edges.
	// Retained inner portals must release readers before dependency spools.
	for id := len(p.queryScalars) - 1; id > 0; id-- {
		entry := &p.queryScalars[id]
		if entry.factory == nil {
			continue
		}
		factory := entry.factory
		err = errors.Join(err, factory.reader.Close(), factory.producer.Close())
		factory.producer, factory.env, factory.layouts = nil, nil, nil
		entry.factory = nil
	}
	for i := range p.queryScalars {
		if claim := p.queryScalars[i].claim; claim != nil {
			claim.Release()
		}
	}
	p.queryScalars = nil
	p.queryScalarBytes = 0
	return err
}
