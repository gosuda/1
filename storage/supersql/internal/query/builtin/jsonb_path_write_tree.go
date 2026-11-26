package builtin

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
)

// Input trees share one exact node allocation. The second root follows every
// target node, preserving bottom-up sizing when its subtree is grafted in place.
func parseJSONBPathWrite(ctx context.Context, texts []string, inputBytes, budget int64, claim columnar.Allocation) (jsonbDocument, int, error) {
	var shapes [2]jsonbShape
	count := len(texts)
	var nodeCount, decodedBytes int64
	frameCount := 0
	for i := range count {
		shape, err := walkJSONB(ctx, texts[i], nil)
		if err != nil {
			return jsonbDocument{}, 0, err
		}
		shapes[i] = shape
		nodeCount += shape.nodes
		decodedBytes += shape.decodedBytes
		frameCount += shape.depth
	}
	const nodeBytes = int64(unsafe.Sizeof(jsonbNode{}))
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	limit := min(budget, int64((1<<30)-5))
	if nodeCount > limit/nodeBytes {
		return jsonbDocument{}, 0, sqlError("54000", "builtin output byte budget exceeded")
	}
	stateBytes := inputBytes + nodeCount*nodeBytes + int64(frameCount)*frameBytes + decodedBytes
	if err := outputBudget(stateBytes, limit); err != nil {
		return jsonbDocument{}, 0, err
	}
	if claim != nil {
		if err := claim.Resize(uint64(stateBytes)); err != nil {
			return jsonbDocument{}, 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return jsonbDocument{}, 0, err
	}
	document := jsonbDocument{
		nodes:      make([]jsonbNode, int(nodeCount)),
		frames:     make([]jsonbFrame, frameCount),
		stateBytes: stateBytes,
	}
	var arena strings.Builder
	arena.Grow(int(decodedBytes))
	next := 0
	for i := range count {
		_, err := walkJSONB(ctx, texts[i], func(kind jsonTextKind, token string, key bool, depth int) error {
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
				document.frames[depth-1].key = token
				return nil
			}
			index := next
			next++
			document.nodes[index] = jsonbNode{kind: kind, child: -1, next: -1, text: token}
			if depth != 0 {
				parent := &document.frames[depth-1]
				document.nodes[index].key = parent.key
				parent.key = ""
				if parent.last < 0 {
					document.nodes[parent.parent].child = index
				} else {
					document.nodes[parent.last].next = index
				}
				parent.last = index
			}
			if kind == jsonTextObject || kind == jsonTextArray {
				document.frames[depth] = jsonbFrame{parent: index, last: -1}
			}
			return nil
		})
		if err != nil {
			return jsonbDocument{}, 0, err
		}
	}
	if err := sizeJSONBNodes(ctx, document.nodes); err != nil {
		return jsonbDocument{}, 0, err
	}
	root := 0
	for i := range count {
		storageBytes := document.nodes[root].storageBytes
		if shapes[i].kind != jsonTextObject && shapes[i].kind != jsonTextArray {
			storageBytes += 8
		}
		if storageBytes > jsonbMaxStorageBytes {
			return jsonbDocument{}, 0, sqlError("54000", "total size of jsonb elements exceeds the maximum")
		}
		root += int(shapes[i].nodes)
	}
	return document, int(shapes[0].nodes), nil
}
