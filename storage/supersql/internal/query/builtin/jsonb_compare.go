package builtin

import (
	"cmp"
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// claimJSONBDocument admits the parser's exact node, frame and decoded arena payload.
// The caller releases the claim after dropping the document and all borrowed nodes.
func claimJSONBDocument(ctx context.Context, text string, budget CallBudget) (jsonbDocument, columnar.Allocation, CallBudget, error) {
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return jsonbDocument{}, nil, budget, err
	}
	size := uint64(shape.nodes)*uint64(unsafe.Sizeof(jsonbNode{})) + uint64(shape.depth)*uint64(unsafe.Sizeof(jsonbFrame{})) + uint64(shape.decodedBytes)
	claim, err := numericTemporary(budget, 0, size)
	if err != nil {
		return jsonbDocument{}, nil, budget, err
	}
	document, err := parseJSONBDocument(ctx, text, int64(min(size, uint64(math.MaxInt64))))
	if err != nil {
		if claim != nil {
			claim.Release()
		}
		return jsonbDocument{}, nil, budget, err
	}
	budget.MemoryBytes -= size
	return document, claim, budget, nil
}

func compileJSONBComparison(signature Signature) scalarKernel {
	switch signature.ID {
	case 4038, 4039, 4040, 4041, 4042, 4043, 4044, 4046, 4050:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		left, lc, budget, err := claimJSONBDocument(ctx, args[0].StrVal, budget)
		if err != nil {
			return model.Value{}, err
		}
		if lc != nil {
			defer lc.Release()
		}
		right, rc, budget, err := claimJSONBDocument(ctx, args[1].StrVal, budget)
		if err != nil {
			return model.Value{}, err
		}
		if rc != nil {
			defer rc.Release()
		}
		if signature.ID == 4046 || signature.ID == 4050 {
			if signature.ID == 4050 {
				left, right = right, left
			}
			yes, err := jsonbDocumentContains(ctx, left, right, budget)
			return model.BoolVal(yes), err
		}
		order, err := compareJSONBDocuments(ctx, left, right, budget)
		if err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 4038:
			return model.BoolVal(order != 0), nil
		case 4039:
			return model.BoolVal(order < 0), nil
		case 4040:
			return model.BoolVal(order > 0), nil
		case 4041:
			return model.BoolVal(order <= 0), nil
		case 4042:
			return model.BoolVal(order >= 0), nil
		case 4043:
			return model.BoolVal(order == 0), nil
		default:
			return model.Int64Val(int64(order)), nil
		}
	}
}

func jsonbKindOrder(kind jsonTextKind) int {
	switch kind {
	case jsonTextNull:
		return 0
	case jsonTextString:
		return 1
	case jsonTextNumber:
		return 2
	case jsonTextBoolean:
		return 3
	case jsonTextArray:
		return 4
	case jsonTextObject:
		return 5
	}
	return -1
}

func jsonbNumberDigit(number jsonbNumber, index int) byte {
	if index < 0 {
		return 0
	}
	if index < len(number.integer) {
		return number.integer[index] - '0'
	}
	index -= len(number.integer)
	if index < len(number.fraction) {
		return number.fraction[index] - '0'
	}
	return 0
}

func compareJSONBNumbers(ctx context.Context, left, right string) (int, error) {
	a, err := inspectJSONBNumber(ctx, left)
	if err != nil {
		return 0, err
	}
	b, err := inspectJSONBNumber(ctx, right)
	if err != nil {
		return 0, err
	}
	if a.zero && b.zero {
		return 0, nil
	}
	if a.negative != b.negative {
		if a.negative {
			return -1, nil
		}
		return 1, nil
	}
	sign := 1
	if a.negative {
		sign = -1
	}
	if a.zero {
		return -sign, nil
	}
	if b.zero {
		return sign, nil
	}
	if order := cmp.Compare(a.point-a.first, b.point-b.first); order != 0 {
		return sign * order, nil
	}
	for i := 0; i <= max(a.last-a.first, b.last-b.first); i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if order := cmp.Compare(jsonbNumberDigit(a, a.first+i), jsonbNumberDigit(b, b.first+i)); order != 0 {
			return sign * order, nil
		}
	}
	return 0, ctx.Err()
}

func compareJSONBScalarNodes(ctx context.Context, a, b jsonbNode) (int, error) {
	if order := cmp.Compare(jsonbKindOrder(a.kind), jsonbKindOrder(b.kind)); order != 0 {
		return order, nil
	}
	switch a.kind {
	case jsonTextNumber:
		return compareJSONBNumbers(ctx, a.text, b.text)
	case jsonTextString:
		return compareJSONBStrings(ctx, a.text, b.text)
	case jsonTextBoolean:
		if a.text == b.text {
			return 0, ctx.Err()
		}
		if a.text == "true" {
			return 1, ctx.Err()
		}
		return -1, ctx.Err()
	}
	return 0, ctx.Err()
}

func compareJSONBStrings(ctx context.Context, a, b string) (int, error) {
	for i := 0; i < min(len(a), len(b)); i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if a[i] != b[i] {
			return int(a[i]) - int(b[i]), nil
		}
	}
	return cmp.Compare(len(a), len(b)), ctx.Err()
}

func jsonbChildCount(ctx context.Context, nodes []jsonbNode, index int) (int, error) {
	count := 0
	for child := nodes[index].child; child >= 0; child = nodes[child].next {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

type jsonbCompareFrame struct {
	left, right int
	object      bool
}

// compareJSONBDocuments borrows canonical documents, including their last-key-wins links.
// Root pseudo-arrays retain PostgreSQL's historical empty-array-before-scalar anomaly.
func compareJSONBDocuments(ctx context.Context, a, b jsonbDocument, budget CallBudget) (int, error) {
	ak, bk := a.nodes[0].kind, b.nodes[0].kind
	if ak != jsonTextObject && bk != jsonTextObject && (ak == jsonTextArray) != (bk == jsonTextArray) {
		ac, bc := 1, 1
		var err error
		if ak == jsonTextArray {
			ac, err = jsonbChildCount(ctx, a.nodes, 0)
		} else {
			bc, err = jsonbChildCount(ctx, b.nodes, 0)
		}
		if err != nil {
			return 0, err
		}
		if ac != bc {
			return cmp.Compare(ac, bc), nil
		}
		if ak == jsonTextArray {
			return 1, nil
		}
		return -1, nil
	}
	size := uint64(min(len(a.frames), len(b.frames))+1) * uint64(unsafe.Sizeof(jsonbCompareFrame{}))
	claim, err := numericTemporary(budget, 0, size)
	if err != nil {
		return 0, err
	}
	if claim != nil {
		defer claim.Release()
	}
	stack := make([]jsonbCompareFrame, 1, min(len(a.frames), len(b.frames))+1)
	stack[0] = jsonbCompareFrame{left: 0, right: 0}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		frame := &stack[len(stack)-1]
		if frame.left < 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		ai, bi := frame.left, frame.right
		an, bn := a.nodes[ai], b.nodes[bi]
		frame.left, frame.right = an.next, bn.next
		if frame.object {
			if order, err := compareJSONBStrings(ctx, an.key, bn.key); err != nil || order != 0 {
				return order, err
			}
		}
		if order, err := compareJSONBScalarNodes(ctx, an, bn); err != nil || order != 0 {
			return order, err
		}
		if an.kind != jsonTextArray && an.kind != jsonTextObject {
			continue
		}
		ac, err := jsonbChildCount(ctx, a.nodes, ai)
		if err != nil {
			return 0, err
		}
		bc, err := jsonbChildCount(ctx, b.nodes, bi)
		if err != nil {
			return 0, err
		}
		if ac != bc {
			return cmp.Compare(ac, bc), nil
		}
		stack = append(stack, jsonbCompareFrame{left: an.child, right: bn.child, object: an.kind == jsonTextObject})
	}
	return 0, ctx.Err()
}
