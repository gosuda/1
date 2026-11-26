package builtin

import (
	"context"
	"math/bits"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBHash(signature Signature) scalarKernel {
	if signature.ID != 4045 && signature.ID != 3416 {
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		document, claim, budget, err := claimJSONBDocument(ctx, args[0].StrVal, budget)
		if err != nil {
			return model.Value{}, err
		}
		if claim != nil {
			defer claim.Release()
		}
		extended := signature.ID == 3416
		seed := uint64(0)
		if extended {
			seed = uint64(args[1].IntVal)
		}
		hash, err := hashJSONBDocument(ctx, document, seed, extended, budget)
		if extended {
			return model.Int64Val(int64(hash)), err
		}
		return model.Int64Val(int64(int32(hash))), err
	}
}

func hashJSONBDocument(ctx context.Context, document jsonbDocument, seed uint64, extended bool, budget CallBudget) (uint64, error) {
	root := document.nodes[0]
	if (root.kind == jsonTextArray || root.kind == jsonTextObject) && root.child < 0 {
		return seed, ctx.Err()
	}
	depth := len(document.frames) + 1
	size := uint64(depth) * uint64(unsafe.Sizeof(jsonbCompareFrame{}))
	claim, err := numericTemporary(budget, 0, size)
	if err != nil {
		return 0, err
	}
	if claim != nil {
		defer claim.Release()
	}
	budget.MemoryBytes -= size
	stack := make([]jsonbCompareFrame, 1, depth)
	hash := uint64(0)
	mixContainer := func(kind jsonTextKind) {
		marker := uint64(0x40000000)
		if kind == jsonTextObject {
			marker = 0x20000000
		}
		if extended {
			marker |= marker << 32
		}
		hash ^= marker
	}
	mixScalar := func(kind jsonTextKind, text string) error {
		var value uint64
		switch kind {
		case jsonTextNull:
			value = seed + 1
		case jsonTextBoolean:
			if text == "true" {
				value = 2
			} else {
				value = 4
			}
			if extended && seed != 0 {
				boolean := uint32(0)
				if text == "true" {
					boolean = 1
				}
				value = pgHashUint32Extended(boolean, seed)
			}
		case jsonTextString:
			var err error
			value, err = pgHashBytesExtended(ctx, unsafe.Slice(unsafe.StringData(text), len(text)), seed)
			if err != nil {
				return err
			}
		case jsonTextNumber:
			var err error
			value, err = hashJSONBNumber(ctx, text, seed, budget)
			if err != nil {
				return err
			}
		}
		if extended {
			hash = uint64(bits.RotateLeft32(uint32(hash>>32), 1))<<32 | uint64(bits.RotateLeft32(uint32(hash), 1))
			hash ^= value
		} else {
			hash = uint64(bits.RotateLeft32(uint32(hash), 1) ^ uint32(value))
		}
		return nil
	}
	if root.kind != jsonTextArray && root.kind != jsonTextObject {
		mixContainer(jsonTextArray)
	}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		frame := &stack[len(stack)-1]
		if frame.left < 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		node := document.nodes[frame.left]
		frame.left = node.next
		if frame.object {
			if err := mixScalar(jsonTextString, node.key); err != nil {
				return 0, err
			}
		}
		if node.kind == jsonTextArray || node.kind == jsonTextObject {
			mixContainer(node.kind)
			stack = append(stack, jsonbCompareFrame{left: node.child, object: node.kind == jsonTextObject})
		} else {
			if err := mixScalar(node.kind, node.text); err != nil {
				return 0, err
			}
		}
	}
	return hash, ctx.Err()
}
