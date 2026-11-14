package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBStrip(signature Signature) scalarKernel {
	if signature.ID == 3262 {
		return jsonbStripNulls
	}
	return nil
}

func jsonbStripNulls(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	document, err := parseJSONBDocument(ctx, args[0].StrVal, budget)
	if err != nil {
		return model.Value{}, err
	}
	stripArrays := args[1].IntVal != 0
	for index := range document.nodes {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		node := &document.nodes[index]
		if node.kind != jsonTextObject && (node.kind != jsonTextArray || !stripArrays) {
			continue
		}
		link := &node.child
		for *link >= 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			child := &document.nodes[*link]
			if child.kind == jsonTextNull {
				*link = child.next
			} else {
				link = &child.next
			}
		}
	}
	output, err := document.render(ctx, budget, false)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeJSONB, StrVal: output}, nil
}
