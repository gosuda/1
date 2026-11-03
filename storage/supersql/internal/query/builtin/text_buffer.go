package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type textBuffer struct {
	builder  strings.Builder
	capacity int64
	budget   int64
}

func (output *textBuffer) reserve(ctx context.Context, additional int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	size := int64(output.builder.Len()) + additional
	if err := outputBudget(size, output.budget); err != nil {
		return err
	}
	if size <= output.capacity {
		return nil
	}
	capacity := min(output.budget, max(size, max(64, 2*output.capacity)))
	// Grow only an empty builder: growing a populated one hides an unbounded copy
	// and applies its own capacity multiplier. Retain just the old and new buffers.
	previous := output.builder.String()
	output.builder.Reset()
	output.builder.Grow(int(capacity))
	output.capacity = capacity
	return copyArrayText(ctx, &output.builder, previous, false)
}

func (output *textBuffer) writeByte(ctx context.Context, value byte) error {
	if err := output.reserve(ctx, 1); err != nil {
		return err
	}
	output.builder.WriteByte(value)
	return nil
}

func (output *textBuffer) finish(ctx context.Context) (model.Value, error) {
	text := output.builder.String()
	if output.capacity == int64(len(text)) {
		return model.StringVal(text), nil
	}
	// Returned payload accounting must not retain geometric growth slack.
	var compact strings.Builder
	compact.Grow(len(text))
	if err := copyArrayText(ctx, &compact, text, false); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(compact.String()), nil
}

// reserveTemporary admits the old buffer, its replacement, and a live rendered
// argument together. Temporary bytes do not include borrowed input storage.
func (output *textBuffer) reserveTemporary(ctx context.Context, additional, temporary int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	limit := min(output.budget, int64((1<<30)-5))
	length := int64(output.builder.Len())
	invalidState := additional < 0 || temporary < 0 || output.capacity < length || output.capacity > limit
	if invalidState || temporary > limit-output.capacity || additional > limit-length {
		return sqlError("54000", "builtin output byte budget exceeded")
	}
	size := length + additional
	if size <= output.capacity {
		return nil
	}
	available := limit - output.capacity - temporary
	if size > available {
		return sqlError("54000", "builtin output byte budget exceeded")
	}
	capacity := size
	if output.capacity != 0 {
		capacity = min(available, max(size, 2*output.capacity))
	}
	previous := output.builder.String()
	output.builder.Reset()
	output.builder.Grow(int(capacity))
	output.capacity = capacity
	return copyArrayText(ctx, &output.builder, previous, false)
}

func (output *textBuffer) finishTemporary(ctx context.Context) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	length := int64(output.builder.Len())
	limit := min(output.budget, int64((1<<30)-5))
	if output.capacity < length || output.capacity > limit {
		return model.Value{}, sqlError("54000", "builtin output byte budget exceeded")
	}
	if output.capacity == length {
		return model.StringVal(output.builder.String()), nil
	}
	if length > limit-output.capacity {
		return model.Value{}, sqlError("54000", "builtin output byte budget exceeded")
	}
	return output.finish(ctx)
}

func copyArrayText(ctx context.Context, output *strings.Builder, text string, escape bool) error {
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(text))
		span := start
		if escape {
			for i := start; i < end; i++ {
				if text[i] == '"' || text[i] == '\\' {
					output.WriteString(text[span:i])
					output.WriteByte('\\')
					span = i
				}
			}
		}
		output.WriteString(text[span:end])
	}
	return nil
}
