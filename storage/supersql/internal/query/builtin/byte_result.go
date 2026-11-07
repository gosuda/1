package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// byteResultBuffer holds admission until the synchronous callback returns.
// value transfers the backing bytes to the result; close releases only admission.
type byteResultBuffer struct {
	data  []byte
	claim columnar.Allocation
}

func (buffer *byteResultBuffer) close() {
	if buffer.claim != nil {
		buffer.claim.Release()
		buffer.claim = nil
	}
	buffer.data = nil
}
func (buffer *byteResultBuffer) value() model.Value { return model.BytesVal(buffer.data) }

func reserveOutputBytes(ctx context.Context, size uint64, budget CallBudget) (columnar.Allocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size > uint64(math.MaxInt64) {
		return nil, sqlError("54000", "builtin result exceeds memory limit")
	}
	if err := outputBudget(int64(size), budget.OutputBytes); err != nil {
		return nil, err
	}
	if size > budget.MemoryBytes {
		return nil, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if budget.Allocator != nil {
		return budget.Allocator.Reserve(size)
	}
	return nil, nil
}
func newByteResult(ctx context.Context, size uint64, budget CallBudget) (byteResultBuffer, error) {
	claim, err := reserveOutputBytes(ctx, size, budget)
	if err != nil {
		return byteResultBuffer{}, err
	}
	if err := ctx.Err(); err != nil {
		if claim != nil {
			claim.Release()
		}
		return byteResultBuffer{}, err
	}
	return byteResultBuffer{make([]byte, int(size)), claim}, nil
}
