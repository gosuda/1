package builtin

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBPathWrite(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 3305, 3579, 5054:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if args[0].IsNull || args[1].IsNull || args[3].IsNull {
			return model.NullVal(model.TypeJSONB), nil
		}
		const treatmentError = "null_value_treatment must be \"delete_key\", \"return_target\", \"use_json_null\", or \"raise_exception\""
		mode := "set"
		replacement := args[2].StrVal
		if signature.ID == 5054 {
			if args[4].IsNull {
				return model.Value{}, sqlError("22023", treatmentError)
			}
			if args[2].IsNull {
				mode = args[4].StrVal
				// PostgreSQL compares the treatment as a C string.
				for start := 0; start < len(mode); start += cancellationChunk {
					if err := ctx.Err(); err != nil {
						return model.Value{}, err
					}
					if zero := strings.IndexByte(mode[start:min(start+cancellationChunk, len(mode))], 0); zero >= 0 {
						mode = mode[:start+zero]
						break
					}
				}
				switch mode {
				case "raise_exception":
					return model.Value{}, sqlError("22004", "JSON value must not be null")
				case "use_json_null":
					mode, replacement = "set", "null"
				case "delete_key", "return_target":
					replacement = ""
				default:
					return model.Value{}, sqlError("22023", treatmentError)
				}
			}
		} else if args[2].IsNull {
			return model.NullVal(model.TypeJSONB), nil
		}
		if mode != "return_target" {
			if len(args[1].Dimensions) > 1 {
				return model.Value{}, sqlError("2202E", "wrong number of array subscripts")
			}
			if err := model.ValidateArrayShape(args[1].Dimensions, uint64(len(args[1].ArrayVal))); err != nil {
				return model.Value{}, sqlError("2202E", "invalid array dimensions")
			}
		}
		budget := min(callBudget.OutputBytes, int64(min(callBudget.MemoryBytes, uint64(^uint64(0)>>1))))
		inputBytes, err := jsonbPathWriteInputBytes(ctx, args, budget)
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
		texts := [2]string{args[0].StrVal, replacement}
		textCount := 2
		if mode != "set" {
			textCount = 1
		}
		document, newRoot, err := parseJSONBPathWrite(ctx, texts[:textCount], inputBytes, budget, claim)
		if err != nil {
			return model.Value{}, err
		}
		root := &document.nodes[0]
		if mode != "return_target" {
			if root.kind != jsonTextObject && root.kind != jsonTextArray {
				if mode == "delete_key" {
					return model.Value{}, sqlError("22023", "cannot delete path in scalar")
				}
				return model.Value{}, sqlError("22023", "cannot set path in scalar")
			}
			if mode == "delete_key" {
				if root.child >= 0 {
					err = document.deleteJSONBPath(ctx, args[1].ArrayVal, budget)
				}
			} else if root.child >= 0 || signature.ID == 3579 || args[3].IntVal != 0 {
				err = document.writeJSONBPath(ctx, args[1].ArrayVal, newRoot, signature.ID == 3579, args[3].IntVal != 0, budget)
			}
			if err != nil {
				return model.Value{}, err
			}
		}
		// Parents precede their children, including the grafted replacement.
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
}

func jsonbPathWriteInputBytes(ctx context.Context, args []model.Value, budget int64) (int64, error) {
	bytes := int64(len(args)) * int64(unsafe.Sizeof(model.Value{}))
	for i := range args {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !args[i].IsNull {
			bytes += int64(len(args[i].StrVal))
		}
	}
	array := &args[1]
	bytes += int64(len(array.ArrayVal))*int64(unsafe.Sizeof(model.Value{})) + int64(len(array.Dimensions))*int64(unsafe.Sizeof(model.ArrayDimension{}))
	if err := outputBudget(bytes, budget); err != nil {
		return 0, err
	}
	for i := range array.ArrayVal {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !array.ArrayVal[i].IsNull {
			bytes += int64(len(array.ArrayVal[i].StrVal))
			if err := outputBudget(bytes, budget); err != nil {
				return 0, err
			}
		}
	}
	return bytes, nil
}

func (document jsonbDocument) writeJSONBPath(ctx context.Context, path []model.Value, newRoot int, insert, flag bool, budget int64) error {
	parent := 0
	for level := range path {
		if err := ctx.Err(); err != nil {
			return err
		}
		step := &path[level]
		if step.IsNull {
			return jsonbDeletePathNullError(ctx, level, document.stateBytes, budget)
		}
		if step.Type != model.TypeString {
			return sqlError("42804", "JSON path requires text elements")
		}
		terminal := level == len(path)-1
		node := &document.nodes[parent]
		link := &node.child
		found := false
		switch node.kind {
		case jsonTextObject:
			for *link >= 0 {
				order, err := compareJSONBKeys(ctx, document.nodes[*link].key, step.StrVal)
				if err != nil {
					return err
				}
				if order >= 0 {
					found = order == 0
					break
				}
				link = &document.nodes[*link].next
			}
			if terminal && found && insert {
				return sqlError("22023", "cannot replace existing key")
			}
		case jsonTextArray:
			index, valid, err := parseJSONPathIndex(ctx, step.StrVal)
			if err != nil {
				return err
			}
			if !valid {
				return jsonbDeletePathIndexError(ctx, level, step.StrVal, document.stateBytes, budget)
			}
			var count int64
			for child := node.child; child >= 0; child = document.nodes[child].next {
				if err := ctx.Err(); err != nil {
					return err
				}
				count++
			}
			if index < 0 {
				index += count
			}
			found = index >= 0 && index < count
			index = max(0, min(index, count))
			for index > 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				link = &document.nodes[*link].next
				index--
			}
			if terminal && found && insert && flag {
				link = &document.nodes[*link].next
			}
		default:
			return nil
		}
		if !terminal {
			if !found {
				return nil
			}
			parent = *link
			continue
		}
		if !found && !insert && !flag {
			return nil
		}
		replacement := &document.nodes[newRoot]
		if node.kind == jsonTextObject {
			replacement.key = step.StrVal
		}
		replacement.next = *link
		if found && !insert {
			replacement.next = document.nodes[*link].next
		}
		*link = newRoot
		return nil
	}
	return nil
}
