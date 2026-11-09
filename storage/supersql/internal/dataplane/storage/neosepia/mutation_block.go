package neosepia

import (
	"bytes"
	"cmp"
	"context"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type mutationBlockLimits struct {
	BuildBytes, SortBytes uint64
	Block                 blockLimits
}

// One target and commit timestamp per builder. Append borrows each normalized
// mutation only for the call; a failed append leaves the preceding rows intact.
// Flush owns its encoded result and resets rows only after successful encoding.
type mutationBlockBuilder struct {
	layout      storage.ObjectLayout
	timestamp   model.Timestamp
	limits      mutationBlockLimits
	builder     *columnar.Builder
	values      []model.Value
	columns     []model.ColumnID
	ordinals    []uint64
	kinds       []storage.MutationOp
	lastOrdinal uint64
}

func newMutationBlockBuilder(ctx context.Context, layout storage.ObjectLayout, timestamp model.Timestamp, limits mutationBlockLimits) (*mutationBlockBuilder, error) {
	if err := layout.Validate(ctx); err != nil {
		return nil, err
	}
	if timestamp <= 0 {
		return nil, storage.ErrSnapshotNotClosed
	}
	if limits.Block.MaxRows <= 0 || uint64(limits.Block.MaxRows) > math.MaxUint32 || len(layout.Columns) > math.MaxInt-2 {
		return nil, columnar.ErrBatchBudget
	}
	rows := min(limits.Block.MaxRows, columnar.DefaultBatchSize)
	count := len(layout.Columns) + 2
	fixed := uint64(unsafe.Sizeof(mutationBlockBuilder{})) + uint64(unsafe.Sizeof(columnar.Builder{})) + uint64(count)*(uint64(unsafe.Sizeof(model.TypeLayout{}))+uint64(unsafe.Sizeof(model.Value{}))) + uint64(len(layout.Columns))*4 + uint64(rows)*(8+uint64(unsafe.Sizeof(storage.MutationOp(0))))
	if fixed > limits.BuildBytes {
		return nil, columnar.ErrBatchBudget
	}
	layouts := make([]model.TypeLayout, count)
	layouts[0] = model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}
	layouts[1] = layouts[0]
	columns := make([]model.ColumnID, len(layout.Columns))
	for i, column := range layout.Columns {
		layouts[i+2] = model.TypeLayout{Type: column.Type, Children: column.Children}
		columns[i] = column.ID
	}
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: rows, MaxBytes: limits.BuildBytes - fixed})
	if err != nil {
		return nil, err
	}
	return &mutationBlockBuilder{layout: layout, timestamp: timestamp, limits: limits, builder: builder, values: make([]model.Value, count), columns: columns, ordinals: make([]uint64, 0, rows), kinds: make([]storage.MutationOp, 0, rows)}, nil
}

func (b *mutationBlockBuilder) Append(ctx context.Context, mutation storage.Mutation) error {
	if b.builder == nil {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mutation.Target != b.layout.Target {
		return storage.ErrInvalidTarget
	}
	if mutation.Ordinal <= b.lastOrdinal {
		return storage.ErrUnnormalizedBatch
	}
	switch mutation.Op {
	case storage.MutationUpsert:
		if !slices.Equal(mutation.ColumnIDs, b.columns) || len(mutation.Values) != len(b.columns) || len(mutation.EndKey) != 0 {
			return storage.ErrUnnormalizedBatch
		}
		for i, column := range b.layout.Columns {
			if mutation.Values[i].IsNull && !column.Nullable {
				return storage.ErrUnnormalizedBatch
			}
		}
		copy(b.values[2:], mutation.Values)
	case storage.MutationDelete, storage.MutationDeleteRange:
		if len(mutation.ColumnIDs) != 0 || len(mutation.Values) != 0 {
			return storage.ErrUnnormalizedBatch
		}
		if mutation.Op == storage.MutationDeleteRange {
			if len(mutation.Key) == 0 || len(mutation.EndKey) == 0 || bytes.Compare(mutation.Key, mutation.EndKey) >= 0 {
				return storage.ErrRangeInvalid
			}
		} else if len(mutation.EndKey) != 0 {
			return storage.ErrUnnormalizedBatch
		}
		for i, column := range b.layout.Columns {
			b.values[i+2] = model.NullVal(column.Type.Storage)
		}
	default:
		return storage.ErrUnnormalizedBatch
	}
	b.values[0] = model.BytesVal(mutation.Key)
	b.values[1] = model.BytesVal(mutation.EndKey)
	err := b.builder.Append(ctx, b.values)
	clear(b.values)
	if err != nil {
		return err
	}
	b.ordinals = append(b.ordinals, mutation.Ordinal)
	b.kinds = append(b.kinds, mutation.Op)
	b.lastOrdinal = mutation.Ordinal
	return nil
}

func (b *mutationBlockBuilder) Rows() int {
	if b.builder == nil {
		return 0
	}
	return b.builder.Rows()
}

func (b *mutationBlockBuilder) Flush(ctx context.Context) ([]byte, error) {
	if b.builder == nil {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count := b.builder.Rows()
	if count == 0 {
		return nil, storage.ErrBatchEmpty
	}
	fixed := uint64(count)*(4+8+8+uint64(unsafe.Sizeof(storage.MutationOp(0)))) + uint64(len(b.values))*uint64(unsafe.Sizeof(columnar.Vector{}))
	if fixed > b.limits.SortBytes {
		return nil, columnar.ErrBatchBudget
	}
	var batch columnar.Batch
	if err := b.builder.Publish(&batch); err != nil {
		return nil, err
	}
	order := make([]uint32, count)
	for i := range order {
		order[i] = uint32(i)
	}
	keys := &batch.Columns[0]
	slices.SortFunc(order, func(a, bIndex uint32) int {
		aRange, bRange := keys.Ranges[a], keys.Ranges[bIndex]
		aKey := keys.Data[aRange.Offset : aRange.Offset+aRange.Length]
		bKey := keys.Data[bRange.Offset : bRange.Offset+bRange.Length]
		if order := bytes.Compare(aKey, bKey); order != 0 {
			return order
		}
		return cmp.Compare(b.ordinals[bIndex], b.ordinals[a])
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sorted, _, err := permuteBlock(ctx, &batch, order, b.limits.SortBytes-fixed)
	if err != nil {
		return nil, err
	}
	versions := blockVersions{Keys: sorted.Columns[0], EndKeys: sorted.Columns[1], CommitTS: make([]model.Timestamp, count), Ordinals: make([]uint64, count), Kinds: make([]storage.MutationOp, count)}
	for i, lane := range order {
		versions.CommitTS[i] = b.timestamp
		versions.Ordinals[i] = b.ordinals[lane]
		versions.Kinds[i] = b.kinds[lane]
	}
	payload := columnar.Batch{Length: count, Columns: sorted.Columns[2:]}
	data, err := encodeBlock(ctx, b.layout.Target, versions, &payload, b.columns, b.limits.Block)
	if err != nil {
		return nil, err
	}
	b.builder.Reset()
	b.ordinals = b.ordinals[:0]
	b.kinds = b.kinds[:0]
	return data, nil
}

func (b *mutationBlockBuilder) Close() error {
	if b.builder == nil {
		return nil
	}
	err := b.builder.Close()
	b.builder = nil
	b.layout = storage.ObjectLayout{}
	b.values = nil
	b.columns = nil
	b.ordinals = nil
	b.kinds = nil
	return err
}
