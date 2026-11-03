package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type partitionInputAdapter struct{ partitionedCursor }

func (c partitionInputAdapter) Partition() uint32 { return c.partitionedCursor.partition() }

type partitionJoinOutput struct {
	exjoin.PartitionedCursor
	ordered bool
}

func (c *partitionJoinOutput) partition() uint32     { return c.PartitionedCursor.Partition() }
func (c *partitionJoinOutput) globallyOrdered() bool { return c.ordered }

func (c *invocation) partitionedJoin(node physical.NodeSpec, children []storage.BatchCursor, options exjoin.Options) (storage.BatchCursor, error) {
	merge := node.Algorithm == physical.MergeJoin
	if node.Algorithm != physical.HashJoin && !merge || len(children) != 2 {
		return nil, ErrCapability
	}
	left := c.prepared.nodes[node.Children[0]].node.Delivered.Distribution
	right := c.prepared.nodes[node.Children[1]].node.Delivered.Distribution
	sameMapping := left.Partitions == right.Partitions && left.Function == right.Function && left.Epoch == right.Epoch
	partitioned := left.Kind == physical.HashPartitioned && right.Kind == physical.HashPartitioned
	invalidKeys := len(left.Keys) == 0 || len(left.Keys) != len(right.Keys)
	if !partitioned || !sameMapping || left.Function != uint64(exchange.XXH3V1) || invalidKeys {
		return nil, ErrCapability
	}
	bytes := uint64(len(left.Keys))*uint64(unsafe.Sizeof(int(0))) + 2*uint64(unsafe.Sizeof(partitionInputAdapter{})) + uint64(unsafe.Sizeof(partitionJoinOutput{}))
	if err := c.charge(bytes); err != nil {
		return nil, err
	}
	partitioning := exjoin.Partitioning{Count: left.Partitions, KeyIndices: make([]int, len(left.Keys))}
	for i, key := range left.Keys {
		ordinal := -1
		for j, equality := range node.Join.Keys {
			if equality.Left == key && equality.Right == right.Keys[i] {
				ordinal = j
				break
			}
		}
		if ordinal < 0 {
			return nil, ErrCapability
		}
		partitioning.KeyIndices[i] = ordinal
	}
	var ordering []exjoin.MergeOrder
	if merge {
		var err error
		ordering, err = c.mergeOrdering(node)
		if err != nil {
			return nil, err
		}
	}
	if c.preparing {
		if merge {
			return nil, exjoin.CheckPartitionedMergeSupport(c.portal.ctx, partitioning, options, ordering)
		}
		return nil, exjoin.CheckPartitionedSupport(c.portal.ctx, partitioning, options)
	}
	leftInput, leftOK := children[0].(partitionedCursor)
	rightInput, rightOK := children[1].(partitionedCursor)
	if !leftOK || !rightOK {
		return nil, ErrCapability
	}
	if merge {
		for _, input := range children {
			ordered, ok := input.(globallyOrderedPartitions)
			if !ok || !ordered.globallyOrdered() {
				return nil, ErrCapability
			}
		}
	}
	var cursor exjoin.PartitionedCursor
	var err error
	if merge {
		cursor, err = exjoin.NewPartitionedMerge(c.portal.ctx, partitionInputAdapter{leftInput}, partitionInputAdapter{rightInput}, partitioning, options, ordering)
	} else {
		cursor, err = exjoin.NewPartitioned(c.portal.ctx, partitionInputAdapter{leftInput}, partitionInputAdapter{rightInput}, partitioning, options)
	}
	if err != nil {
		return nil, err
	}
	return &partitionJoinOutput{PartitionedCursor: cursor, ordered: merge && len(node.Delivered.Ordering) != 0}, nil
}
