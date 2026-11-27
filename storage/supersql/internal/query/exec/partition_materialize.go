package exec

import (
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type materializedPartitionCursor struct {
	*partitionSortCursor
	ordered bool
}

func (c *materializedPartitionCursor) globallyOrdered() bool { return c.ordered }

func (c *invocation) partitionMaterialize(node physical.NodeSpec, layouts []model.TypeLayout, input storage.BatchCursor) (storage.BatchCursor, error) {
	width := uint64(len(layouts) + 1)
	bytes := uint64(unsafe.Sizeof(partitionTagInput{})) + uint64(unsafe.Sizeof(materializeCursor{})) + uint64(unsafe.Sizeof(materializedPartitionCursor{})) + uint64(unsafe.Sizeof(partitionSortCursor{}))
	bytes += width * (uint64(unsafe.Sizeof(model.TypeLayout{})) + 5*uint64(unsafe.Sizeof(columnar.Vector{})))
	bytes += uint64(c.portal.env.Limits.BatchRows) * uint64(unsafe.Sizeof(uint32(0)))
	if err := c.charge(bytes); err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, nil
	}
	tagged, ok := input.(partitionedCursor)
	if !ok {
		return nil, errors.Join(ErrCapability, input.Close())
	}
	count := node.Delivered.Distribution.Partitions
	if node.Delivered.Distribution.Kind != physical.HashPartitioned || count < 2 {
		return nil, errors.Join(ErrCapability, input.Close())
	}
	schema := make([]model.TypeLayout, len(layouts)+1)
	schema[0] = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	copy(schema[1:], layouts)
	transport := &partitionTagInput{input: tagged, partitions: count, batchRows: c.portal.env.Limits.BatchRows, columns: len(layouts), output: make([]columnar.Vector, len(schema)), batch: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))}}
	materialized := &materializeCursor{input: transport, env: &c.portal.env, layouts: schema, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(schema))}}
	runs := &partitionSortCursor{input: materialized, ctx: c.portal.ctx, query: c.portal.env.Query, batchRows: c.portal.env.Limits.BatchRows, partitions: count, output: make([]columnar.Vector, len(layouts)), selection: make([]uint32, c.portal.env.Limits.BatchRows), batch: columnar.Batch{Columns: make([]columnar.Vector, 0, len(schema))}}
	ordered, ok := input.(globallyOrderedPartitions)
	return &materializedPartitionCursor{partitionSortCursor: runs, ordered: ok && ordered.globallyOrdered()}, nil
}
