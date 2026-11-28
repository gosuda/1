package aggregate

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const stringAggregateCopyBytes = 32 << 10

func stringAggregateLength(value model.Value) int {
	if value.IsNull {
		return 0
	}
	if value.Type == model.TypeString {
		return len(value.StrVal)
	}
	return len(value.BytesVal)
}

func (c *cursor) copyStringAggregate(ctx context.Context, target []byte, value model.Value) error {
	for offset := 0; offset < len(target); offset += stringAggregateCopyBytes {
		if err := c.check(ctx); err != nil {
			return err
		}
		end := min(offset+stringAggregateCopyBytes, len(target))
		if value.Type == model.TypeString {
			copy(target[offset:end], value.StrVal[offset:end])
		} else {
			copy(target[offset:end], value.BytesVal[offset:end])
		}
	}
	return nil
}

func (c *cursor) advanceStringAggregate(ctx context.Context, state *transition, arguments []model.Value) error {
	buffer := state.value.BytesVal
	valueBytes := stringAggregateLength(arguments[0])
	delimiterBytes := 0
	if state.count != 0 {
		delimiterBytes = stringAggregateLength(arguments[1])
	}
	limit := int(c.options.MaxRecordBytes)
	if valueBytes > limit-len(buffer) || delimiterBytes > limit-len(buffer)-valueBytes {
		return resource("string_agg result exceeds byte limit", model.ErrValueBudget)
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	length := len(buffer) + delimiterBytes + valueBytes
	if length > cap(buffer) {
		// Both buffers fit the admitted 2M per-call state allowance. Do not
		// let append's implicit growth allocate past the explicit byte limit.
		capacity := min(limit, max(length, max(64, cap(buffer)*2)))
		next := make([]byte, length, capacity)
		if err := c.copyStringAggregate(ctx, next[:len(buffer)], model.BytesVal(buffer)); err != nil {
			return err
		}
		buffer = next
	} else {
		buffer = buffer[:length]
	}
	// Only the unpublished tail is writable until every copy succeeds. A
	// failed Advance leaves the prior result's bytes, length and count intact.
	endDelimiter := len(state.value.BytesVal) + delimiterBytes
	if err := c.copyStringAggregate(ctx, buffer[len(state.value.BytesVal):endDelimiter], arguments[1]); err != nil {
		return err
	}
	if err := c.copyStringAggregate(ctx, buffer[endDelimiter:], arguments[0]); err != nil {
		return err
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	state.value = model.BytesVal(buffer)
	state.count++
	return nil
}

func (c *cursor) finishStringAggregate(ctx context.Context, call compiledCall, state *transition) (model.Value, error) {
	if state.count == 0 {
		return model.NullVal(call.output.Storage), nil
	}
	if call.output.Storage == model.TypeBytes {
		return state.value, nil
	}
	if err := c.check(ctx); err != nil {
		return model.Value{}, err
	}
	// Builder owns the result separately from the mutable transition. Its
	// rounded capacity is bounded by 2M and admitted before finalization.
	var result strings.Builder
	result.Grow(len(state.value.BytesVal))
	for offset := 0; offset < len(state.value.BytesVal); offset += stringAggregateCopyBytes {
		if err := c.check(ctx); err != nil {
			return model.Value{}, err
		}
		end := min(offset+stringAggregateCopyBytes, len(state.value.BytesVal))
		_, _ = result.Write(state.value.BytesVal[offset:end])
	}
	if err := c.check(ctx); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(result.String()), nil
}
