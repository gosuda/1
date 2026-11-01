package builtin

import (
	"context"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (document jsonbDocument) deleteJSONBPath(ctx context.Context, path []model.Value, budget int64) error {
	parent := 0
	for level := range path {
		if err := ctx.Err(); err != nil {
			return err
		}
		step := &path[level]
		// setPath checks NULL before dispatching even a reached scalar or empty
		// nested container. Missing prefixes never reach their suffixes.
		if step.IsNull {
			return jsonbDeletePathNullError(ctx, level, document.stateBytes, budget)
		}
		if step.Type != model.TypeString {
			return sqlError("42804", "JSON path requires text elements")
		}
		node := &document.nodes[parent]
		var link *int
		switch node.kind {
		case jsonTextObject:
			link = &node.child
			for *link >= 0 {
				order, err := compareJSONBKeys(ctx, document.nodes[*link].key, step.StrVal)
				if err != nil {
					return err
				}
				if order == 0 {
					break
				}
				link = &document.nodes[*link].next
			}
			if *link < 0 {
				return nil
			}
		case jsonTextArray:
			index, valid, err := parseJSONPathIndex(ctx, step.StrVal)
			if err != nil {
				return err
			}
			if !valid {
				return jsonbDeletePathIndexError(ctx, level, step.StrVal, document.stateBytes, budget)
			}
			link, err = document.jsonbDeleteIndexLink(ctx, parent, index)
			if err != nil || link == nil {
				return err
			}
		default:
			return nil
		}
		if level == len(path)-1 {
			*link = document.nodes[*link].next
			return nil
		}
		parent = *link
	}
	return nil
}

func jsonbDeletePathNullError(ctx context.Context, level int, stateBytes, budget int64) error {
	var position [20]byte
	digits := strconv.AppendInt(position[:0], int64(level)+1, 10)
	const prefix, suffix = "path element at position ", " is null"
	messageBytes := int64(len(prefix) + len(digits) + len(suffix))
	if err := outputBudget(stateBytes+messageBytes, budget); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var message strings.Builder
	message.Grow(int(messageBytes))
	message.WriteString(prefix)
	message.Write(digits)
	message.WriteString(suffix)
	return sqlError("22004", message.String())
}

func jsonbDeletePathIndexError(ctx context.Context, level int, text string, stateBytes, budget int64) error {
	// PostgreSQL formats the C-string index, including its NUL truncation.
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(text))
		if zero := strings.IndexByte(text[start:end], 0); zero >= 0 {
			text = text[:start+zero]
			break
		}
	}
	var position [20]byte
	digits := strconv.AppendInt(position[:0], int64(level)+1, 10)
	const prefix, detail = "path element at position ", " is not an integer: \""
	messageBytes := int64(len(prefix)+len(digits)+len(detail)) + int64(len(text)) + 1
	if err := outputBudget(stateBytes+messageBytes, budget); err != nil {
		return err
	}
	var message strings.Builder
	message.Grow(int(messageBytes))
	message.WriteString(prefix)
	message.Write(digits)
	message.WriteString(detail)
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		message.WriteString(text[start:min(start+cancellationChunk, len(text))])
	}
	message.WriteByte('"')
	return sqlError("22P02", message.String())
}
