package normalize

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (n *normalizer) scalar(id ir.ScalarID, depth uint32) scalarResult {
	if id == 0 || !n.step(1) {
		return scalarResult{}
	}
	if depth >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return scalarResult{}
	}
	source, _ := n.g.Scalar(id)
	sameContext := n.scalars[id].context == n.scalarContext && n.scalars[id].projection == n.projectInput && n.scalars[id].bounds == n.foldBounds
	if n.scalars[id].id != 0 && (sameContext || source.Kind == ir.Const) {
		return n.scalars[id]
	}
	if n.scalarActive[id] {
		n.err = ErrInvalid
		return scalarResult{}
	}
	n.scalarActive[id] = true
	meta := source.Metadata()
	meta.Layout = n.layouts[source.Layout]
	var result scalarResult
	switch source.Kind {
	case ir.Const:
		value, err := n.g.CloneConstant(n.ctx, id, n.o.MaxBytes-n.bytes)
		if err != nil {
			n.err = err
			break
		}
		n.valueBytes(value)
		result = n.constant(meta, value)
	case ir.Slot:
		if selected, found := n.projectedSlot(source.Slot, depth); found {
			result = selected
			break
		}
		value, found := model.Value{}, false
		if n.foldBounds {
			value, found = n.constantSlot(source.Slot)
		}
		if found {
			result = n.constant(meta, value)
		} else {
			result = n.addScalar(meta, nil)
		}
	case ir.Case:
		result = n.caseScalar(id, source, meta, depth)
	case ir.Coalesce:
		result = n.coalesce(id, source, meta, depth)
	case ir.Bool:
		result = n.boolean(id, source, meta, depth)
	case ir.RowCompare:
		result = n.rowCompare(id, source, meta, depth)
	default:
		args := allocate[scalarResult](n, uint64(source.OperandCount()))
		for i := range args {
			child, _ := n.g.Operand(id, uint32(i))
			previous := n.foldBounds
			n.foldBounds = previous || ((source.Kind == ir.ArrayIndex || source.Kind == ir.ArraySlice) && i > 0)
			args[i] = n.scalar(child, depth+1)
			n.foldBounds = previous
		}
		if n.err != nil {
			break
		}
		if source.Relation != 0 {
			relation := source.Relation
			if source.Kind == ir.Subquery && source.SubqueryKind == ir.ExistsSubquery && n.simpleExistsInput(relation, depth+1) {
				body, _ := n.g.Rel(relation)
				if body.Kind == ir.Project && !body.SecurityBarrier {
					relation, _ = n.g.Input(relation, 0)
				}
			}
			normalized := n.relation(relation, depth+1)
			meta.Relation = normalized.id
			// Verification requires a Subquery scalar's effects to cover its
			// relation's effects (ir/verify_scalar.go). Lowering can add effects
			// the bound scalar never carried -- quantified_apply builds a
			// ScalarApply, and any Apply of that kind raises 21000, which
			// relationEffects reports as MayError. Draining the normalized
			// relation's effects here keeps the scalar a superset without
			// relaxing either verifier.
			meta.Effects = meta.Effects.Union(normalized.n.Effects)
		}
		if source.Kind == ir.Once || source.Kind == ir.Collate || source.Kind == ir.Relabel {
			// Relabels retain their result identity and layout after simplification.
			meta.Nullable, meta.Effects = args[0].n.Nullable, args[0].n.Effects
		}
		if source.Kind == ir.ArrayCoerce {
			meta.Nullable = args[0].n.Nullable
		}
		if source.Kind == ir.Field && args[0].n.Kind == ir.Row {
			// Normalize every field first: unused constant errors are still planning errors.
			result = args[0].row[source.FieldIndex]
			break
		}
		if value, ok := n.fold(source, args); ok {
			result = n.constant(meta, value)
		} else if source.Kind == ir.Subquery {
			comparisons := allocate[ir.SignatureID](n, uint64(source.ComparisonCount()))
			for i := range comparisons {
				comparisons[i], _ = n.g.Comparison(id, uint32(i))
			}
			result = n.addScalarComparisons(meta, args, comparisons)
		} else {
			result = n.addScalar(meta, args)
		}
	}
	if n.err == nil && result.sourceLayout != 0 && !n.samePresentation(source.Layout, result.sourceLayout) {
		relabel := ir.Scalar{Kind: ir.Relabel, Scope: meta.Scope, Layout: meta.Layout, Nullable: result.n.Nullable, Effects: result.n.Effects, CastContext: ir.ImplicitCast, Span: meta.Span}
		result = n.addScalar(relabel, []scalarResult{result})
	}
	result.sourceLayout = source.Layout
	n.scalarActive[id] = false
	result.context = n.scalarContext
	result.projection = n.projectInput
	result.bounds = n.foldBounds
	n.scalars[id] = result
	return result
}

func (n *normalizer) addScalar(meta ir.Scalar, args []scalarResult) scalarResult {
	return n.addScalarComparisons(meta, args, nil)
}

func (n *normalizer) addScalarComparisons(meta ir.Scalar, args []scalarResult, comparisons []ir.SignatureID) scalarResult {
	ids := allocate[ir.ScalarID](n, uint64(len(args)))
	if n.err != nil {
		return scalarResult{}
	}
	if meta.Kind == ir.ArrayStack || meta.Kind == ir.RowCompare {
		meta.Effects = ir.Effects{MayError: true}
	}
	for i, arg := range args {
		ids[i] = arg.id
		meta.Effects = meta.Effects.Union(arg.n.Effects)
	}
	id, err := n.b.AddScalar(n.ctx, ir.ScalarSpec{Scalar: meta, Operands: ids, Comparisons: comparisons})
	n.err = err
	result := scalarResult{id: id, n: meta}
	if meta.Kind == ir.Row {
		result.row = args
	}
	return result
}

func (n *normalizer) constant(meta ir.Scalar, value model.Value) scalarResult {
	if n.err != nil {
		return scalarResult{}
	}
	meta = ir.Scalar{Kind: ir.Const, Scope: meta.Scope, Layout: meta.Layout, Nullable: value.IsNull, Span: meta.Span}
	id, err := n.b.AddScalar(n.ctx, ir.ScalarSpec{Scalar: meta, Value: value})
	n.err = err
	return scalarResult{id: id, n: meta, value: value}
}

func (n *normalizer) caseScalar(id ir.ScalarID, source, meta ir.Scalar, depth uint32) scalarResult {
	args := allocate[scalarResult](n, uint64(source.OperandCount()))
	if n.err != nil {
		return scalarResult{}
	}
	used := 0
	for i := uint32(0); i+1 < source.OperandCount(); i += 2 {
		condition, _ := n.g.Operand(id, i)
		when := n.scalar(condition, depth+1)
		if n.err != nil {
			return scalarResult{}
		}
		if when.n.Kind == ir.Const && (when.value.IsNull || when.value.IntVal == 0) {
			continue
		}
		branch, _ := n.g.Operand(id, i+1)
		then := n.scalar(branch, depth+1)
		if when.n.Kind == ir.Const {
			if used == 0 {
				return then
			}
			args[used] = then
			return n.addScalar(meta, args[:used+1])
		}
		args[used], args[used+1] = when, then
		used += 2
	}
	last, _ := n.g.Operand(id, source.OperandCount()-1)
	otherwise := n.scalar(last, depth+1)
	if used == 0 {
		return otherwise
	}
	args[used] = otherwise
	return n.addScalar(meta, args[:used+1])
}

func (n *normalizer) coalesce(id ir.ScalarID, source, meta ir.Scalar, depth uint32) scalarResult {
	args := allocate[scalarResult](n, uint64(source.OperandCount()))
	if n.err != nil {
		return scalarResult{}
	}
	used := 0
	var last scalarResult
	for i := uint32(0); i < source.OperandCount(); i++ {
		child, _ := n.g.Operand(id, i)
		last = n.scalar(child, depth+1)
		if n.err != nil {
			return scalarResult{}
		}
		if last.n.Kind == ir.Const && last.value.IsNull {
			continue
		}
		args[used] = last
		used++
		if !last.n.Nullable {
			break
		}
	}
	if used == 0 {
		return last
	}
	if used == 1 {
		return args[0]
	}
	return n.addScalar(meta, args[:used])
}

func (n *normalizer) boolean(id ir.ScalarID, source, meta ir.Scalar, depth uint32) scalarResult {
	args := allocate[scalarResult](n, uint64(source.OperandCount()))
	if n.err != nil {
		return scalarResult{}
	}
	used, allConstant, hasNull := 0, true, false
	for i := uint32(0); i < source.OperandCount(); i++ {
		child, _ := n.g.Operand(id, i)
		arg := n.scalar(child, depth+1)
		if n.err != nil {
			return scalarResult{}
		}
		if source.BoolOp == ir.Not {
			if arg.n.Kind != ir.Const {
				return n.addScalar(meta, []scalarResult{arg})
			}
			value := arg.value
			if !value.IsNull {
				value = model.BoolVal(value.IntVal == 0)
			}
			return n.constant(meta, value)
		}
		if arg.n.Kind == ir.Const {
			if arg.value.IsNull {
				hasNull = true
			} else {
				decisive := (source.BoolOp == ir.And && arg.value.IntVal == 0) || (source.BoolOp == ir.Or && arg.value.IntVal == 1)
				if !decisive {
					continue
				}
				if allConstant {
					return n.constant(meta, arg.value)
				}
				args[used] = arg
				used++
				break
			}
		} else {
			allConstant = false
		}
		args[used] = arg
		used++
	}
	if allConstant {
		value := model.BoolVal(source.BoolOp == ir.And)
		if hasNull {
			value = model.NullVal(model.TypeBoolean)
		}
		return n.constant(meta, value)
	}
	if used == 1 {
		return args[0]
	}
	return n.addScalar(meta, args[:used])
}

func (n *normalizer) fold(source ir.Scalar, args []scalarResult) (model.Value, bool) {
	relabel := source.Kind == ir.Collate || source.Kind == ir.Relabel
	unfoldable := source.Kind == ir.Once || (!relabel && source.Effects.Volatility != ir.Immutable)
	if n.err != nil || unfoldable {
		return model.Value{}, false
	}
	for _, arg := range args {
		if arg.n.Kind != ir.Const {
			return model.Value{}, false
		}
	}
	if relabel {
		value := args[0].value
		if source.Kind == ir.Relabel && !value.IsNull {
			from, _ := n.g.Type(args[0].sourceLayout)
			to, _ := n.g.Type(source.Layout)
			if from.ID == 23 && builtin.IsOIDType(to.ID) {
				value.IntVal = int64(uint32(value.IntVal))
			} else if builtin.IsOIDType(from.ID) && to.ID == 23 {
				value.IntVal = int64(int32(uint32(value.IntVal)))
			}
		}
		return value, true
	}
	if source.Kind == ir.NullTest {
		value := args[0].value
		isNull, isNotNull := value.IsNull, !value.IsNull
		if !value.IsNull && value.Type == model.TypeComposite {
			isNull, isNotNull = true, true
			for _, field := range value.ArrayVal {
				isNull = isNull && field.IsNull
				isNotNull = isNotNull && !field.IsNull
			}
		}
		if source.Negated {
			return model.BoolVal(isNotNull), true
		}
		return model.BoolVal(isNull), true
	}
	if source.Kind != ir.Call && source.Kind != ir.Cast {
		return model.Value{}, false
	}
	identity := source.Function
	if source.Operator != 0 {
		op, ok := builtin.LookupOperator(source.Operator)
		if !ok {
			return model.Value{}, false
		}
		identity = op.Function
	}
	sig, known := builtin.LookupSignature(identity)
	if !known || sig.Volatility != 'i' || sig.ReturnsSet || int(sig.ArgumentCount) != len(args) || sig.Strict != source.Strict {
		return model.Value{}, false
	}
	resultType, _ := n.g.Type(source.Layout)
	if sig.Result != resultType.ID {
		return model.Value{}, false
	}
	bound, _ := n.g.Signature(source.Signature)
	var types [8]model.Type
	var values [8]model.Value
	options := n.o.Builtin
	for i, arg := range args {
		layout, _ := n.g.SignatureParameter(source.Signature, uint32(i))
		types[i], _ = n.g.Type(layout)
		values[i] = arg.value
	}
	if bound.Effects.Volatility != ir.Immutable {
		return model.Value{}, false
	}
	options.Collation = builtin.InputCollation(bound.InputCollation, types[:len(args)])
	callable, err := builtin.Resolve(identity, options)
	if err != nil {
		n.foldError(err)
		return model.Value{}, false
	}
	if err := callable.ValidateTypes(types[:len(args)], resultType); err != nil {
		return model.Value{}, false
	}
	if !n.step(1) {
		return model.Value{}, false
	}
	remaining := n.o.MaxBytes - n.bytes
	value, err := callable.Call(n.ctx, values[:len(args)], builtin.CallBudget{OutputBytes: int64(remaining), MemoryBytes: remaining})
	if err != nil {
		n.foldError(err)
		return model.Value{}, false
	}
	n.valueBytes(value)
	return value, n.err == nil
}

func (n *normalizer) foldError(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		n.err = err
		return
	}
	var missing *builtin.MissingImplementationError
	var unknown *builtin.UnknownFunctionError
	if errors.As(err, &missing) || errors.As(err, &unknown) || errors.Is(err, builtin.ErrTypeBindingRequired) {
		return
	}
	n.err = err
}

func (n *normalizer) samePresentation(left, right ir.LayoutID) bool {
	if !n.step(1) {
		return false
	}
	if left == right {
		return true
	}
	count := n.g.LayoutFieldNameCount(left)
	if count != n.g.LayoutFieldNameCount(right) || n.g.LayoutChildCount(left) != n.g.LayoutChildCount(right) {
		return false
	}
	for i := range count {
		if !n.step(1) {
			return false
		}
		a, _ := n.g.LayoutFieldName(left, i)
		b, _ := n.g.LayoutFieldName(right, i)
		if a != b {
			return false
		}
	}
	for i := range n.g.LayoutChildCount(left) {
		a, _ := n.g.LayoutChild(left, i)
		b, _ := n.g.LayoutChild(right, i)
		if !n.samePresentation(a, b) {
			return false
		}
	}
	return true
}
