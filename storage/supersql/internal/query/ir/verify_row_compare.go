package ir

import "github.com/gosuda/supersql-alpha/internal/model"

func (g *Graph) validateRowCompare(n Scalar, args []ScalarID) error {
	switch n.RowComparison {
	case RowCompareLess, RowCompareLessEqual, RowCompareGreaterEqual, RowCompareGreater:
	default:
		return invalid("invalid row comparison strategy")
	}
	if len(args) < 2 || g.layouts[n.Layout-1].typ != (model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}) {
		return invalid("row comparison requires at least two comparators and a boolean result")
	}
	effects := Effects{MayError: true}
	for _, id := range args {
		call := g.scalars[id-1]
		if call.Kind != Call || call.Function == 0 || call.Operator != 0 || call.operands.count != 2 || g.layouts[call.Layout-1].typ != (model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}) {
			return invalid("row comparator must be a binary int4 function call")
		}
		if !validID(call.Signature, g.signatures) {
			return invalid("row comparator has no selected signature")
		}
		signature := g.signatures[call.Signature-1]
		if signature.VariadicArray || signature.Function != call.Function || signature.Operator != 0 || signature.Strict != call.Strict {
			return invalid("row comparator differs from selected signature")
		}
		if err := g.validateInvocation(call.Signature, call.operands, call.Layout, call.Nullable, call.Effects); err != nil {
			return err
		}
		if call.Nullable && !n.Nullable {
			return invalid("row comparison omits comparator nullability")
		}
		effects = effects.Union(call.Effects)
	}
	if n.Effects != effects {
		return invalid("row comparison effects differ from comparators and comparison check")
	}
	return nil
}
