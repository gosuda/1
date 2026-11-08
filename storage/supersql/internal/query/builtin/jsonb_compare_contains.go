package builtin

import (
	"context"
	"unsafe"
)

type jsonbContainFrame struct {
	left, right              int
	member, candidate        int
	entered, waiting, object bool
}

func jsonbDocumentContains(ctx context.Context, a, b jsonbDocument, budget CallBudget) (bool, error) {
	depth := min(len(a.frames), len(b.frames)) + 2
	size := uint64(depth) * uint64(unsafe.Sizeof(jsonbContainFrame{}))
	claim, err := numericTemporary(budget, 0, size)
	if err != nil {
		return false, err
	}
	if claim != nil {
		defer claim.Release()
	}
	stack := make([]jsonbContainFrame, 1, depth)
	result := false
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		f := &stack[len(stack)-1]
		an, bn := a.nodes[f.left], b.nodes[f.right]
		if !f.entered {
			f.entered = true
			if an.kind != jsonTextArray && an.kind != jsonTextObject {
				order, err := compareJSONBScalarNodes(ctx, an, bn)
				if err != nil {
					return false, err
				}
				result = order == 0
				stack = stack[:len(stack)-1]
				continue
			}
			if an.kind == jsonTextObject {
				if bn.kind != jsonTextObject {
					result = false
					stack = stack[:len(stack)-1]
					continue
				}
				f.object = true
				f.member = bn.child
			} else {
				if bn.kind == jsonTextObject {
					result = false
					stack = stack[:len(stack)-1]
					continue
				}
				if bn.kind == jsonTextArray {
					f.member = bn.child
				} else {
					f.member = f.right
				}
			}
			f.candidate = an.child
		}
		if f.waiting {
			f.waiting = false
			if result {
				if bn.kind != jsonTextArray && !f.object {
					f.member = -1
				} else {
					f.member = b.nodes[f.member].next
				}
				f.candidate = an.child
			} else {
				f.candidate = a.nodes[f.candidate].next
			}
		}
		if f.member < 0 {
			result = true
			stack = stack[:len(stack)-1]
			continue
		}
		if f.candidate < 0 {
			result = false
			stack = stack[:len(stack)-1]
			continue
		}
		candidate, member := a.nodes[f.candidate], b.nodes[f.member]
		if f.object {
			order, err := compareJSONBKeys(ctx, candidate.key, member.key)
			if err != nil {
				return false, err
			}
			if order < 0 {
				f.candidate = candidate.next
				continue
			}
			if order > 0 {
				result = false
				stack = stack[:len(stack)-1]
				continue
			}
		}
		if candidate.kind != member.kind {
			if f.object {
				result = false
				stack = stack[:len(stack)-1]
			} else {
				f.candidate = candidate.next
			}
			continue
		}
		if member.kind == jsonTextArray || member.kind == jsonTextObject {
			f.waiting = true
			stack = append(stack, jsonbContainFrame{left: f.candidate, right: f.member})
			continue
		}
		order, err := compareJSONBScalarNodes(ctx, candidate, member)
		if err != nil {
			return false, err
		}
		if order != 0 {
			if f.object {
				result = false
				stack = stack[:len(stack)-1]
			} else {
				f.candidate = candidate.next
			}
			continue
		}
		if bn.kind != jsonTextArray && !f.object {
			f.member = -1
		} else {
			f.member = member.next
		}
		f.candidate = an.child
	}
	return result, ctx.Err()
}
