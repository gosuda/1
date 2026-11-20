package exec

import (
	"context"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type compiledRowLock struct {
	identity [4]int
	row      int
	layout   []storage.KeyColumn
	values   []model.Value
	strength txn.RowLockStrength
	wait     txn.RowLockWait
}

type rowLockCursor struct {
	input      storage.BatchCursor
	transaction *mutationScope
	encoder    storage.KeyEncoder
	locks      []compiledRowLock
	requests   []txn.RowLockRequest
	selection  []uint32
	maxKey     uint64
	closed     bool
	closeErr   error
}

func (c *invocation) lockRows(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	if len(children) != 1 {
		return nil, ErrInvalid
	}
	graph, ok := c.plan.Graph(node.Logical.Graph)
	if !ok {
		return nil, ErrInvalid
	}
	relation, ok := graph.Rel(node.Logical.Rel)
	if !ok || relation.Kind != ir.LockRows || relation.LockCount() == 0 {
		return nil, ErrInvalid
	}
	unit := uint64(unsafe.Sizeof(compiledRowLock{})) + uint64(unsafe.Sizeof(txn.RowLockRequest{}))
	if err := c.charge(uint64(relation.LockCount())*unit + uint64(c.portal.env.Limits.BatchRows)*4); err != nil {
		return nil, err
	}
	locks := make([]compiledRowLock, relation.LockCount())
	for index := range relation.LockCount() {
		logical, ok := graph.RowLock(node.Logical.Rel, index)
		if !ok || logical.KeyCount() == 0 {
			return nil, ErrInvalid
		}
		lock := &locks[index]
		identity := [...]model.SlotID{logical.ObjectSlot, logical.DescriptorVersionSlot, logical.GenerationSlot, logical.EncodingVersionSlot}
		for i, slot := range identity {
			lock.identity[i] = joinColumn(node.Output, physical.Slot{Graph: node.Logical.Graph, ID: slot})
			if lock.identity[i] < 0 {
				return nil, ErrInvalid
			}
		}
		lock.row = joinColumn(node.Output, physical.Slot{Graph: node.Logical.Graph, ID: logical.RowSlot})
		if lock.row < 0 || layouts[lock.row].Type.Storage != model.TypeComposite || uint32(len(layouts[lock.row].Children)) != logical.KeyCount() {
			return nil, ErrInvalid
		}
		if err := c.charge(uint64(logical.KeyCount()) * (uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{})))); err != nil {
			return nil, err
		}
		lock.layout = make([]storage.KeyColumn, logical.KeyCount())
		lock.values = make([]model.Value, logical.KeyCount())
		for keyIndex := range logical.KeyCount() {
			key, ok := graph.RowLockKey(node.Logical.Rel, index, keyIndex)
			if !ok {
				return nil, ErrInvalid
			}
			layout := layouts[lock.row].Children[keyIndex]
			lock.layout[keyIndex] = storage.KeyColumn{ColumnID: model.ColumnID(keyIndex + 1), Type: layout.Type, Children: layout.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		}
		lock.strength = txn.RowLockStrength(logical.Strength - ir.KeyShare)
		lock.wait = txn.RowLockWait(logical.Wait)
	}
	if c.preparing {
		return nil, nil
	}
	for _, lock := range locks {
		if _, err := c.portal.env.Keys.EncodePrefix(c.portal.ctx, lock.layout, nil, c.portal.env.Modify.MaxRecordBytes); err != nil {
			return nil, err
		}
	}
	c.portal.rowLocks = true
	return &rowLockCursor{
		input: children[0], transaction: &c.portal.mutations, encoder: c.portal.env.Keys, locks: locks,
		requests: make([]txn.RowLockRequest, 0, len(locks)), selection: make([]uint32, 0, c.portal.env.Limits.BatchRows), maxKey: c.portal.env.Modify.MaxRecordBytes,
	}, nil
}

func (c *rowLockCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if c.closed {
		return io.EOF
	}
	if err := c.input.Next(ctx, out); err != nil {
		return err
	}
	c.selection = c.selection[:0]
	for row := range out.Rows() {
		lane := out.Lane(row)
		admit := true
		for wait := txn.WaitForRowLock; wait <= txn.SkipLockedRow && admit; wait++ {
			c.requests = c.requests[:0]
			for index := range c.locks {
				lock := &c.locks[index]
				if lock.wait != wait {
					continue
				}
				request, err := c.request(ctx, out, lane, lock)
				if err != nil {
					return err
				}
				c.requests = append(c.requests, request)
			}
			if len(c.requests) == 0 {
				continue
			}
			acquired, err := c.transaction.LockRows(ctx, c.requests, wait)
			if err != nil {
				return err
			}
			admit = acquired
		}
		if admit {
			c.selection = append(c.selection, uint32(lane))
		}
	}
	out.Selection = c.selection
	return nil
}

func (c *rowLockCursor) request(ctx context.Context, batch *columnar.Batch, lane int, lock *compiledRowLock) (txn.RowLockRequest, error) {
	var identity [4]int64
	for i, column := range lock.identity {
		vector := &batch.Columns[column]
		if vector.IsNull(lane) || vector.Type.Storage != model.TypeInt64 {
			return txn.RowLockRequest{}, ErrInvalid
		}
		identity[i] = vector.Int64[vector.ValueIndex(lane)]
	}
	if identity[0] <= 0 || identity[1] <= 0 || identity[2] <= 0 || identity[3] <= 0 || uint64(identity[3]) > math.MaxUint32 {
		return txn.RowLockRequest{}, ErrInvalid
	}
	target := storage.ObjectRef{ObjectID: model.ObjectID(identity[0]), DescriptorVersion: uint64(identity[1]), Generation: uint64(identity[2]), EncodingVersion: uint32(identity[3])}
	if err := target.Validate(); err != nil {
		return txn.RowLockRequest{}, err
	}
	row := &batch.Columns[lock.row]
	if row.IsNull(lane) || len(row.Children) != len(lock.values) {
		return txn.RowLockRequest{}, ErrInvalid
	}
	valueIndex := row.ValueIndex(lane)
	for i := range lock.values {
		value, _, err := columnar.CopyValue(ctx, &row.Children[i], valueIndex, c.maxKey)
		if err != nil {
			return txn.RowLockRequest{}, err
		}
		lock.values[i] = value
	}
	key, err := c.encoder.EncodePrefix(ctx, lock.layout, lock.values, c.maxKey)
	clear(lock.values)
	if err != nil {
		return txn.RowLockRequest{}, err
	}
	return txn.RowLockRequest{Target: target, Key: key, Strength: lock.strength}, nil
}

func (c *rowLockCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	for i := range c.locks {
		clear(c.locks[i].values)
		c.locks[i].values, c.locks[i].layout = nil, nil
	}
	c.requests, c.selection, c.locks = nil, nil, nil
	c.closeErr = c.input.Close()
	c.input, c.transaction, c.encoder = nil, nil, nil
	return c.closeErr
}
