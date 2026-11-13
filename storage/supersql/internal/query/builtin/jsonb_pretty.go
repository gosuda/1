package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBPretty(signature Signature) scalarKernel {
	if signature.ID != 3306 {
		return nil
	}
	return jsonbPretty
}

func jsonbPretty(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	document, err := parseJSONBDocument(ctx, args[0].StrVal, budget)
	if err != nil {
		return model.Value{}, err
	}
	size, err := walkJSONBPretty(ctx, document, nil)
	if err != nil {
		return model.Value{}, err
	}
	if err := outputBudget(document.stateBytes+size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	if _, err := walkJSONBPretty(ctx, document, &output); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(output.String()), ctx.Err()
}

// A nil output sizes the same traversal without constructing compact JSON or
// an intermediate token stream. The parse stack is reused by both passes.
func walkJSONBPretty(ctx context.Context, document jsonbDocument, output *strings.Builder) (int64, error) {
	nodes, frames := document.nodes, document.frames
	current, depth := 0, 0
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		node := &nodes[current]
		if depth > 0 {
			prefix, err := appendJSONBPrettyPrefix(ctx, output, depth, nodes[frames[depth-1].parent].kind, node.key)
			if err != nil {
				return 0, err
			}
			size += prefix
		}
		switch node.kind {
		case jsonTextObject, jsonTextArray:
			size++
			appendJSONBPrettyOpen(output, node.kind)
			if node.child >= 0 {
				frames[depth].parent = current
				depth++
				current = node.child
				continue
			}
			// PostgreSQL emits a closing newline even for empty containers.
			if err := appendJSONBPrettyClose(ctx, output, node.kind, depth); err != nil {
				return 0, err
			}
			size += 2 + int64(depth)*4
		default:
			size += node.bytes
			if err := appendJSONBPrettyScalar(ctx, output, node); err != nil {
				return 0, err
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if depth == 0 {
				if output != nil {
					return int64(output.Len()), nil
				}
				return size, nil
			}
			if sibling := nodes[current].next; sibling >= 0 {
				size++
				if output != nil {
					output.WriteByte(',')
				}
				current = sibling
				break
			}
			depth--
			current = frames[depth].parent
			if err := appendJSONBPrettyClose(ctx, output, nodes[current].kind, depth); err != nil {
				return 0, err
			}
			size += 2 + int64(depth)*4
		}
	}
}

func appendJSONBPrettyClose(ctx context.Context, output *strings.Builder, kind jsonTextKind, depth int) error {
	if err := appendJSONBIndent(ctx, output, depth); err != nil {
		return err
	}
	if output != nil {
		if kind == jsonTextObject {
			output.WriteByte('}')
		} else {
			output.WriteByte(']')
		}
	}
	return nil
}

func appendJSONBIndent(ctx context.Context, output *strings.Builder, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	output.WriteByte('\n')
	const spaces = "                                                                "
	for remaining := depth * 4; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(remaining, len(spaces))
		output.WriteString(spaces[:count])
		remaining -= count
	}
	return nil
}

func appendJSONBPrettyPrefix(ctx context.Context, output *strings.Builder, depth int, parent jsonTextKind, key string) (int64, error) {
	if err := appendJSONBIndent(ctx, output, depth); err != nil {
		return 0, err
	}
	size := 1 + int64(depth)*4
	if parent != jsonTextObject {
		return size, nil
	}
	if output != nil {
		return size, appendJSONBMemberKey(ctx, output, parent, key)
	}
	keyBytes, err := jsonQuotedBytes(ctx, key)
	return size + keyBytes + 2, err
}

func appendJSONBPrettyOpen(output *strings.Builder, kind jsonTextKind) {
	if output == nil {
		return
	}
	if kind == jsonTextObject {
		output.WriteByte('{')
	} else {
		output.WriteByte('[')
	}
}

func appendJSONBPrettyScalar(ctx context.Context, output *strings.Builder, node *jsonbNode) error {
	if output == nil {
		return nil
	}
	switch node.kind {
	case jsonTextString:
		return appendJSONQuoted(ctx, output, node.text)
	case jsonTextNumber:
		number, err := inspectJSONBNumber(ctx, node.text)
		if err != nil {
			return err
		}
		return number.append(ctx, output)
	default:
		output.WriteString(node.text)
		return ctx.Err()
	}
}
