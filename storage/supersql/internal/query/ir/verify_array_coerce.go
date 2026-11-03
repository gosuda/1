package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (g *Graph) validateArrayCoerce(ctx context.Context, n Scalar, args []ScalarID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) != 2 || n.CastContext < ExplicitCast || n.CastContext > ImplicitCast {
		return invalid("array coercion requires a source, template, and cast context")
	}
	source := g.scalars[args[0]-1]
	input, result := g.layouts[source.Layout-1], g.layouts[n.Layout-1]
	if input.typ.Storage != model.TypeArray || input.children.count != 1 || result.typ.Storage != model.TypeArray || result.children.count != 1 {
		return invalid("array coercion requires array source and result layouts")
	}
	if n.Nullable != source.Nullable {
		return invalid("array coercion nullability differs from source")
	}
	if !g.sameLayout(g.scalars[args[1]-1].Layout, g.layoutRefs[result.children.start]) {
		return invalid("array coercion template result differs from result element layout")
	}
	elementLayout := g.layoutRefs[input.children.start]
	for id := args[1]; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		step := g.scalars[id-1]
		operands := sliceAt(g.scalarRefs, step.operands)
		switch step.Kind {
		case ArrayElement:
			if len(operands) != 0 || !step.Nullable || step.Effects != (Effects{}) || !g.sameLayout(step.Layout, elementLayout) {
				return invalid("array coercion placeholder differs from source element")
			}
			return nil
		case Call, Cast:
			if len(operands) == 0 || len(operands) > 8 {
				return invalid("array coercion invocation requires one to eight operands")
			}
			for _, auxiliary := range operands[1:] {
				if g.scalars[auxiliary-1].Kind != Const {
					return invalid("array coercion auxiliary operands must be constants")
				}
			}
		case Relabel, Collate:
			if len(operands) != 1 {
				return invalid("array coercion relabel requires one operand")
			}
		default:
			return invalid("array coercion template requires an element conversion chain")
		}
		// Builder handles are topological; decreasing IDs bound this walk without a visited set.
		if operands[0] == 0 || operands[0] >= id {
			return invalid("array coercion template has a cycle or forward handle")
		}
		id = operands[0]
	}
}
