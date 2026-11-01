package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBConcat(signature Signature) scalarKernel {
	if signature.ID == 3301 {
		return jsonbConcat
	}
	return nil
}

func jsonbConcat(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	left, err := parseJSONBDocument(ctx, args[0].StrVal, budget)
	if err != nil {
		return model.Value{}, err
	}
	right, err := parseJSONBDocument(ctx, args[1].StrVal, budget-left.stateBytes)
	if err != nil {
		return model.Value{}, err
	}

	// Keep both parse states charged while rebased nodes retain their strings.
	// A new leading root preserves the parent's-before-children size invariant.
	const nodeBytes = int64(unsafe.Sizeof(jsonbNode{}))
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	nodeCount := int64(len(left.nodes)) + int64(len(right.nodes)) + 1
	frameCount := max(len(left.frames), len(right.frames)) + 1
	stateBytes := left.stateBytes + right.stateBytes + nodeCount*nodeBytes + int64(frameCount)*frameBytes
	if err := outputBudget(stateBytes, budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	document := jsonbDocument{
		nodes:      make([]jsonbNode, int(nodeCount)),
		frames:     make([]jsonbFrame, frameCount),
		stateBytes: stateBytes,
	}
	leftRoot, rightRoot := 1, len(left.nodes)+1
	if err := copyJSONBConcatNodes(ctx, document.nodes, left.nodes, leftRoot); err != nil {
		return model.Value{}, err
	}
	if err := copyJSONBConcatNodes(ctx, document.nodes, right.nodes, rightRoot); err != nil {
		return model.Value{}, err
	}
	nodes := document.nodes
	nodes[0] = jsonbNode{kind: jsonTextArray, child: -1, next: -1}
	if nodes[leftRoot].kind == jsonTextObject && nodes[rightRoot].kind == jsonTextObject {
		nodes[0].kind = jsonTextObject
		first, err := mergeJSONBConcatObjects(ctx, nodes, nodes[leftRoot].child, nodes[rightRoot].child)
		if err != nil {
			return model.Value{}, err
		}
		nodes[0].child = first
	} else {
		// PostgreSQL's iterator exposes raw scalars as one-element arrays.
		// Objects are also single elements unless both roots are objects.
		link := &nodes[0].child
		for _, root := range [2]int{leftRoot, rightRoot} {
			first := root
			if nodes[root].kind == jsonTextArray {
				first = nodes[root].child
			}
			*link = first
			for *link >= 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				link = &nodes[*link].next
			}
		}
	}
	if err := sizeJSONBContainer(ctx, &nodes[0], nodes); err != nil {
		return model.Value{}, err
	}
	if nodes[0].storageBytes > jsonbMaxStorageBytes {
		return model.Value{}, sqlError("54000", "total size of jsonb elements exceeds the maximum")
	}
	output, err := document.encode(ctx, budget, false)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeJSONB, StrVal: output}, nil
}

func copyJSONBConcatNodes(ctx context.Context, target, source []jsonbNode, offset int) error {
	for index, node := range source {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node.child >= 0 {
			node.child += offset
		}
		if node.next >= 0 {
			node.next += offset
		}
		target[index+offset] = node
	}
	return nil
}

func mergeJSONBConcatObjects(ctx context.Context, nodes []jsonbNode, left, right int) (int, error) {
	first := -1
	link := &first
	for left >= 0 || right >= 0 {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		order := 1
		if right < 0 {
			order = -1
		} else if left >= 0 {
			var err error
			order, err = compareJSONBKeys(ctx, nodes[left].key, nodes[right].key)
			if err != nil {
				return -1, err
			}
		}
		selected := right
		if order < 0 {
			selected = left
			left = nodes[left].next
		} else {
			if order == 0 {
				left = nodes[left].next
			}
			right = nodes[right].next
		}
		*link = selected
		link = &nodes[selected].next
	}
	*link = -1
	return first, nil
}
