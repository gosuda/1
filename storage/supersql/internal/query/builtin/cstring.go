package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func copyCString(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	text := arguments[0].StrVal
	for start := 0; start < len(text); {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		end := start + min(cancellationChunk, len(text)-start)
		if offset := strings.IndexByte(text[start:end], 0); offset >= 0 {
			return ownedText(ctx, budget, text[:start+offset])
		}
		start = end
	}
	return ownedText(ctx, budget, text)
}
