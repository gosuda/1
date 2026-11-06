package schema

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func NewTableKeyScan(ctx context.Context, reader storage.SnapshotReader, decoder storage.KeyDecoder, encoder storage.KeyEncoder, job Job, target storage.ObjectRef, columns []storage.ColumnLayout, keyLayout []storage.KeyColumn, limits BackfillScanLimits) (*IndexKeyScan, error) {
	if reader == nil || decoder == nil || encoder == nil || target.ObjectID != job.TableID {
		return nil, ErrJobRecord
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	wrongJob := job.Kind == AddIndexJob || job.State != JobBackfilling
	invalidProtection := job.SnapshotTS <= 0 || job.ProtectedTimestamp != job.SnapshotTS
	if wrongJob || invalidProtection {
		return nil, ErrJobRecord
	}
	columnIDs := make([]model.ColumnID, len(keyLayout))
	for i, key := range keyLayout {
		if key.ColumnID == 0 || i > 0 && key.ColumnID == keyLayout[i-1].ColumnID {
			return nil, storage.ErrInvalidLayout
		}
		position, ok := slices.BinarySearchFunc(columns, key.ColumnID, func(column storage.ColumnLayout, id model.ColumnID) int {
			switch {
			case column.ID < id:
				return -1
			case column.ID > id:
				return 1
			default:
				return 0
			}
		})
		if !ok || !storage.KeyLayoutsEqual([]storage.KeyColumn{{Type: columns[position].Type, Children: columns[position].Children}}, []storage.KeyColumn{{Type: key.Type, Children: key.Children}}) {
			return nil, storage.ErrInvalidLayout
		}
		columnIDs[i] = key.ColumnID
	}
	return newKeyScan(ctx, reader, decoder, encoder, job, target, columns, keyLayout, columnIDs, limits)
}
