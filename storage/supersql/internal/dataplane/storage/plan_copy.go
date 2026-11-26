package storage

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// CopyPushdownRequest owns all visible metadata and datum payloads. The byte
// count excludes the returned header; failed copies expose no partial request.
func CopyPushdownRequest(ctx context.Context, source PushdownRequest, maxBytes uint64) (PushdownRequest, uint64, error) {
	budget := planCopyBudget{ctx: ctx, maximum: maxBytes}
	owned, err := budget.request(source)
	if err != nil {
		return PushdownRequest{}, 0, err
	}
	return owned, budget.used, nil
}

type planCopyBudget struct {
	ctx           context.Context
	maximum, used uint64
}

func copyPlanSlice[T any](budget *planCopyBudget, source []T) ([]T, error) {
	if err := budget.ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, nil
	}
	size := uint64(unsafe.Sizeof(*new(T)))
	if size != 0 && uint64(len(source)) > (budget.maximum-budget.used)/size {
		return nil, model.ErrValueBudget
	}
	budget.used += uint64(len(source)) * size
	result := make([]T, len(source))
	copy(result, source)
	return result, nil
}

func (budget *planCopyBudget) values(source []model.Value) ([]model.Value, error) {
	result, err := copyPlanSlice(budget, source)
	if err != nil {
		return nil, err
	}
	for i, value := range source {
		result[i], err = budget.value(value)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (budget *planCopyBudget) value(source model.Value) (model.Value, error) {
	result, used, err := model.CopyValue(budget.ctx, source, budget.maximum-budget.used)
	if err != nil {
		return model.Value{}, err
	}
	budget.used += used
	return result, nil
}

func (budget *planCopyBudget) predicate(source Predicate) (Predicate, error) {
	result := Predicate{root: source.root}
	var err error
	result.nodes, err = copyPlanSlice(budget, source.nodes)
	if err != nil {
		return Predicate{}, err
	}
	result.edges, err = copyPlanSlice(budget, source.edges)
	if err != nil {
		return Predicate{}, err
	}
	result.constants, err = budget.values(source.constants)
	if err != nil {
		return Predicate{}, err
	}
	return result, nil
}

func (budget *planCopyBudget) request(source PushdownRequest) (PushdownRequest, error) {
	result := source
	var err error
	result.Layout, err = copyPlanSlice(budget, source.Layout)
	if err != nil {
		return PushdownRequest{}, err
	}
	for i, column := range source.Layout {
		layout, used, err := model.CopyTypeLayout(budget.ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, budget.maximum-budget.used)
		if err != nil {
			return PushdownRequest{}, err
		}
		budget.used += used
		result.Layout[i].Children = layout.Children
		result.Layout[i].MissingValue, err = budget.value(column.MissingValue)
		if err != nil {
			return PushdownRequest{}, err
		}
	}
	var keyBytes uint64
	result.KeyLayout, keyBytes, err = CopyKeyLayout(budget.ctx, source.KeyLayout, budget.maximum-budget.used)
	if err != nil {
		return PushdownRequest{}, err
	}
	budget.used += keyBytes
	result.ColumnIDs, err = copyPlanSlice(budget, source.ColumnIDs)
	if err != nil {
		return PushdownRequest{}, err
	}
	result.Spans, err = copyPlanSlice(budget, source.Spans)
	if err != nil {
		return PushdownRequest{}, err
	}
	for i, span := range source.Spans {
		result.Spans[i].Lower.Values, err = budget.values(span.Lower.Values)
		if err != nil {
			return PushdownRequest{}, err
		}
		result.Spans[i].Upper.Values, err = budget.values(span.Upper.Values)
		if err != nil {
			return PushdownRequest{}, err
		}
	}
	result.EncodedSpans, err = copyPlanSlice(budget, source.EncodedSpans)
	if err != nil {
		return PushdownRequest{}, err
	}
	for i, span := range source.EncodedSpans {
		result.EncodedSpans[i].Lower.Key, err = copyPlanSlice(budget, span.Lower.Key)
		if err != nil {
			return PushdownRequest{}, err
		}
		result.EncodedSpans[i].Upper.Key, err = copyPlanSlice(budget, span.Upper.Key)
		if err != nil {
			return PushdownRequest{}, err
		}
	}
	result.Predicate, err = budget.predicate(source.Predicate)
	if err != nil {
		return PushdownRequest{}, err
	}
	return result, nil
}
