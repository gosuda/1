package exec

import (
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	"github.com/gosuda/supersql-alpha/internal/query/exec/groupingset"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type partitionAggregateOutput struct {
	exaggregate.PartitionedCursor
	ordered bool
}

func (c *partitionAggregateOutput) partition() uint32     { return c.PartitionedCursor.Partition() }
func (c *partitionAggregateOutput) globallyOrdered() bool { return c.ordered }

func (c *invocation) partitionedAggregate(node physical.NodeSpec, input storage.BatchCursor, options exaggregate.Options) (_ storage.BatchCursor, err error) {
	owned := input
	defer func() {
		if err != nil && owned != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	partitioning, err := c.aggregatePartitioning(node)
	if err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, exaggregate.CheckPartitionedSupport(c.portal.ctx, partitioning, options)
	}
	tagged, ok := input.(partitionedCursor)
	if !ok {
		return nil, ErrCapability
	}
	if options.OrderedGroups {
		ordered, ok := input.(globallyOrderedPartitions)
		if !ok || !ordered.globallyOrdered() {
			return nil, ErrCapability
		}
	}
	owned = nil
	cursor, err := exaggregate.NewPartitioned(c.portal.ctx, partitionInputAdapter{tagged}, partitioning, options)
	if err != nil {
		return nil, err
	}
	return &partitionAggregateOutput{PartitionedCursor: cursor, ordered: options.OrderedGroups && len(node.Delivered.Ordering) != 0}, nil
}

func (c *invocation) aggregatePartitioning(node physical.NodeSpec) (exaggregate.Partitioning, error) {
	child := c.prepared.nodes[node.Children[0]].node
	distribution, output := child.Delivered.Distribution, node.Delivered.Distribution
	sameMapping := output.Partitions == distribution.Partitions && output.Function == distribution.Function && output.Epoch == distribution.Epoch
	if distribution.Kind != physical.HashPartitioned || output.Kind != physical.HashPartitioned || distribution.Function != uint64(exchange.XXH3V1) || !sameMapping {
		return exaggregate.Partitioning{}, ErrCapability
	}
	bytes := uint64(len(distribution.Keys))*uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof(partitionInputAdapter{})) + uint64(unsafe.Sizeof(partitionAggregateOutput{}))
	if err := c.charge(bytes); err != nil {
		return exaggregate.Partitioning{}, err
	}
	partitioning := exaggregate.Partitioning{Count: distribution.Partitions, KeyIndices: make([]int, len(distribution.Keys))}
	for i, key := range distribution.Keys {
		ordinal := slices.Index(node.Aggregate.GroupKeys, key)
		if ordinal < 0 {
			return exaggregate.Partitioning{}, ErrCapability
		}
		partitioning.KeyIndices[i] = ordinal
	}
	return partitioning, nil
}

func (c *invocation) partitionedGroupingSets(node physical.NodeSpec, input storage.BatchCursor, options groupingset.Options) (_ storage.BatchCursor, err error) {
	owned := input
	defer func() {
		if err != nil && owned != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	partitioning, err := c.aggregatePartitioning(node)
	if err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, groupingset.CheckPartitionedSupport(c.portal.ctx, partitioning, options)
	}
	tagged, ok := input.(partitionedCursor)
	if !ok {
		return nil, ErrCapability
	}
	owned = nil
	cursor, err := groupingset.NewPartitioned(c.portal.ctx, partitionInputAdapter{tagged}, partitioning, options)
	if err != nil {
		return nil, err
	}
	return &partitionAggregateOutput{PartitionedCursor: cursor}, nil
}
