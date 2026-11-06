package neosepia

import (
	"context"
	"encoding/binary"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The caller retains the source lease through encoding and serializes Close.
// Workspace excludes the source's independently bounded decode cache and output.
func rewriteBlock(ctx context.Context, source *paxBlock, order []uint32, limits blockLimits) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || source.closed {
		return nil, storage.ErrClosed
	}
	if len(order) == 0 || len(order) > limits.MaxRows {
		return nil, columnar.ErrInvalidBatch
	}
	for i, lane := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if uint64(lane) >= uint64(source.rows) {
			return nil, columnar.ErrInvalidBatch
		}
		if i != 0 && lane <= order[i-1] {
			return nil, columnar.ErrInvalidBatch
		}
	}
	budget := encBudget{limit: limits.MaxDecodedBytes}
	headerBytes := uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(model.ColumnID(0)))
	if err := budget.chargeMul(uint64(source.columns), headerBytes); err != nil {
		return nil, err
	}
	columns := make([]model.ColumnID, source.columns)
	batch := columnar.Batch{Length: source.rows, Columns: make([]columnar.Vector, source.columns)}
	for i := range columns {
		columns[i] = model.ColumnID(binary.LittleEndian.Uint32(source.descriptors[i*paxColumnSize:]))
		vector, err := source.Column(ctx, columns[i])
		if err != nil {
			return nil, err
		}
		batch.Columns[i] = vector
	}
	selected, used, err := gatherDecodedBlock(ctx, &batch, order, budget.limit-budget.used)
	if err != nil {
		return nil, err
	}
	if err := budget.charge(used); err != nil {
		return nil, err
	}
	versions, err := gatherVersions(ctx, source.vis, order, &budget)
	if err != nil {
		return nil, err
	}
	limits.MaxDecodedBytes -= budget.used
	if limits.MaxDecodedBytes == 0 {
		return nil, columnar.ErrBatchBudget
	}
	return encodeBlock(ctx, source.target, versions, &selected, columns, limits)
}

func gatherVersions(ctx context.Context, visibility []byte, order []uint32, budget *encBudget) (blockVersions, error) {
	rowBytes := uint64(unsafe.Sizeof(columnar.Range{})) + uint64(unsafe.Sizeof(model.Timestamp(0))) + 8 + uint64(unsafe.Sizeof(storage.MutationOp(0)))
	if err := budget.chargeMul(uint64(len(order)), rowBytes); err != nil {
		return blockVersions{}, err
	}
	keyType := model.Type{ID: 17, Storage: model.TypeBytes}
	versions := blockVersions{
		Keys:     columnar.Vector{Type: keyType, Length: len(order), Data: visibility, Ranges: make([]columnar.Range, len(order))},
		CommitTS: make([]model.Timestamp, len(order)), Ordinals: make([]uint64, len(order)), Kinds: make([]storage.MutationOp, len(order)),
	}
	for i, lane := range order {
		if err := ctx.Err(); err != nil {
			return blockVersions{}, err
		}
		record := visibility[int(lane)*paxVersionSize:]
		versions.CommitTS[i] = model.Timestamp(int64(binary.LittleEndian.Uint64(record)))
		versions.Ordinals[i] = binary.LittleEndian.Uint64(record[8:])
		versions.Kinds[i] = storage.MutationOp(record[16])
		versions.Keys.Ranges[i] = columnar.Range{Offset: binary.LittleEndian.Uint32(record[24:]), Length: binary.LittleEndian.Uint32(record[28:])}
		if versions.Kinds[i] == storage.MutationDeleteRange {
			if versions.EndKeys.Ranges == nil {
				if err := budget.chargeMul(uint64(len(order)), uint64(unsafe.Sizeof(columnar.Range{}))); err != nil {
					return blockVersions{}, err
				}
				versions.EndKeys = columnar.Vector{Type: keyType, Length: len(order), Data: visibility, Ranges: make([]columnar.Range, len(order))}
			}
			versions.EndKeys.Ranges[i] = columnar.Range{Offset: binary.LittleEndian.Uint32(record[32:]), Length: binary.LittleEndian.Uint32(record[36:])}
		}
	}
	return versions, nil
}
