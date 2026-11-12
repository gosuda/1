package builtin

import (
	"context"
	"math"
	"strconv"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errJSONBSubscriptScalar = sqlError("22023", "cannot replace existing key: the path assumes a composite object, but it is a scalar value")

// JSONBSubscriptStore implements jsonb assignment subscripting. Unlike
// jsonb_set, it fills missing containers and array positions along the path.
func JSONBSubscriptStore(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	if len(args) < 3 {
		return model.Value{}, sqlError("22023", "jsonb subscript assignment requires a path")
	}
	path := args[1 : len(args)-1]
	for _, step := range path {
		if step.IsNull {
			return model.Value{}, sqlError("22004", "jsonb subscript in assignment must not be null")
		}
		if step.Type != model.TypeInt64 && step.Type != model.TypeString {
			return model.Value{}, sqlError("42804", "jsonb subscript must have integer or text type")
		}
	}

	source := args[0].StrVal
	if args[0].IsNull {
		if path[0].Type == model.TypeInt64 {
			source = "[]"
		} else {
			source = "{}"
		}
	}
	replacement := args[len(args)-1].StrVal
	if args[len(args)-1].IsNull {
		replacement = "null"
	}
	budget := min(callBudget.OutputBytes, int64(min(callBudget.MemoryBytes, uint64(math.MaxInt64))))
	inputBytes, err := jsonbSubscriptInputBytes(ctx, args, source, replacement, budget)
	if err != nil {
		return model.Value{}, err
	}
	var claim columnar.Allocation
	if callBudget.Allocator != nil {
		claim, err = callBudget.Allocator.Reserve(uint64(inputBytes))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	document, newRoot, err := parseJSONBPathWrite(ctx, []string{source, replacement}, inputBytes, budget, claim)
	if err != nil {
		return model.Value{}, err
	}
	extraNodes, err := document.jsonbSubscriptExtraNodes(ctx, path)
	if err != nil {
		return model.Value{}, err
	}
	if err = document.growJSONBSubscriptNodes(extraNodes, budget, claim); err != nil {
		return model.Value{}, err
	}
	if err = document.writeJSONBSubscript(ctx, path, newRoot); err != nil {
		return model.Value{}, err
	}
	// The tree is final here. Its depth can exceed the depth of the parsed inputs,
	// so the traversal stack used by encode is resized before sizing and encoding.
	if err = document.ensureJSONBTraversalFrames(budget, claim); err != nil {
		return model.Value{}, err
	}
	for index := len(document.nodes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		node := &document.nodes[index]
		if node.kind == jsonTextObject || node.kind == jsonTextArray {
			if err := sizeJSONBContainer(ctx, node, document.nodes); err != nil {
				return model.Value{}, err
			}
		}
	}
	if document.nodes[0].storageBytes > jsonbMaxStorageBytes {
		return model.Value{}, sqlError("54000", "total size of jsonb elements exceeds the maximum")
	}
	peak := document.stateBytes + document.nodes[0].bytes
	if err := outputBudget(peak, budget); err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		if err := claim.Resize(uint64(peak)); err != nil {
			return model.Value{}, err
		}
	}
	output, err := document.encode(ctx, budget, false)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeJSONB, StrVal: output}, nil
}

func jsonbSubscriptInputBytes(ctx context.Context, args []model.Value, source, replacement string, budget int64) (int64, error) {
	bytes := int64(len(args))*int64(unsafe.Sizeof(model.Value{})) + int64(len(source)+len(replacement))
	for _, step := range args[1 : len(args)-1] {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if step.Type == model.TypeString {
			bytes += int64(len(step.StrVal))
		}
	}
	if err := outputBudget(bytes, budget); err != nil {
		return 0, err
	}
	return bytes, nil
}

func (document *jsonbDocument) jsonbSubscriptExtraNodes(ctx context.Context, path []model.Value) (int64, error) {
	parent := 0
	for level, step := range path {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		terminal := level == len(path)-1
		node := &document.nodes[parent]
		switch node.kind {
		case jsonTextObject:
			key := jsonbSubscriptKey(step)
			child := node.child
			for child >= 0 && document.nodes[child].key != key {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				child = document.nodes[child].next
			}
			if child < 0 {
				if terminal {
					return 0, nil
				}
				return jsonbMissingSubscriptNodes(ctx, path, level+1)
			}
			if terminal {
				return 0, nil
			}
			if !jsonbSubscriptContainer(document.nodes[child].kind) {
				return 0, errJSONBSubscriptScalar
			}
			parent = child
		case jsonTextArray:
			index, count, link, err := document.jsonbSubscriptArrayPosition(ctx, parent, step)
			if err != nil {
				return 0, err
			}
			if index >= count {
				extra := index - count
				if terminal {
					return extra, nil
				}
				missing, err := jsonbMissingSubscriptNodes(ctx, path, level+1)
				if err != nil || extra > math.MaxInt64-missing {
					if err != nil {
						return 0, err
					}
					return 0, sqlError("54000", "jsonb subscript result exceeds the output byte budget")
				}
				return extra + missing, nil
			}
			if terminal {
				return 0, nil
			}
			parent = *link
			if !jsonbSubscriptContainer(document.nodes[parent].kind) {
				return 0, errJSONBSubscriptScalar
			}
		default:
			return 0, errJSONBSubscriptScalar
		}
	}
	return 0, nil
}

func jsonbMissingSubscriptNodes(ctx context.Context, path []model.Value, level int) (int64, error) {
	nodes := int64(0)
	for ; level < len(path); level++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if nodes == math.MaxInt64 {
			return 0, sqlError("54000", "jsonb subscript result exceeds the output byte budget")
		}
		nodes++
		index, valid, err := jsonbSubscriptIndex(ctx, path[level])
		if err != nil {
			return 0, err
		}
		if valid && index > 0 {
			if nodes > math.MaxInt64-index {
				return 0, sqlError("54000", "jsonb subscript result exceeds the output byte budget")
			}
			nodes += index
		}
	}
	return nodes, nil
}

func (document *jsonbDocument) growJSONBSubscriptNodes(extra, budget int64, claim columnar.Allocation) error {
	if extra == 0 {
		return nil
	}
	const nodeBytes = int64(unsafe.Sizeof(jsonbNode{}))
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	if extra < 0 || extra > int64(math.MaxInt)-int64(len(document.nodes)) || extra > (budget-document.stateBytes)/nodeBytes {
		return sqlError("54000", "jsonb subscript result exceeds the output byte budget")
	}
	document.stateBytes += extra * nodeBytes
	if claim != nil {
		if err := claim.Resize(uint64(document.stateBytes)); err != nil {
			return err
		}
	}
	nodes := make([]jsonbNode, len(document.nodes), len(document.nodes)+int(extra))
	copy(nodes, document.nodes)
	document.nodes = nodes
	return nil
}

// ensureJSONBTraversalFrames sizes the traversal stack to the depth of the final
// tree. Assigning a subscript can deepen the tree without changing its node count
// (a replacement subtree is grafted into freshly built intermediate containers), so
// the depth observed while parsing the inputs is not a bound on the depth that
// appendJSONBNodes will descend to. The required depth is at most the node count.
func (document *jsonbDocument) ensureJSONBTraversalFrames(budget int64, claim columnar.Allocation) error {
	required := document.jsonbNodeDepth()
	if required <= len(document.frames) {
		return nil
	}
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	extra := int64(required - len(document.frames))
	if extra > (budget-document.stateBytes)/frameBytes {
		return sqlError("54000", "jsonb subscript result exceeds the output byte budget")
	}
	document.stateBytes += extra * frameBytes
	if claim != nil {
		if err := claim.Resize(uint64(document.stateBytes)); err != nil {
			return err
		}
	}
	frames := make([]jsonbFrame, required)
	copy(frames, document.frames)
	document.frames = frames
	return nil
}

// jsonbNodeDepth returns the nesting depth of the container tree, counting the
// root container itself. A scalar root has depth zero. The depth is bounded by
// jsonTextMaxDepth because walkJSONB rejects deeper input, and subscript
// assignment only ever builds one container per path level.
func (document *jsonbDocument) jsonbNodeDepth() int {
	if len(document.nodes) == 0 || (document.nodes[0].kind != jsonTextObject && document.nodes[0].kind != jsonTextArray) {
		return 0
	}
	type frame struct{ index, depth int }
	stack := make([]frame, 0, 8)
	stack = append(stack, frame{index: 0, depth: 1})
	maximum := 0
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.depth > maximum {
			maximum = current.depth
		}
		for child := document.nodes[current.index].child; child >= 0; child = document.nodes[child].next {
			if document.nodes[child].kind == jsonTextObject || document.nodes[child].kind == jsonTextArray {
				if current.depth+1 > jsonTextMaxDepth {
					return jsonTextMaxDepth
				}
				stack = append(stack, frame{index: child, depth: current.depth + 1})
			}
		}
	}
	return maximum
}

func (document *jsonbDocument) writeJSONBSubscript(ctx context.Context, path []model.Value, newRoot int) error {
	parent := 0
	for level, step := range path {
		if err := ctx.Err(); err != nil {
			return err
		}
		terminal := level == len(path)-1
		node := &document.nodes[parent]
		switch node.kind {
		case jsonTextObject:
			key := jsonbSubscriptKey(step)
			link := &node.child
			found := false
			for *link >= 0 {
				order, err := compareJSONBKeys(ctx, document.nodes[*link].key, key)
				if err != nil {
					return err
				}
				if order >= 0 {
					found = order == 0
					break
				}
				link = &document.nodes[*link].next
			}
			if found && !terminal {
				parent = *link
				if !jsonbSubscriptContainer(document.nodes[parent].kind) {
					return errJSONBSubscriptScalar
				}
				continue
			}
			replacement := newRoot
			if !terminal {
				var err error
				replacement, err = document.buildJSONBSubscriptPath(ctx, path, level+1, newRoot)
				if err != nil {
					return err
				}
			}
			next := *link
			if found {
				next = document.nodes[*link].next
			}
			document.nodes[replacement].key = key
			document.nodes[replacement].next = next
			*link = replacement
			return nil
		case jsonTextArray:
			index, count, link, err := document.jsonbSubscriptArrayPosition(ctx, parent, step)
			if err != nil {
				return err
			}
			if index < count && !terminal {
				parent = *link
				if !jsonbSubscriptContainer(document.nodes[parent].kind) {
					return errJSONBSubscriptScalar
				}
				continue
			}
			for count < index {
				if err := ctx.Err(); err != nil {
					return err
				}
				null, err := document.appendJSONBSubscriptNode(jsonbNode{kind: jsonTextNull, child: -1, next: -1, text: "null"})
				if err != nil {
					return err
				}
				*link = null
				link = &document.nodes[null].next
				count++
			}
			replacement := newRoot
			if !terminal {
				replacement, err = document.buildJSONBSubscriptPath(ctx, path, level+1, newRoot)
				if err != nil {
					return err
				}
			}
			if index < count {
				document.nodes[replacement].next = document.nodes[*link].next
			} else {
				document.nodes[replacement].next = -1
			}
			*link = replacement
			return nil
		default:
			return errJSONBSubscriptScalar
		}
	}
	return nil
}

func (document *jsonbDocument) buildJSONBSubscriptPath(ctx context.Context, path []model.Value, level, newRoot int) (int, error) {
	if level >= len(path) {
		return newRoot, nil
	}
	root := -1
	var link *int
	var key string
	// Parents must precede children because container sizes are computed in reverse.
	for current := level; current < len(path); current++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		kind := jsonTextObject
		index, valid, err := jsonbSubscriptIndex(ctx, path[current])
		if err != nil {
			return 0, err
		}
		if valid {
			kind = jsonTextArray
		}
		container, err := document.appendJSONBSubscriptNode(jsonbNode{kind: kind, child: -1, next: -1, key: key})
		if err != nil {
			return 0, err
		}
		if root < 0 {
			root = container
		} else {
			*link = container
		}
		link = &document.nodes[container].child
		key = ""
		if kind == jsonTextObject {
			key = jsonbSubscriptKey(path[current])
			continue
		}
		for position := int64(0); position < index; position++ {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			null, err := document.appendJSONBSubscriptNode(jsonbNode{kind: jsonTextNull, child: -1, next: -1, text: "null"})
			if err != nil {
				return 0, err
			}
			*link = null
			link = &document.nodes[null].next
		}
	}
	document.nodes[newRoot].key = key
	*link = newRoot
	return root, nil
}

func (document *jsonbDocument) jsonbSubscriptArrayPosition(ctx context.Context, parent int, step model.Value) (int64, int64, *int, error) {
	index, valid, err := jsonbSubscriptIndex(ctx, step)
	if err != nil {
		return 0, 0, nil, err
	}
	if !valid {
		return 0, 0, nil, sqlError("22P02", "jsonb array subscript is not an integer")
	}
	count := int64(0)
	for child := document.nodes[parent].child; child >= 0; child = document.nodes[child].next {
		if err := ctx.Err(); err != nil {
			return 0, 0, nil, err
		}
		count++
	}
	if index < 0 {
		if index < -count {
			return 0, 0, nil, sqlError("22023", "jsonb array subscript is out of range")
		}
		index += count
	}
	link := &document.nodes[parent].child
	for position := int64(0); position < index && position < count; position++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, nil, err
		}
		link = &document.nodes[*link].next
	}
	return index, count, link, nil
}

func (document *jsonbDocument) appendJSONBSubscriptNode(node jsonbNode) (int, error) {
	if len(document.nodes) == cap(document.nodes) {
		return 0, sqlError("54000", "jsonb subscript result exceeds the output byte budget")
	}
	index := len(document.nodes)
	document.nodes = append(document.nodes, node)
	return index, nil
}

func jsonbSubscriptContainer(kind jsonTextKind) bool {
	return kind == jsonTextObject || kind == jsonTextArray
}

func jsonbSubscriptKey(step model.Value) string {
	if step.Type == model.TypeInt64 {
		return strconv.FormatInt(step.IntVal, 10)
	}
	return step.StrVal
}

func jsonbSubscriptIndex(ctx context.Context, step model.Value) (int64, bool, error) {
	if step.Type == model.TypeInt64 {
		if step.IntVal < math.MinInt32 || step.IntVal > math.MaxInt32 {
			return 0, false, nil
		}
		return step.IntVal, true, ctx.Err()
	}
	return parseJSONPathIndex(ctx, step.StrVal)
}
