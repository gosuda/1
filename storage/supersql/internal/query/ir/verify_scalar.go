package ir

import (
	"context"
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func validID[I ~uint32, T any](id I, values []T) bool {
	return id != 0 && uint64(id) <= uint64(len(values))
}
func validExtent[T any](r extent, values []T) bool {
	return uint64(r.start)+uint64(r.count) <= uint64(len(values))
}
func sliceAt[T any](values []T, r extent) []T {
	return values[int(r.start):int(uint64(r.start)+uint64(r.count))]
}
func (g *Graph) ancestor(want, scopeID ScopeID) bool {
	for scopeID != 0 {
		if scopeID == want {
			return true
		}
		scopeID = g.scopes[scopeID-1].parent
	}
	return false
}
func (g *Graph) visible(slot model.SlotID, scopeID ScopeID) bool {
	if !validID(slot, g.slots) || !validID(scopeID, g.scopes) {
		return false
	}
	if g.slots[slot-1].Scope == scopeID {
		return true
	}
	return contains(sliceAt(g.slotRefs, g.scopes[scopeID-1].captures), slot)
}
func contains[T comparable](values []T, value T) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func (g *Graph) sameLayout(a, b LayoutID) bool {
	if a == b {
		return true
	}
	left, right := g.layouts[a-1], g.layouts[b-1]
	if left.typ != right.typ || left.children.count != right.children.count {
		return false
	}
	for i, child := range sliceAt(g.layoutRefs, left.children) {
		if !g.sameLayout(child, g.layoutRefs[int(right.children.start)+i]) {
			return false
		}
	}
	return true
}
func (g *Graph) boolean(id ScalarID) bool {
	return validID(id, g.scalars) && g.layouts[g.scalars[id-1].Layout-1].typ.Storage == model.TypeBoolean
}
func (g *Graph) validateValue(ctx context.Context, id LayoutID, value model.Value, depth, maximum uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= maximum {
		return limitError()
	}
	l := g.layouts[id-1]
	if value.Type != l.typ.Storage {
		return invalid("constant storage type differs from layout")
	}
	if value.IsNull {
		return nil
	}
	switch value.Type {
	case model.TypeArray:
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return invalid("invalid constant array shape")
		}
		for _, v := range value.ArrayVal {
			if err := g.validateValue(ctx, g.layoutRefs[l.children.start], v, depth+1, maximum); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(value.ArrayVal) != int(l.children.count) {
			return invalid("constant row width differs from layout")
		}
		for i, v := range value.ArrayVal {
			if err := g.validateValue(ctx, g.layoutRefs[int(l.children.start)+i], v, depth+1, maximum); err != nil {
				return err
			}
		}
	case model.TypeBoolean:
		if value.IntVal != 0 && value.IntVal != 1 {
			return invalid("invalid boolean constant")
		}
	}
	return nil
}
func (g *Graph) validateScalar(ctx context.Context, n Scalar, limits Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	invalidKind := n.Kind < Const || n.Kind > JSONBStore
	invalidScopeOrLayout := !validID(n.Scope, g.scopes) || !validID(n.Layout, g.layouts)
	invalidSpan := n.Span.Start > n.Span.End || n.Span.End > limits.SourceBytes
	if invalidKind || invalidScopeOrLayout || invalidSpan || n.Effects.Volatility > Volatile || !validExtent(n.operands, g.scalarRefs) || !validExtent(n.comparisons, g.comparisonRefs) {
		return invalid("invalid scalar metadata or operand range")
	}
	args := sliceAt(g.scalarRefs, n.operands)
	comparisons := sliceAt(g.comparisonRefs, n.comparisons)
	for _, id := range args {
		if !validID(id, g.scalars) || g.scalars[id-1].Scope != n.Scope {
			return invalid("forward scalar handle or cross-scope operand")
		}
		if n.Effects.Union(g.scalars[id-1].Effects) != n.Effects {
			return invalid("scalar effects omit operand effects")
		}
	}
	allowed := Scalar{Kind: n.Kind, Scope: n.Scope, Layout: n.Layout, Nullable: n.Nullable, Effects: n.Effects, Span: n.Span, operands: n.operands}
	result := g.layouts[n.Layout-1]
	switch n.Kind {
	case ArrayElement:
		if len(args) != 0 || !n.Nullable || n.Effects != (Effects{}) {
			return invalid("array element placeholder must be nullable and effect-free")
		}
	case ArrayCoerce:
		allowed.CastContext = n.CastContext
		if err := g.validateArrayCoerce(ctx, n, args); err != nil {
			return err
		}
	case ArrayIndex, ArraySlice:
		if n.Kind == ArraySlice {
			allowed.Subscripts = n.Subscripts
		}
		if err := g.validateArraySubscript(n, args); err != nil {
			return err
		}
	case ArrayStore:
		allowed.Subscripts, allowed.SliceAssignment = n.Subscripts, n.SliceAssignment
		if err := g.validateArrayStore(n, args); err != nil {
			return err
		}
	case JSONBStore:
		if err := g.validateJSONBStore(n, args); err != nil {
			return err
		}
	case Once:
		if len(args) != 1 {
			return invalid("single evaluation requires one operand")
		}
		input := g.scalars[args[0]-1]
		if !g.sameLayout(n.Layout, input.Layout) || n.Nullable != input.Nullable || n.Effects != input.Effects {
			return invalid("single evaluation metadata differs from operand")
		}
	case Collate, Relabel:
		if len(args) != 1 {
			return invalid("collation relabel requires one operand")
		}
		input := g.scalars[args[0]-1]
		operand := g.layouts[input.Layout-1]
		typ := operand.typ
		if n.Kind == Collate {
			typ.Collation = result.typ.Collation
		} else {
			allowed.CastContext = n.CastContext
			if n.CastContext < ExplicitCast || n.CastContext > ImplicitCast {
				return invalid("binary relabel requires a cast context")
			}
			if result.typ.Modifier != -1 && result.typ.Modifier != typ.Modifier {
				return invalid("binary relabel narrows type modifier")
			}
			typ.ID, typ.Modifier = result.typ.ID, result.typ.Modifier
		}
		if typ != result.typ || operand.children.count != result.children.count || n.Nullable != input.Nullable || n.Effects != input.Effects {
			return invalid("collation relabel changes operand metadata")
		}
		for i, child := range sliceAt(g.layoutRefs, operand.children) {
			if !g.sameLayout(child, g.layoutRefs[int(result.children.start)+i]) {
				return invalid("collation relabel changes nested layout")
			}
		}
	case Const:
		allowed.constant = n.constant
		if len(args) != 0 || !validID(n.constant, g.values) || g.values[n.constant-1].Type != result.typ.Storage || g.values[n.constant-1].IsNull != n.Nullable || n.Effects != (Effects{}) {
			return invalid("invalid constant metadata")
		}
	case Slot:
		allowed.Slot = n.Slot
		if len(args) != 0 || !g.visible(n.Slot, n.Scope) || !g.sameLayout(n.Layout, g.slots[n.Slot-1].Layout) || n.Nullable != g.slots[n.Slot-1].Nullable || n.Effects != (Effects{}) {
			return invalid("invalid slot reference")
		}
	case Param:
		allowed.Parameter = n.Parameter
		if len(args) != 0 || n.Parameter == 0 || n.Effects != (Effects{}) {
			return invalid("invalid parameter")
		}
		if n.Parameter > limits.MaxOperands {
			return limitError()
		}
		for _, previous := range g.scalars {
			if previous.Kind == Param && previous.Parameter == n.Parameter && !g.sameLayout(previous.Layout, n.Layout) {
				return invalid("parameter has inconsistent resolved types")
			}
		}
	case SQLJSON:
		allowed.JSONOperation = n.JSONOperation
		allowed.JSONFormat = n.JSONFormat
		allowed.JSONEncoding = n.JSONEncoding
		allowed.JSONOutputFormat = n.JSONOutputFormat
		allowed.JSONOutputEncoding = n.JSONOutputEncoding
		allowed.JSONWrapper = n.JSONWrapper
		allowed.JSONQuotes = n.JSONQuotes
		allowed.JSONItemType = n.JSONItemType
		allowed.JSONFlags = n.JSONFlags
		allowed.JSONPassing = n.JSONPassing
		if n.JSONOperation < SQLJSONParse || n.JSONOperation > SQLJSONQuery ||
			n.JSONFormat < SQLJSONDefaultFormat || n.JSONFormat > SQLJSONBinaryFormat ||
			n.JSONEncoding < SQLJSONDefaultEncoding || n.JSONEncoding > SQLJSONUTF32 ||
			n.JSONOutputFormat < SQLJSONDefaultFormat || n.JSONOutputFormat > SQLJSONBinaryFormat ||
			n.JSONOutputEncoding < SQLJSONDefaultEncoding || n.JSONOutputEncoding > SQLJSONUTF32 ||
			n.JSONWrapper < SQLJSONUnspecifiedWrapper || n.JSONWrapper > SQLJSONUnconditionalWrapper ||
			n.JSONQuotes < SQLJSONUnspecifiedQuotes || n.JSONQuotes > SQLJSONOmitQuotes ||
			n.JSONItemType < SQLJSONAnyItem || n.JSONItemType > SQLJSONScalarItem {
			return invalid("invalid SQL/JSON metadata")
		}
		validArity := false
		switch n.JSONOperation {
		case SQLJSONParse, SQLJSONScalar, SQLJSONSerialize, SQLJSONIs:
			validArity = len(args) == 1 && n.JSONPassing == 0
		case SQLJSONArray:
			validArity = n.JSONPassing == 0
		case SQLJSONObject:
			validArity = len(args)%2 == 0 && n.JSONPassing == 0
		case SQLJSONExists, SQLJSONValue, SQLJSONQuery:
			validArity = len(args) == 2+4*int(n.JSONPassing)
		}
		effects := Effects{MayError: true}
		for _, id := range args {
			effects = effects.Union(g.scalars[id-1].Effects)
		}
		// SQL/JSON volatility also follows the argument types: a PASSING value
		// whose output function is stable (date, timestamptz) makes the whole
		// construct stable even though its operand effects carry no volatility.
		// Operand effects must therefore be contained by the declared effects.
		if !validArity || n.Effects.Union(effects) != n.Effects {
			return invalid("invalid SQL/JSON operands or effects")
		}
	case Call, Cast:
		allowed.Function, allowed.Operator, allowed.Strict, allowed.Signature = n.Function, n.Operator, n.Strict, n.Signature
		if err := g.validateInvocation(n.Signature, n.operands, n.Layout, n.Nullable, n.Effects); err != nil {
			return err
		}
		signature := g.signatures[n.Signature-1]
		if signature.VariadicArray && n.Kind != Call {
			return invalid("VARIADIC is only valid on function calls")
		}
		if n.Function != signature.Function || n.Operator != signature.Operator || n.Strict != signature.Strict {
			return invalid("call identity differs from resolved signature")
		}
		if n.Kind == Cast {
			allowed.CastContext = n.CastContext
			if len(args) != 1 || n.CastContext < ExplicitCast || n.CastContext > ImplicitCast {
				return invalid("invalid cast")
			}
		}
	case Least, Greatest:
		allowed.Function, allowed.Operator, allowed.Strict, allowed.Signature = n.Function, n.Operator, n.Strict, n.Signature
		if err := g.validateMinMax(n, args); err != nil {
			return err
		}
	case RowCompare:
		allowed.RowComparison = n.RowComparison
		if err := g.validateRowCompare(n, args); err != nil {
			return err
		}
	case Case:
		if len(args) < 3 || len(args)%2 != 1 {
			return invalid("CASE requires WHEN/THEN pairs and ELSE")
		}
		for i, id := range args {
			if i < len(args)-1 && i%2 == 0 {
				if !g.boolean(id) {
					return invalid("CASE condition is not boolean")
				}
				continue
			}
			if !g.sameLayout(n.Layout, g.scalars[id-1].Layout) || (g.scalars[id-1].Nullable && !n.Nullable) {
				return invalid("CASE result type or nullability mismatch")
			}
		}
	case Coalesce:
		if len(args) == 0 {
			return invalid("COALESCE needs operands")
		}
		allNullable := true
		for _, id := range args {
			if !g.sameLayout(n.Layout, g.scalars[id-1].Layout) {
				return invalid("COALESCE type mismatch")
			}
			allNullable = allNullable && g.scalars[id-1].Nullable
		}
		if allNullable && !n.Nullable {
			return invalid("COALESCE nullability mismatch")
		}
	case NullTest:
		allowed.Negated = n.Negated
		if len(args) != 1 || result.typ.Storage != model.TypeBoolean || n.Nullable {
			return invalid("invalid null test")
		}
	case Bool:
		allowed.BoolOp = n.BoolOp
		invalidOperator := n.BoolOp < And || n.BoolOp > Not
		invalidUnaryArity := n.BoolOp == Not && len(args) != 1
		invalidVariadicArity := n.BoolOp != Not && len(args) < 2
		if invalidOperator || invalidUnaryArity || invalidVariadicArity || result.typ.Storage != model.TypeBoolean {
			return invalid("invalid boolean arity or type")
		}
		for _, id := range args {
			if !g.boolean(id) || (g.scalars[id-1].Nullable && !n.Nullable) {
				return invalid("boolean operand mismatch")
			}
		}
	case ArrayStack:
		if len(args) == 0 || result.typ.Storage != model.TypeArray || n.Nullable {
			return invalid("array stack requires array operands and a nonnull array result")
		}
		effects := Effects{MayError: true}
		for _, id := range args {
			operand := g.scalars[id-1]
			if !g.sameLayout(n.Layout, operand.Layout) {
				return invalid("array stack operand layout differs from result")
			}
			effects = effects.Union(operand.Effects)
		}
		if n.Effects != effects {
			return invalid("array stack effects differ from operands and shape check")
		}
	case Row, Array:
		invalidRow := n.Kind == Row && (result.typ.Storage != model.TypeComposite || uint64(len(args)) != uint64(result.children.count))
		invalidArray := n.Kind == Array && result.typ.Storage != model.TypeArray
		if n.Nullable || invalidRow || invalidArray {
			return invalid("row or array result mismatch")
		}
		for i, id := range args {
			index := 0
			if n.Kind == Row {
				index = i
			}
			if !g.sameLayout(g.scalars[id-1].Layout, g.layoutRefs[int(result.children.start)+index]) {
				return invalid("row or array element mismatch")
			}
		}
	case Field, FieldStore:
		allowed.FieldIndex = n.FieldIndex
		expected := 1
		if n.Kind == FieldStore {
			expected = 2
		}
		if len(args) != expected {
			return invalid("field operation has invalid operands")
		}
		parent := g.scalars[args[0]-1]
		composite := g.layouts[parent.Layout-1]
		if composite.typ.Storage != model.TypeComposite || n.FieldIndex >= composite.children.count {
			return invalid("field operation requires an existing composite child")
		}
		child := g.layoutRefs[composite.children.start+n.FieldIndex]
		if n.Kind == FieldStore {
			replacement := g.scalars[args[1]-1]
			if !g.sameLayout(n.Layout, parent.Layout) || !g.sameLayout(replacement.Layout, child) || n.Nullable {
				return invalid("field store layout mismatch")
			}
			break
		}
		if !g.sameLayout(n.Layout, child) {
			return invalid("field extraction layout mismatch")
		}
		// Layouts carry child types, not child nullability. Only a Row constructor
		// establishes whether its selected child can be NULL.
		nullable := true
		if parent.Kind == Row {
			childID := g.scalarRefs[parent.operands.start+n.FieldIndex]
			nullable = g.scalars[childID-1].Nullable
		}
		if (parent.Nullable || nullable) && !n.Nullable {
			return invalid("field extraction omits parent or child nullability")
		}
	case ArrayQuantified:
		allowed.Signature, allowed.Function, allowed.Operator, allowed.Strict, allowed.SubqueryKind = n.Signature, n.Function, n.Operator, n.Strict, n.SubqueryKind
		if len(args) != 2 || len(comparisons) != 0 || (n.SubqueryKind != AnySubquery && n.SubqueryKind != AllSubquery) || result.typ.Storage != model.TypeBoolean || !n.Nullable || n.Operator == 0 || n.Function != 0 || !validID(n.Signature, g.signatures) {
			return invalid("invalid quantified array comparison")
		}
		signature := g.signatures[n.Signature-1]
		array := g.layouts[g.scalars[args[1]-1].Layout-1]
		if array.typ.Storage != model.TypeArray || array.children.count != 1 || signature.parameters.count != 2 || signature.SetReturning || (signature.Function == 0) == (signature.Operator == 0) || signature.Function != n.Function || signature.Operator != n.Operator || signature.Strict != n.Strict || !g.sameLayout(signature.Result, n.Layout) || n.Effects.Union(signature.Effects) != n.Effects {
			return invalid("quantified array comparison signature mismatch")
		}
		if !g.sameLayout(g.layoutRefs[signature.parameters.start], g.scalars[args[0]-1].Layout) || !g.sameLayout(g.layoutRefs[signature.parameters.start+1], g.layoutRefs[array.children.start]) {
			return invalid("quantified array comparison argument mismatch")
		}
	case Subquery:
		allowed.SubqueryKind, allowed.Relation, allowed.RowComparison, allowed.comparisons = n.SubqueryKind, n.Relation, n.RowComparison, n.comparisons
		if !validID(n.Relation, g.rels) || n.SubqueryKind < ScalarSubquery || n.SubqueryKind > TupleSubquery {
			return invalid("invalid subquery reference")
		}
		rel := g.rels[n.Relation-1]
		if n.Effects.Union(rel.Effects) != n.Effects {
			return invalid("subquery effects omit relational effects")
		}
		if g.scopes[rel.Scope-1].parent != n.Scope {
			return invalid("subquery scope must be a child scope")
		}
		switch n.SubqueryKind {
		case ScalarSubquery:
			if len(args) != 0 || len(comparisons) != 0 || rel.outputs.count != 1 || !g.sameLayout(n.Layout, g.slots[g.slotRefs[rel.outputs.start]-1].Layout) || !n.Nullable || !n.Effects.MayError {
				return invalid("scalar subquery requires nullable matching result and cardinality-error effect")
			}
		case ExistsSubquery:
			if len(args) != 0 || len(comparisons) != 0 || result.typ.Storage != model.TypeBoolean || n.Nullable {
				return invalid("invalid EXISTS result")
			}
		case TupleSubquery:
			if len(args) != 0 || len(comparisons) != 0 || result.typ.Storage != model.TypeComposite || n.Nullable || !n.Effects.MayError || rel.outputs.count != result.children.count {
				return invalid("invalid tuple subquery result")
			}
			for i := uint32(0); i < rel.outputs.count; i++ {
				output := g.slots[g.slotRefs[rel.outputs.start+i]-1].Layout
				if !g.sameLayout(g.layoutRefs[result.children.start+i], output) {
					return invalid("tuple subquery child layout mismatch")
				}
			}
		case ArraySubquery:
			if len(args) != 0 || len(comparisons) != 0 || rel.outputs.count != 1 || result.typ.Storage != model.TypeArray || result.children.count != 1 || n.Nullable || !n.Effects.MayError {
				return invalid("invalid ARRAY subquery result")
			}
			output := g.slots[g.slotRefs[rel.outputs.start]-1].Layout
			nested := g.layouts[output-1].typ.Storage == model.TypeArray && g.sameLayout(n.Layout, output)
			if !nested && !g.sameLayout(g.layoutRefs[result.children.start], output) {
				return invalid("ARRAY subquery element layout mismatch")
			}
		case AnySubquery, AllSubquery, RowSubquery:
			width := int(rel.outputs.count)
			if width == 0 || len(args) != width || len(comparisons) != width || result.typ.Storage != model.TypeBoolean || !n.Nullable {
				return invalid("invalid row-comparison subquery")
			}
			if n.SubqueryKind == RowSubquery && (!n.Effects.MayError || n.RowComparison == 0) {
				return invalid("row subquery needs row semantics and cardinality-error effect")
			}
			if n.RowComparison != 0 && !validSubqueryRowComparison(n.RowComparison) {
				return invalid("invalid row-comparison strategy")
			}
			if width > 1 && n.RowComparison == 0 {
				return invalid("multi-column comparison needs row semantics")
			}
			for i, signatureID := range comparisons {
				if err := g.validateSubqueryComparison(n, args[i], g.slotRefs[int(rel.outputs.start)+i], signatureID); err != nil {
					return err
				}
			}
		}
	}
	if len(comparisons) != 0 && n.Kind != Subquery {
		return invalid("comparison signatures on another scalar kind")
	}
	if allowed != n {
		return invalid("scalar contains payload for another kind")
	}
	return nil
}

func validSubqueryRowComparison(kind RowComparisonKind) bool {
	switch kind {
	case RowCompareLess, RowCompareLessEqual, RowCompareEqual, RowCompareGreaterEqual, RowCompareGreater, RowCompareNotEqual:
		return true
	default:
		return false
	}
}

func (g *Graph) validateSubqueryComparison(n Scalar, left ScalarID, right model.SlotID, signatureID SignatureID) error {
	if !validID(signatureID, g.signatures) {
		return invalid("subquery comparison signature missing")
	}
	signature := g.signatures[signatureID-1]
	if signature.parameters.count != 2 || signature.SetReturning || (signature.Function == 0) == (signature.Operator == 0) || n.Effects.Union(signature.Effects) != n.Effects {
		return invalid("invalid subquery comparison signature")
	}
	if !g.sameLayout(g.layoutRefs[signature.parameters.start], g.scalars[left-1].Layout) || !g.sameLayout(g.layoutRefs[signature.parameters.start+1], g.slots[right-1].Layout) {
		return invalid("subquery comparison argument mismatch")
	}
	booleanResult := n.RowComparison == 0 || n.RowComparison == RowCompareEqual || n.RowComparison == RowCompareNotEqual
	result := g.layouts[signature.Result-1]
	if booleanResult {
		if !g.sameLayout(signature.Result, n.Layout) {
			return invalid("subquery comparison must return boolean")
		}
	} else if result.typ.ID != 23 || result.typ.Storage != model.TypeInt64 || result.children.count != 0 {
		return invalid("ordered subquery comparison must return int4")
	}
	return nil
}

func (g *Graph) validateArraySubscript(n Scalar, args []ScalarID) error {
	if len(args) == 0 || !n.Nullable {
		return invalid("array fetch requires an array source and nullable result")
	}
	source := g.layouts[g.scalars[args[0]-1].Layout-1]
	if source.typ.Storage != model.TypeArray || source.children.count != 1 {
		return invalid("array fetch source must have one element layout")
	}
	if n.Kind == ArrayIndex {
		if len(args) < 2 || len(args) > 7 || !g.sameLayout(n.Layout, g.layoutRefs[source.children.start]) {
			return invalid("array index requires one to six indexes and the exact element layout")
		}
	} else {
		subscripts := n.Subscripts
		if subscripts.Dimensions < 1 || subscripts.Dimensions > 6 {
			return invalid("array slice requires one to six dimensions")
		}
		mask := uint8(1<<subscripts.Dimensions) - 1
		if (subscripts.Upper|subscripts.Lower)&^mask != 0 || len(args) != 1+bits.OnesCount8(subscripts.Upper)+bits.OnesCount8(subscripts.Lower) {
			return invalid("array slice bound masks differ from operands")
		}
		if !g.sameLayout(n.Layout, g.scalars[args[0]-1].Layout) {
			return invalid("array slice result differs from source layout")
		}
	}
	for i, id := range args {
		operand := g.scalars[id-1]
		if i != 0 {
			layout := g.layouts[operand.Layout-1]
			if layout.typ.ID != 23 || layout.typ.Storage != model.TypeInt64 || layout.children.count != 0 {
				return invalid("array fetch indexes must be int4")
			}
		}
	}
	return nil
}

func (g *Graph) validateArrayStore(n Scalar, args []ScalarID) error {
	if len(args) < 3 || n.Nullable || n.Subscripts.Dimensions < 1 || n.Subscripts.Dimensions > 6 {
		return invalid("array store requires source, indexes, replacement, and a nonnull result")
	}
	source := g.scalars[args[0]-1]
	layout := g.layouts[source.Layout-1]
	if layout.typ.Storage != model.TypeArray || layout.children.count != 1 || !g.sameLayout(n.Layout, source.Layout) {
		return invalid("array store source and result layout mismatch")
	}
	mask := uint8(1<<n.Subscripts.Dimensions) - 1
	if (n.Subscripts.Upper|n.Subscripts.Lower)&^mask != 0 {
		return invalid("array store bound mask exceeds its dimensions")
	}
	bounds := len(args) - 2
	if n.SliceAssignment {
		if bounds != bits.OnesCount8(n.Subscripts.Upper)+bits.OnesCount8(n.Subscripts.Lower) {
			return invalid("array slice store bound masks differ from operands")
		}
		if !g.sameLayout(g.scalars[args[len(args)-1]-1].Layout, source.Layout) {
			return invalid("array slice replacement layout mismatch")
		}
	} else {
		if n.Subscripts.Upper != 0 || n.Subscripts.Lower != 0 || bounds != int(n.Subscripts.Dimensions) || !g.sameLayout(g.scalars[args[len(args)-1]-1].Layout, g.layoutRefs[layout.children.start]) {
			return invalid("array element replacement layout mismatch")
		}
	}
	for _, id := range args[1 : len(args)-1] {
		bound := g.layouts[g.scalars[id-1].Layout-1]
		if bound.typ.ID != 23 || bound.typ.Storage != model.TypeInt64 || bound.children.count != 0 {
			return invalid("array store indexes must be int4")
		}
	}
	return nil
}

func (g *Graph) validateJSONBStore(n Scalar, args []ScalarID) error {
	if len(args) < 3 || n.Nullable {
		return invalid("jsonb store requires source, subscripts, replacement, and a nonnull result")
	}
	source := g.scalars[args[0]-1]
	replacement := g.scalars[args[len(args)-1]-1]
	layout := g.layouts[source.Layout-1]
	if layout.typ.ID != 3802 || layout.typ.Storage != model.TypeJSONB || !g.sameLayout(n.Layout, source.Layout) || !g.sameLayout(source.Layout, replacement.Layout) {
		return invalid("jsonb store source, replacement, and result layouts differ")
	}
	for _, id := range args[1 : len(args)-1] {
		index := g.layouts[g.scalars[id-1].Layout-1]
		integer := index.typ.ID == 23 && index.typ.Storage == model.TypeInt64
		text := index.typ.ID == 25 && index.typ.Storage == model.TypeString
		if index.children.count != 0 || !integer && !text {
			return invalid("jsonb store indexes must be int4 or text")
		}
	}
	return nil
}
