package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func recordCodecResolutionFatal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, model.ErrValueBudget) {
		return true
	}
	var sql *model.Error
	return errors.As(err, &sql) && sql.Code == "54000"
}
