package builtin

import (
	"context"
	"strings"
	"unsafe"
)

type jsonbNode struct {
	kind         jsonTextKind
	child, next  int
	text, key    string
	bytes        int64
	storageBytes int64
}

type jsonbFrame struct {
	parent, last int
	key          string
}

type jsonbDocument struct {
	nodes      []jsonbNode
	frames     []jsonbFrame
	stateBytes int64
}

// Parsing owns one exact node slice, one depth-bounded frame slice, and one
// decoded-string arena. Output never aliases these or the borrowed input.
func parseJSONBDocument(ctx context.Context, text string, budget int64) (jsonbDocument, error) {
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return jsonbDocument{}, err
	}
	limit := min(budget, int64((1<<30)-5))
	const nodeBytes = int64(unsafe.Sizeof(jsonbNode{}))
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	if shape.nodes > limit/nodeBytes {
		return jsonbDocument{}, sqlError("54000", "builtin output byte budget exceeded")
	}
	stateBytes := shape.nodes*nodeBytes + int64(shape.depth)*frameBytes
	if stateBytes > limit || shape.decodedBytes > limit-stateBytes {
		return jsonbDocument{}, sqlError("54000", "builtin output byte budget exceeded")
	}
	stateBytes += shape.decodedBytes
	if err := outputBudget(stateBytes, budget); err != nil {
		return jsonbDocument{}, err
	}
	nodes := make([]jsonbNode, int(shape.nodes))
	frames := make([]jsonbFrame, shape.depth)
	var arena strings.Builder
	arena.Grow(int(shape.decodedBytes))
	next := 0
	_, err = walkJSONB(ctx, text, func(kind jsonTextKind, token string, key bool, depth int) error {
		if kind == 0 {
			return nil
		}
		if kind == jsonTextString {
			start := arena.Len()
			if _, err := walkJSONDecodedString(ctx, token, func(value rune) error {
				arena.WriteRune(value)
				return nil
			}); err != nil {
				return err
			}
			token = arena.String()[start:]
		}
		if key {
			frames[depth-1].key = token
			return nil
		}
		index := next
		next++
		nodes[index] = jsonbNode{kind: kind, child: -1, next: -1, text: token}
		if depth != 0 {
			parent := &frames[depth-1]
			nodes[index].key = parent.key
			parent.key = ""
			if parent.last < 0 {
				nodes[parent.parent].child = index
			} else {
				nodes[parent.last].next = index
			}
			parent.last = index
		}
		if kind == jsonTextObject || kind == jsonTextArray {
			frames[depth] = jsonbFrame{parent: index, last: -1}
		}
		return nil
	})
	if err != nil {
		return jsonbDocument{}, err
	}
	if err := sizeJSONBNodes(ctx, nodes); err != nil {
		return jsonbDocument{}, err
	}
	storageBytes := nodes[0].storageBytes
	if shape.kind != jsonTextObject && shape.kind != jsonTextArray {
		storageBytes += 8 // Root scalars use a one-element raw-scalar array.
	}
	if storageBytes > jsonbMaxStorageBytes {
		return jsonbDocument{}, sqlError("54000", "total size of jsonb elements exceeds the maximum")
	}
	return jsonbDocument{nodes: nodes, frames: frames, stateBytes: stateBytes}, nil
}

func canonicalJSONB(ctx context.Context, text string, budget int64, wire bool) (string, error) {
	document, err := parseJSONBDocument(ctx, text, budget)
	if err != nil {
		return "", err
	}
	return document.encode(ctx, budget, wire)
}

func (document jsonbDocument) render(ctx context.Context, budget int64, wire bool) (string, error) {
	nodes := document.nodes
	for index := len(nodes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if nodes[index].kind == jsonTextObject || nodes[index].kind == jsonTextArray {
			if err := sizeJSONBContainer(ctx, &nodes[index], nodes); err != nil {
				return "", err
			}
		}
	}
	return document.encode(ctx, budget, wire)
}

func (document jsonbDocument) encode(ctx context.Context, budget int64, wire bool) (string, error) {
	nodes := document.nodes
	outputBytes := nodes[0].bytes
	peakBytes := document.stateBytes + outputBytes
	if wire {
		// The versioned bytea copy overlaps the canonical text; include it
		// even though the parse state is dead before that copy starts.
		peakBytes += outputBytes + 1
	}
	if err := outputBudget(peakBytes, budget); err != nil {
		return "", err
	}
	var output strings.Builder
	output.Grow(int(outputBytes))
	if err := appendJSONBNodes(ctx, &output, nodes, document.frames); err != nil {
		return "", err
	}
	return output.String(), ctx.Err()
}

func sizeJSONBNodes(ctx context.Context, nodes []jsonbNode) error {
	// Children are allocated after parents, even when sibling links are sorted.
	for index := len(nodes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := &nodes[index]
		switch node.kind {
		case jsonTextString:
			size, err := jsonQuotedBytes(ctx, node.text)
			if err != nil {
				return err
			}
			node.bytes = size
			node.storageBytes = int64(len(node.text))
		case jsonTextNumber:
			number, err := inspectJSONBNumber(ctx, node.text)
			if err != nil {
				return err
			}
			node.bytes = number.bytes()
			node.storageBytes = number.storageBytes()
		case jsonTextBoolean, jsonTextNull:
			node.bytes = int64(len(node.text))
		case jsonTextObject, jsonTextArray:
			if node.kind == jsonTextObject {
				first, err := sortJSONBMembers(ctx, nodes, node.child)
				if err != nil {
					return err
				}
				node.child = first
			}
			if err := sizeJSONBContainer(ctx, node, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func sizeJSONBContainer(ctx context.Context, node *jsonbNode, nodes []jsonbNode) error {
	node.bytes, node.storageBytes = 2, 4
	for child := node.child; child >= 0; child = nodes[child].next {
		if err := ctx.Err(); err != nil {
			return err
		}
		if child != node.child {
			node.bytes += 2
		}
		if node.kind == jsonTextObject {
			keyBytes, err := jsonQuotedBytes(ctx, nodes[child].key)
			if err != nil {
				return err
			}
			node.bytes += keyBytes + 2
			node.storageBytes += int64(len(nodes[child].key)) + 4
		}
		node.bytes += nodes[child].bytes
		node.storageBytes += 4
	}
	// Object keys precede values; numeric and container payloads are aligned.
	for child := node.child; child >= 0; child = nodes[child].next {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := nodes[child].kind
		if kind == jsonTextNumber || kind == jsonTextObject || kind == jsonTextArray {
			node.storageBytes += (-node.storageBytes) & 3
		}
		node.storageBytes += nodes[child].storageBytes
	}
	return nil
}

func appendJSONBMemberKey(ctx context.Context, output *strings.Builder, parent jsonTextKind, key string) error {
	if parent != jsonTextObject {
		return nil
	}
	if err := appendJSONQuoted(ctx, output, key); err != nil {
		return err
	}
	output.WriteString(": ")
	return nil
}

// Bottom-up list mergesort needs no pointer tree, auxiliary slice, or recursive
// stack. Equal keys sort in reverse input order, as jsonb_util.c requires.
func sortJSONBMembers(ctx context.Context, nodes []jsonbNode, first int) (int, error) {
	if first < 0 || nodes[first].next < 0 {
		return first, ctx.Err()
	}
	for width := 1; ; width *= 2 {
		cursor, head, tail, merges := first, -1, -1, 0
		for cursor >= 0 {
			if err := ctx.Err(); err != nil {
				return -1, err
			}
			merges++
			run, err := mergeJSONBRun(ctx, nodes, cursor, width)
			if err != nil {
				return -1, err
			}
			if tail < 0 {
				head = run.head
			} else {
				nodes[tail].next = run.head
			}
			tail, cursor = run.tail, run.next
		}
		nodes[tail].next = -1
		first = head
		if merges <= 1 {
			break
		}
	}
	previous := first
	for current := nodes[first].next; current >= 0; {
		next := nodes[current].next
		order, err := compareJSONBKeys(ctx, nodes[previous].key, nodes[current].key)
		if err != nil {
			return -1, err
		}
		if order == 0 {
			nodes[previous].next = next
		} else {
			previous = current
		}
		current = next
	}
	return first, ctx.Err()
}

type jsonbMergedRun struct{ head, tail, next int }

func mergeJSONBRun(ctx context.Context, nodes []jsonbNode, cursor, width int) (jsonbMergedRun, error) {
	left, right, leftCount := cursor, cursor, 0
	for leftCount < width && right >= 0 {
		if err := ctx.Err(); err != nil {
			return jsonbMergedRun{}, err
		}
		right = nodes[right].next
		leftCount++
	}
	run := jsonbMergedRun{head: -1, tail: -1}
	rightCount := width
	for leftCount > 0 || rightCount > 0 && right >= 0 {
		if err := ctx.Err(); err != nil {
			return jsonbMergedRun{}, err
		}
		chooseLeft := rightCount == 0 || right < 0
		if leftCount > 0 && !chooseLeft {
			order, err := compareJSONBKeys(ctx, nodes[left].key, nodes[right].key)
			if err != nil {
				return jsonbMergedRun{}, err
			}
			chooseLeft = order < 0 || order == 0 && left > right
		}
		selected := right
		if leftCount > 0 && chooseLeft {
			selected = left
			left = nodes[left].next
			leftCount--
		} else {
			right = nodes[right].next
			rightCount--
		}
		if run.tail < 0 {
			run.head = selected
		} else {
			nodes[run.tail].next = selected
		}
		run.tail = selected
	}
	run.next = right
	return run, nil
}

func compareJSONBKeys(ctx context.Context, left, right string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(left) < len(right) {
		return -1, nil
	}
	if len(left) > len(right) {
		return 1, nil
	}
	for start := 0; start < len(left); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(start+cancellationChunk, len(left))
		if order := strings.Compare(left[start:end], right[start:end]); order != 0 {
			return order, nil
		}
	}
	return 0, nil
}

func appendJSONBNodes(ctx context.Context, output *strings.Builder, nodes []jsonbNode, frames []jsonbFrame) error {
	current, depth := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := &nodes[current]
		switch node.kind {
		case jsonTextObject, jsonTextArray:
			open, close := byte('['), byte(']')
			if node.kind == jsonTextObject {
				open, close = '{', '}'
			}
			output.WriteByte(open)
			if node.child >= 0 {
				frames[depth].parent = current
				depth++
				current = node.child
				if err := appendJSONBMemberKey(ctx, output, node.kind, nodes[current].key); err != nil {
					return err
				}
				continue
			}
			output.WriteByte(close)
		case jsonTextString:
			if err := appendJSONQuoted(ctx, output, node.text); err != nil {
				return err
			}
		case jsonTextNumber:
			number, err := inspectJSONBNumber(ctx, node.text)
			if err != nil {
				return err
			}
			if err := number.append(ctx, output); err != nil {
				return err
			}
		case jsonTextBoolean, jsonTextNull:
			output.WriteString(node.text)
		}
		for {
			if depth == 0 {
				return ctx.Err()
			}
			parent := frames[depth-1].parent
			if sibling := nodes[current].next; sibling >= 0 {
				output.WriteString(", ")
				current = sibling
				if err := appendJSONBMemberKey(ctx, output, nodes[parent].kind, nodes[current].key); err != nil {
					return err
				}
				break
			}
			if nodes[parent].kind == jsonTextObject {
				output.WriteByte('}')
			} else {
				output.WriteByte(']')
			}
			current = parent
			depth--
		}
	}
}
