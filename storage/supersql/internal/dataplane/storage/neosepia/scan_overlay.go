package neosepia

import (
	"context"
	"errors"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Overlay values are already typed; only requested and predicate columns enter
// reusable vectors. Committed columns continue borrowing their original PAX block.
func (cursor *paxScanCursor) loadOwn(ctx context.Context, mutation *storage.Mutation) error {
	if mutation.Target != cursor.spec.Target {
		return storage.ErrInvalidTarget
	}
	if len(mutation.ColumnIDs) != len(mutation.Values) {
		return storage.ErrUnnormalizedBatch
	}
	for i, id := range mutation.ColumnIDs {
		if id == 0 || (i > 0 && mutation.ColumnIDs[i-1] >= id) {
			return storage.ErrUnnormalizedBatch
		}
	}
	if cursor.own == nil {
		if err := cursor.initializeOwn(); err != nil {
			return err
		}
	}
	for i, slot := range cursor.ownSlots {
		position, ok := slices.BinarySearch(mutation.ColumnIDs, cursor.ownIDs[i])
		if !ok {
			return storage.ErrUnnormalizedBatch
		}
		value := mutation.Values[position]
		if value.IsNull && !cursor.spec.Layout[slot].Nullable {
			return storage.ErrUnnormalizedBatch
		}
		cursor.ownValues[i] = value
	}
	cursor.ownBatch.Reset()
	err := cursor.own.Append(ctx, cursor.ownValues)
	clear(cursor.ownValues)
	if err != nil {
		return errors.Join(err, cursor.own.Publish(&cursor.ownBatch))
	}
	return cursor.own.Publish(&cursor.ownBatch)
}

func (cursor *paxScanCursor) initializeOwn() error {
	var predicateIDs []model.ColumnID
	if cursor.spec.Predicate != nil {
		predicateIDs = cursor.spec.Predicate.Columns()
	}
	count := 0
	for _, column := range cursor.spec.Layout {
		if slices.Contains(cursor.spec.Projection, column.ID) || slices.Contains(predicateIDs, column.ID) {
			count++
		}
	}
	fixed := uint64(unsafe.Sizeof(columnar.Builder{})) + uint64(count)*(uint64(unsafe.Sizeof(model.TypeLayout{}))+uint64(unsafe.Sizeof(model.Value{}))+uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(int(0)))+uint64(unsafe.Sizeof(model.ColumnID(0))))
	if fixed > cursor.spec.OverlayBlockBytes {
		return columnar.ErrBatchBudget
	}
	layouts := make([]model.TypeLayout, count)
	ids := make([]model.ColumnID, count)
	slots := make([]int, count)
	next := 0
	for slot, column := range cursor.spec.Layout {
		if !slices.Contains(cursor.spec.Projection, column.ID) && !slices.Contains(predicateIDs, column.ID) {
			continue
		}
		layouts[next] = model.TypeLayout{Type: column.Type, Children: column.Children}
		ids[next] = column.ID
		slots[next] = slot
		next++
	}
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: cursor.spec.MaxRows, MaxBytes: cursor.spec.OverlayBlockBytes - fixed})
	if err != nil {
		return err
	}
	batch := columnar.Batch{Columns: make([]columnar.Vector, count)}
	if err := builder.Publish(&batch); err != nil {
		return errors.Join(err, builder.Close())
	}
	cursor.own = builder
	cursor.ownBatch = batch
	cursor.ownIDs = ids
	cursor.ownSlots = slots
	cursor.ownValues = make([]model.Value, count)
	return nil
}
