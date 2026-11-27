package storage

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// KeyLayoutBytes includes root column headers and physical child metadata.
func KeyLayoutBytes(ctx context.Context, source []KeyColumn, maxBytes uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	header := uint64(unsafe.Sizeof(KeyColumn{}))
	if uint64(len(source)) > maxBytes/header {
		return 0, model.ErrValueBudget
	}
	used := uint64(len(source)) * header
	for _, column := range source {
		if column.Type.ID == 0 || !column.Type.Storage.Valid() {
			return 0, ErrInvalidLayout
		}
		size, err := model.PhysicalTypeLayoutBytes(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, maxBytes-used)
		if err != nil {
			return 0, err
		}
		used += size
	}
	return used, nil
}

// CopyKeyLayout owns physical metadata only, after admitting the complete copy.
func CopyKeyLayout(ctx context.Context, source []KeyColumn, maxBytes uint64) ([]KeyColumn, uint64, error) {
	used, err := KeyLayoutBytes(ctx, source, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	if source == nil {
		return nil, 0, nil
	}
	result := make([]KeyColumn, len(source))
	remaining := used - uint64(len(source))*uint64(unsafe.Sizeof(KeyColumn{}))
	for i, column := range source {
		layout, size, err := model.CopyPhysicalTypeLayout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, remaining)
		if err != nil {
			return nil, 0, err
		}
		remaining -= size
		result[i] = column
		result[i].Children = layout.Children
	}
	return result, used, nil
}

// KeyLayoutsEqual compares physical declarations, not SQL presentation names.
func KeyLayoutsEqual(left, right []KeyColumn) bool {
	if len(left) != len(right) {
		return false
	}
	for i, column := range left {
		other := right[i]
		if column.ColumnID != other.ColumnID || column.Descending != other.Descending || column.NullsFirst != other.NullsFirst || !physicalTypeLayoutsEqual(model.TypeLayout{Type: column.Type, Children: column.Children}, model.TypeLayout{Type: other.Type, Children: other.Children}, 0) {
			return false
		}
	}
	return true
}

func physicalTypeLayoutsEqual(left, right model.TypeLayout, depth int) bool {
	if depth > 64 || left.Type != right.Type || len(left.Children) != len(right.Children) {
		return false
	}
	for i, child := range left.Children {
		if !physicalTypeLayoutsEqual(child, right.Children[i], depth+1) {
			return false
		}
	}
	return true
}
