package schema

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Entry borrows a complete base row in increasing ColumnID order. No entry is
// published on failure or cancellation, and neither row nor plan is modified.
func (p *IndexPlan) Entry(ctx context.Context, row *model.Row) (*IndexEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, invalidIndexPlan()
	}
	if row == nil || row.Deleted {
		return nil, nil
	}
	if err := p.validateRow(ctx, row); err != nil {
		return nil, err
	}
	if p.predicate != nil {
		predicate, err := p.predicate.Evaluate(ctx, row.Values, model.ExpressionRuntime{})
		if err != nil {
			return nil, err
		}
		if _, err := model.ValueBytes(ctx, predicate, p.limits.EntryBytes); err != nil {
			return nil, indexPlanLimitError(err)
		}
		column := storage.ColumnLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}, Nullable: true}
		if err := validateIndexPlanValue(ctx, predicate, column); err != nil {
			return nil, err
		}
		if predicate.IsNull || predicate.IntVal == 0 {
			return nil, ctx.Err()
		}
	}
	budget := indexPlanBudget{limit: p.limits.EntryBytes}
	if err := budget.add(1, uint64(unsafe.Sizeof(IndexEntry{}))); err != nil {
		return nil, err
	}
	count := len(p.layout.Columns)
	if err := budget.add(uint64(count), uint64(unsafe.Sizeof(model.Value{}))+uint64(unsafe.Sizeof(model.ColumnID(0)))); err != nil {
		return nil, err
	}
	entry := &IndexEntry{ColumnIDs: make([]model.ColumnID, count), Values: make([]model.Value, count)}
	for i := range entry.ColumnIDs {
		entry.ColumnIDs[i] = model.ColumnID(i + 1)
	}
	hasNull := false
	for i, key := range p.keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var value model.Value
		if key.expression != nil {
			var err error
			value, err = key.expression.Evaluate(ctx, row.Values, model.ExpressionRuntime{})
			if err != nil {
				return nil, err
			}
		} else {
			value = row.Values[key.column]
		}
		owned, err := copyIndexPlanValue(ctx, value, p.layout.Columns[i], &budget)
		if err != nil {
			return nil, err
		}
		entry.Values[i] = owned
		hasNull = hasNull || owned.IsNull
	}
	// Deferred-key storage retains the base owner in the physical key. A
	// pre-publication build still enforces uniqueness separately, while a
	// published deferrable constraint validates the final transaction overlay.
	disambiguator := model.BytesVal(nil)
	if !p.unique || p.deferred || p.disambiguated || (hasNull && !p.nullsNotDistinct) {
		disambiguator.BytesVal = row.Key
	}
	for offset, value := range [...]model.Value{disambiguator, model.BytesVal(row.Key)} {
		slot := len(p.keys) + offset
		owned, err := copyIndexPlanValue(ctx, value, p.layout.Columns[slot], &budget)
		if err != nil {
			return nil, err
		}
		entry.Values[slot] = owned
	}
	for i, source := range p.includes {
		slot := len(p.keys) + 2 + i
		owned, err := copyIndexPlanValue(ctx, row.Values[source], p.layout.Columns[slot], &budget)
		if err != nil {
			return nil, err
		}
		entry.Values[slot] = owned
	}
	key, err := p.encoder.EncodePrefix(ctx, p.keyLayout, entry.Values[:len(p.keyLayout)], budget.limit-budget.used)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, &IndexKeyImplementationError{Cause: storage.ErrInvalidEncoding}
	}
	if err := budget.add(uint64(cap(key)), 1); err != nil {
		return nil, err
	}
	entry.Key = key
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entry, nil
}

func (p *IndexPlan) validateRow(ctx context.Context, row *model.Row) error {
	if len(row.Values) != len(p.baseColumns) || len(row.Key) == 0 {
		return indexPlanValueError(0, storage.ErrUnnormalizedBatch)
	}
	budget := indexPlanBudget{limit: p.limits.EntryBytes}
	if err := budget.add(1, uint64(unsafe.Sizeof(model.Row{}))); err != nil {
		return err
	}
	if err := budget.add(uint64(len(row.Key)), 1); err != nil {
		return err
	}
	if err := budget.add(uint64(len(row.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return err
	}
	for i, value := range row.Values {
		size, err := model.ValueBytes(ctx, value, budget.limit-budget.used)
		if err != nil {
			return indexPlanLimitError(err)
		}
		budget.used += size
		if err := validateIndexPlanValue(ctx, value, p.baseColumns[i]); err != nil {
			return err
		}
	}
	scratch := indexPlanBudget{limit: p.limits.EntryBytes}
	if err := scratch.add(uint64(len(p.baseKeyColumns)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return err
	}
	values := make([]model.Value, len(p.baseKeyColumns))
	for i, column := range p.baseKeyColumns {
		values[i] = row.Values[column]
	}
	key, err := p.encoder.EncodePrefix(ctx, p.baseKeyLayout, values, scratch.limit-scratch.used)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scratch.add(uint64(cap(key)), 1); err != nil {
		return err
	}
	if !bytes.Equal(key, row.Key) {
		return &model.Error{Code: "XX001", Message: "secondary index source row has an inconsistent primary key", Cause: storage.ErrCorruptStorage}
	}
	return ctx.Err()
}

func copyIndexPlanValue(ctx context.Context, value model.Value, column storage.ColumnLayout, budget *indexPlanBudget) (model.Value, error) {
	owned, size, err := model.CopyValue(ctx, value, budget.limit-budget.used)
	if err != nil {
		return model.Value{}, indexPlanLimitError(err)
	}
	budget.used += size
	if err := validateIndexPlanValue(ctx, owned, column); err != nil {
		return model.Value{}, err
	}
	return owned, nil
}

func validateIndexPlanValue(ctx context.Context, value model.Value, column storage.ColumnLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	layout := model.TypeLayout{Type: column.Type, Children: column.Children}
	if err := storage.ValidateStoredValue(ctx, value, layout); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return indexPlanValueError(column.ID, err)
	}
	if value.IsNull && !column.Nullable {
		return &model.Error{Code: "23502", Message: fmt.Sprintf("null value in secondary index column %d", column.ID), Cause: storage.ErrUnnormalizedBatch}
	}
	if err := validateIndexPlanValueShape(ctx, value); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return indexPlanValueError(column.ID, err)
	}
	return nil
}

// CopyValue/ValueBytes has already bounded the complete payload tree, including
// inactive NULL payloads, before storage's layout-directed traversal reaches it.
func validateIndexPlanValueShape(ctx context.Context, value model.Value) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.IsNull {
		return nil
	}
	if value.Type == model.TypeBoolean && value.IntVal != 0 && value.IntVal != 1 {
		return storage.ErrUnnormalizedBatch
	}
	if value.Type == model.TypeComposite && len(value.Dimensions) != 0 {
		return storage.ErrUnnormalizedBatch
	}
	if value.Type == model.TypeArray || value.Type == model.TypeComposite {
		for _, child := range value.ArrayVal {
			if err := validateIndexPlanValueShape(ctx, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func indexPlanValueError(column model.ColumnID, cause error) error {
	return &model.Error{Code: "42804", Message: fmt.Sprintf("invalid secondary index input for column %d", column), Cause: errors.Join(storage.ErrUnnormalizedBatch, cause)}
}
