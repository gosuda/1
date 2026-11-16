package txn

import (
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (transaction *Transaction) enumerateRange(ctx context.Context, target transactionTarget, mutation storage.Mutation, budget uint64) (err error) {
	keys := target.normalizer.keyLayout
	if len(keys) == 0 {
		return storage.ErrInvalidLayout
	}
	if !transaction.reserveResource() {
		return errTransactionBudget
	}
	defer transaction.resources.Add(-1)
	result, err := transaction.snapshot(ctx)
	if err != nil {
		return err
	}
	reader := result.reader
	defer func() { err = errors.Join(err, reader.Close()) }()
	used, err := mutationMemory(mutation, budget)
	if err != nil {
		return err
	}
	memory := overlayMemory{limit: budget, used: used}
	if err := memory.add(uint64(len(keys)), 4+uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return errTransactionBudget
	}
	columns := make([]model.ColumnID, len(keys))
	values := make([]model.Value, len(keys))
	for i, key := range keys {
		columns[i] = key.ColumnID
	}
	plan, err := reader.PlanScan(ctx, storage.PushdownRequest{
		Target: mutation.Target, Layout: target.layout.Columns, KeyLayout: keys, ColumnIDs: columns,
		EncodedSpans: []storage.EncodedKeySpan{{
			Lower: storage.EncodedKeyBound{Key: mutation.Key, Inclusive: true},
			Upper: storage.EncodedKeyBound{Key: mutation.EndKey},
		}},
	})
	if err != nil {
		return err
	}
	cursor, err := reader.VectorizedScan(ctx, plan)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	batch := columnar.Batch{MaxRows: 1}
	for {
		if err := ctx.Err(); err != nil {
			return transactionError(err)
		}
		err := cursor.Next(ctx, &batch)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(batch.Columns) != len(keys) || batch.Rows() > 1 {
			return columnar.ErrInvalidBatch
		}
		for row := 0; row < batch.Rows(); row++ {
			used := memory.used
			for i := range keys {
				value, size, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(row), memory.limit-used)
				if err != nil {
					return err
				}
				used += size
				values[i] = value
			}
			key, err := transaction.encoder.EncodePrefix(ctx, keys, values, memory.limit-used)
			if err != nil {
				return err
			}
			if uint64(cap(key)) > memory.limit-used {
				return errTransactionBudget
			}
			clear(values)
		}
	}
}
