package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBDelete(signature Signature) scalarKernel {
	switch signature.ID {
	case 3302, 3303, 3304, 3343:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		arrayArgument := signature.ID == 3304 || signature.ID == 3343
		if arrayArgument {
			array := &args[1]
			if len(array.Dimensions) > 1 {
				return model.Value{}, sqlError("2202E", "wrong number of array subscripts")
			}
			if err := model.ValidateArrayShape(array.Dimensions, uint64(len(array.ArrayVal))); err != nil {
				return model.Value{}, sqlError("2202E", "invalid array dimensions")
			}
		}
		document, err := parseJSONBDocument(ctx, args[0].StrVal, budget)
		if err != nil {
			return model.Value{}, err
		}
		root := &document.nodes[0]
		if root.kind != jsonTextObject && root.kind != jsonTextArray {
			if signature.ID == 3304 {
				return model.Value{}, sqlError("22023", "cannot delete path in scalar")
			}
			return model.Value{}, sqlError("22023", "cannot delete from scalar")
		}
		if signature.ID == 3303 && root.kind == jsonTextObject {
			return model.Value{}, sqlError("22023", "cannot delete from object using integer index")
		}
		if root.child >= 0 {
			switch signature.ID {
			case 3302:
				err = document.deleteJSONBKeys(ctx, args[1:2])
			case 3303:
				var link *int
				link, err = document.jsonbDeleteIndexLink(ctx, 0, args[1].IntVal)
				if err == nil && link != nil {
					*link = document.nodes[*link].next
				}
			case 3304:
				err = document.deleteJSONBPath(ctx, args[1].ArrayVal, budget)
			case 3343:
				err = document.deleteJSONBKeys(ctx, args[1].ArrayVal)
			}
			if err != nil {
				return model.Value{}, err
			}
		}
		output, err := document.render(ctx, budget, false)
		if err != nil {
			return model.Value{}, err
		}
		return model.Value{Type: model.TypeJSONB, StrVal: output}, nil
	}
}

func (document jsonbDocument) deleteJSONBKeys(ctx context.Context, keys []model.Value) error {
	for i := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !keys[i].IsNull && keys[i].Type != model.TypeString {
			return sqlError("42804", "JSONB deletion requires text elements")
		}
	}
	root := &document.nodes[0]
	if len(keys) == 0 {
		return ctx.Err()
	}
	link := &root.child
	for *link >= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		child := &document.nodes[*link]
		text := child.key
		if root.kind == jsonTextArray {
			if child.kind != jsonTextString {
				link = &child.next
				continue
			}
			text = child.text
		}
		matched := false
		for i := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if keys[i].IsNull {
				continue
			}
			order, err := compareJSONBKeys(ctx, text, keys[i].StrVal)
			if err != nil {
				return err
			}
			if order == 0 {
				matched = true
				break
			}
		}
		if matched {
			*link = child.next
		} else {
			link = &child.next
		}
	}
	return nil
}

// Links refer only to this call's private node slice. No path or traversal
// workspace is allocated, and removed nodes remain charged until rendering ends.
func (document jsonbDocument) jsonbDeleteIndexLink(ctx context.Context, parent int, index int64) (*int, error) {
	if index < 0 {
		var count int64
		for child := document.nodes[parent].child; child >= 0; child = document.nodes[child].next {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			count++
		}
		index += count
		if index < 0 {
			return nil, nil
		}
	}
	link := &document.nodes[parent].child
	for *link >= 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if index == 0 {
			return link, nil
		}
		index--
		link = &document.nodes[*link].next
	}
	return nil, nil
}
