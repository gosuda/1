package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type pointReadLimits struct{ ScanBytes, RowBytes uint64 }

// The point result owns its key and values. Source ownership transfers even on
// error; block budgets belong to the source, independently of copied row bytes.
func readPAXPoint(ctx context.Context, source *mvccCursor, overlay storage.OverlayView, request storage.ReadRequest, limits pointReadLimits) (_ model.Row, err error) {
	if source == nil {
		return model.Row{}, storage.ErrInvalidScanPlan
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	cursor, err := newPAXScanCursor(ctx, source, paxScanSpec{Target: request.Target, Layout: request.Layout, Projection: request.ColumnIDs, MaxRows: 1, MaxBytes: limits.ScanBytes})
	if err != nil {
		return model.Row{}, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	if overlay != nil {
		view, err := overlay.Retain()
		if err != nil {
			return model.Row{}, err
		}
		defer func() { err = errors.Join(err, view.Close()) }()
		mutation, found, err := view.Lookup(ctx, request.Target, request.Key)
		if err != nil {
			return model.Row{}, err
		}
		if found {
			return copyOwnPoint(ctx, mutation, request, limits.RowBytes)
		}
	}
	var row versionRow
	for {
		row, err = source.Next(ctx)
		if err == io.EOF {
			return model.Row{}, storage.ErrRowNotFound
		}
		if err != nil {
			return model.Row{}, err
		}
		comparison := bytes.Compare(row.key, request.Key)
		if comparison < 0 {
			continue
		}
		if comparison > 0 {
			return model.Row{}, storage.ErrRowNotFound
		}
		break
	}
	if err := validateScanTarget(request.Target, row.lease.block.Target()); err != nil {
		return model.Row{}, err
	}
	cursor.lease = row.lease.retain()
	cursor.lanes = append(cursor.lanes, uint32(row.row))
	cursor.filter.Selection = cursor.lanes
	fixed := uint64(unsafe.Sizeof(model.Row{})) + uint64(len(row.key))
	if fixed > limits.RowBytes {
		return model.Row{}, columnar.ErrBatchBudget
	}
	valueSize := uint64(unsafe.Sizeof(model.Value{}))
	if uint64(len(request.ColumnIDs)) > (limits.RowBytes-fixed)/valueSize {
		return model.Row{}, columnar.ErrBatchBudget
	}
	used := fixed + uint64(len(request.ColumnIDs))*valueSize
	result := model.Row{Key: make([]byte, len(row.key)), CommitTS: row.timestamp, Ordinal: row.ordinal, Values: make([]model.Value, len(request.ColumnIDs))}
	copy(result.Key, row.key)
	for i, id := range request.ColumnIDs {
		vector, err := cursor.column(ctx, id)
		if err != nil {
			return model.Row{}, err
		}
		value, allocation, err := columnar.CopyValue(ctx, &vector, row.row, limits.RowBytes-used)
		if err != nil {
			return model.Row{}, err
		}
		result.Values[i] = value
		used += allocation
	}
	return result, nil
}

func copyOwnPoint(ctx context.Context, mutation storage.Mutation, request storage.ReadRequest, maxBytes uint64) (model.Row, error) {
	if err := ctx.Err(); err != nil {
		return model.Row{}, err
	}
	if mutation.Target != request.Target {
		return model.Row{}, storage.ErrInvalidTarget
	}
	if mutation.Op == storage.MutationDelete || mutation.Op == storage.MutationDeleteRange {
		return model.Row{}, storage.ErrRowNotFound
	}
	if mutation.Op != storage.MutationUpsert || !bytes.Equal(mutation.Key, request.Key) || len(mutation.ColumnIDs) != len(mutation.Values) {
		return model.Row{}, storage.ErrUnnormalizedBatch
	}
	for i, id := range mutation.ColumnIDs {
		if id == 0 || (i > 0 && mutation.ColumnIDs[i-1] >= id) {
			return model.Row{}, storage.ErrUnnormalizedBatch
		}
	}
	fixed := uint64(unsafe.Sizeof(model.Row{})) + uint64(len(mutation.Key))
	valueSize := uint64(unsafe.Sizeof(model.Value{}))
	if fixed > maxBytes || uint64(len(request.ColumnIDs)) > (maxBytes-fixed)/valueSize {
		return model.Row{}, columnar.ErrBatchBudget
	}
	used := fixed + uint64(len(request.ColumnIDs))*valueSize
	result := model.Row{Key: make([]byte, len(mutation.Key)), Ordinal: mutation.Ordinal, Values: make([]model.Value, len(request.ColumnIDs))}
	copy(result.Key, mutation.Key)
	for i, id := range request.ColumnIDs {
		position, ok := slices.BinarySearch(mutation.ColumnIDs, id)
		if !ok {
			return model.Row{}, storage.ErrUnnormalizedBatch
		}
		layout, _ := scanColumnLayout(request.Layout, id)
		value := mutation.Values[position]
		if value.IsNull && !layout.Nullable {
			return model.Row{}, storage.ErrUnnormalizedBatch
		}
		if err := storage.ValidateStoredValue(ctx, value, model.TypeLayout{Type: layout.Type, Children: layout.Children}); err != nil {
			return model.Row{}, err
		}
		owned, allocated, err := model.CopyValue(ctx, value, maxBytes-used)
		if errors.Is(err, model.ErrValueBudget) {
			return model.Row{}, errors.Join(columnar.ErrBatchBudget, err)
		}
		if err != nil {
			return model.Row{}, err
		}
		result.Values[i] = owned
		used += allocated
	}
	return result, nil
}

func validateScanTarget(requested, stored storage.ObjectRef) error {
	if stored.ObjectID != requested.ObjectID || stored.Generation != requested.Generation || stored.EncodingVersion != requested.EncodingVersion {
		return storage.ErrInvalidTarget
	}
	if stored.DescriptorVersion > requested.DescriptorVersion {
		return storage.ErrInvalidLayout
	}
	return nil
}
