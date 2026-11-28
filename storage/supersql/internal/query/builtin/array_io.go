package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Per-call scratch; the compiled element callable remains immutable and shared.
type arrayElementCall struct {
	function  Callable
	arguments [3]model.Value
}

func (element *arrayElementCall) invoke(ctx context.Context, value model.Value, callBudget CallBudget) (model.Value, error) {
	element.arguments[0] = value
	result, err := element.function.Call(ctx, element.arguments[:element.function.signature.ArgumentCount], callBudget)
	element.arguments[0] = model.Value{}
	return result, err
}
