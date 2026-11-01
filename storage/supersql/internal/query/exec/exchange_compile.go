package exec

import (
	"context"
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// partitionedCursor publishes one partition per batch. Ordinary operators must
// preserve this identity or consume it at an explicit gather boundary.
type partitionedCursor interface {
	storage.BatchCursor
	partition() uint32
}

type globallyOrderedPartitions interface {
	partitionedCursor
	globallyOrdered() bool
}

type hashExchangeCursor struct {
	input *exchange.Repartition
	batch exchange.PartitionBatch
}

func (c *hashExchangeCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	c.batch.Batch.MaxRows = out.MaxRows
	if err := c.input.Next(ctx, &c.batch); err != nil {
		return err
	}
	publishView(out, &c.batch.Batch)
	return nil
}
func (c *hashExchangeCursor) partition() uint32 { return c.batch.Partition }
func (c *hashExchangeCursor) Close() error {
	c.batch.Batch.Reset()
	return c.input.Close()
}

type partitionView struct {
	storage.BatchCursor
	source partitionedCursor
}

func (c *partitionView) partition() uint32 { return c.source.partition() }

func (c *partitionView) globallyOrdered() bool {
	source, ok := c.source.(globallyOrderedPartitions)
	return ok && source.globallyOrdered()
}

func partitionFlow(plan *physical.Plan, id physical.NodeID, node physical.Node) error {
	meta := node.Metadata()
	missing := func(reason string) error {
		return &RequiredExecutionError{Node: id, Algorithm: meta.Algorithm, Reason: reason}
	}
	kind, _, _, _ := node.Delivered().Distribution()
	if kind != physical.SingleNode {
		if id == plan.Root() {
			return missing("partitioned root requires an explicit gather")
		}
		if kind != physical.HashPartitioned {
			return missing("native range/replicated partition streams")
		}
		switch meta.Algorithm {
		case physical.LocalHashRepartition, physical.HashJoin, physical.MergeJoin, physical.HashAggregate, physical.StreamAggregate, physical.Materialize, physical.CTEMaterialize, physical.CTEScan, physical.Filter, physical.Project, physical.Sort, physical.TopK:
		default:
			return missing("stateful partition-stream execution")
		}
	}
	for i := uint32(0); i < node.ChildCount(); i++ {
		childID, _ := node.Child(i)
		child, _ := plan.Node(childID)
		childKind, _, _, _ := child.Delivered().Distribution()
		if childKind == physical.SingleNode {
			continue
		}
		switch meta.Algorithm {
		case physical.LocalHashRepartition, physical.LocalGather, physical.LocalOrderedMerge, physical.HashJoin, physical.MergeJoin, physical.HashAggregate, physical.StreamAggregate, physical.Materialize, physical.CTEMaterialize, physical.Filter, physical.Project, physical.Sort, physical.TopK:
		default:
			return missing("operator cannot consume partition identities")
		}
	}
	return nil
}

func (c *invocation) localExchange(node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	if len(children) != 1 {
		return nil, ErrInvalid
	}
	child, _ := c.plan.Node(node.Children[0])
	childKind, _, _, _ := child.Delivered().Distribution()
	if !c.preparing && childKind != physical.SingleNode {
		if _, ok := children[0].(partitionedCursor); !ok {
			return nil, ErrCapability
		}
	}
	// A child can be a multiplexed partition stream, not N independently owned
	// cursors. Its queues already supply backpressure; no fake duplicate sources.
	sources := exchange.Options{Query: c.portal.env.Query, Layouts: layouts, BatchRows: c.portal.env.Limits.BatchRows, BatchBytes: c.portal.env.Limits.BatchBytes, QueueBytes: node.Exchange.QueueBytes, MetadataBytes: c.portal.env.Limits.MetadataBytes, EstimatedRows: child.Metadata().Estimate.Rows}
	if node.Algorithm == physical.LocalGather {
		if c.preparing {
			return nil, exchange.CheckSupport(c.portal.ctx, 1, sources)
		}
		return exchange.NewGather(c.portal.ctx, children, sources)
	}
	if node.Algorithm == physical.LocalOrderedMerge {
		return c.orderedExchange(node, children, childKind, sources)
	}
	options := c.portal.env.Exchange
	options.Sources = sources
	options.Encoder = c.portal.env.Keys
	options.Function = exchange.HashFunction(node.Delivered.Distribution.Function)
	options.Partitions = node.Delivered.Distribution.Partitions
	keys := node.Delivered.Distribution.Keys
	if err := c.charge(uint64(len(keys))*uint64(unsafe.Sizeof(exchange.HashKey{})) + uint64(unsafe.Sizeof(hashExchangeCursor{}))); err != nil {
		return nil, err
	}
	options.Keys = make([]exchange.HashKey, len(keys))
	for i, key := range keys {
		index := slices.Index(node.Output, key.Slot)
		if index < 0 || layouts[index].Type != key.Type {
			return nil, ErrCapability
		}
		options.Keys[i] = exchange.HashKey{Column: index, InputCollation: key.Type.Collation}
	}
	if c.preparing {
		return nil, exchange.CheckHashSupport(c.portal.ctx, 1, options)
	}
	input, err := exchange.NewHashRepartition(c.portal.ctx, children, options)
	if err != nil {
		return nil, err
	}
	return &hashExchangeCursor{input: input}, nil
}

func (c *invocation) orderedExchange(node physical.NodeSpec, children []storage.BatchCursor, childKind physical.DistributionKind, sources exchange.Options) (storage.BatchCursor, error) {
	if !c.preparing && childKind != physical.SingleNode {
		ordered, ok := children[0].(globallyOrderedPartitions)
		if !ok || !ordered.globallyOrdered() {
			return nil, ErrCapability
		}
	}
	keys, columns, err := c.orderColumns(node.Delivered.Ordering, node.Output, sources.Layouts)
	if err != nil {
		return nil, err
	}
	if err := c.charge(uint64(len(keys)) * uint64(unsafe.Sizeof(exsort.Key{}))); err != nil {
		return nil, err
	}
	options := exchange.MergeOptions{Sources: sources, Encoder: c.portal.env.Keys, MaxKeyBytes: c.portal.env.Exchange.MaxKeyBytes, WorkBytes: c.portal.env.Exchange.WorkBytes, Keys: make([]exsort.Key, len(keys))}
	for i, key := range keys {
		options.Keys[i] = exsort.Key{Column: columns[i], Descending: key.Descending, NullsFirst: key.NullsFirst, InputCollation: key.Type.Collation}
	}
	if c.preparing {
		return nil, exchange.CheckMergeSupport(c.portal.ctx, 1, options)
	}
	return exchange.NewOrderedMerge(c.portal.ctx, children, options)
}

func (c *invocation) preservePartition(node physical.NodeSpec, children []storage.BatchCursor, cursor storage.BatchCursor) (storage.BatchCursor, error) {
	if node.Delivered.Distribution.Kind == physical.SingleNode {
		return cursor, nil
	}
	if _, ok := cursor.(partitionedCursor); ok {
		return cursor, nil
	}
	if len(children) != 1 {
		return nil, errors.Join(ErrCapability, cursor.Close())
	}
	source, ok := children[0].(partitionedCursor)
	if !ok {
		return nil, errors.Join(ErrCapability, cursor.Close())
	}
	if err := c.charge(uint64(unsafe.Sizeof(partitionView{}))); err != nil {
		return nil, errors.Join(err, cursor.Close())
	}
	return &partitionView{BatchCursor: cursor, source: source}, nil
}
