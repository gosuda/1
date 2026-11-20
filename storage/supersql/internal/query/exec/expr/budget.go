package expr

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func valueBytes(ctx context.Context, value model.Value, limit uint64) (uint64, error) {
	size, err := model.ValueBytes(ctx, value, limit)
	if err != nil && (errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth)) {
		return 0, ErrLimit
	}
	return size, err
}
