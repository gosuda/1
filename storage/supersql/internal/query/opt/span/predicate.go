package span

import (
	"bytes"
	"cmp"
	"math"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type endpoint struct {
	value          model.Value
	set, inclusive bool
}

type interval struct{ lower, upper endpoint }
type term []interval

type fact struct {
	terms                        []term
	unrestricted, exact, widened bool
}

func full(exact bool) fact { return fact{unrestricted: true, exact: exact} }
func (f fact) empty() bool { return !f.unrestricted && len(f.terms) == 0 }

func (e *extractor) newTerm() term {
	if !e.charge(uint64(len(e.keys)), uint64(unsafe.Sizeof(interval{}))) {
		return nil
	}
	return make(term, len(e.keys))
}

func (e *extractor) singleton(index int, bounds interval) fact {
	t := e.newTerm()
	if e.err != nil {
		return fact{}
	}
	t[index] = bounds
	if !e.charge(1, uint64(unsafe.Sizeof(term{}))) {
		return fact{}
	}
	return fact{terms: []term{t}, exact: true}
}

func isBuiltinType(id model.TypeID) bool {
	_, ok := builtin.LookupType(id)
	return ok
}

func (e *extractor) resolveCollation(collation model.CollationID) model.CollationID {
	if collation == 100 && e.limits.DefaultCollation != 0 {
		return e.limits.DefaultCollation
	}
	return collation
}

func (e *extractor) resolvedBinaryCollation(collation model.CollationID) bool {
	resolved := e.resolveCollation(collation)
	return resolved == 950 || resolved == 951
}

// orderPreservingRelabel reports whether a binary relabel from inner to outer
// preserves canonical KeyEncoder ordering and equality.
func (e *extractor) orderPreservingRelabel(inner, outer model.Type) bool {
	if inner.Storage != outer.Storage {
		return false
	}
	if inner.Storage == model.TypeString {
		if e.resolveCollation(inner.Collation) != e.resolveCollation(outer.Collation) || !e.resolvedBinaryCollation(inner.Collation) {
			return false
		}
	} else if inner.Collation != 0 || outer.Collation != 0 {
		return false
	}
	if (inner.ID == 1043 && outer.ID == 25) || (inner.ID == 25 && outer.ID == 1043) {
		return true
	}
	// User-defined domains relabel transparently to their base type, and
	// KeyEncoder unwraps domains to the same base codec.
	if !isBuiltinType(inner.ID) && e.supportedType(outer) {
		return true
	}
	return false
}

func (e *extractor) key(id ir.ScalarID) (int, bool) {
	for i, key := range e.keys {
		if !e.step() {
			return 0, false
		}
		if key.Expression != 0 && key.Expression == id {
			return i, e.supportedType(key.Column.Type)
		}
	}
	n, ok := e.graph.Scalar(id)
	sawBPChar := false
	// Once around a slot is pure and preserves its value. Order-preserving
	// binary relabels (varchar <-> text, domain -> base) are unwrapped without
	// evaluating casts or function calls.
	for ok && e.step() {
		if n.Kind == ir.Once {
			id, ok = e.graph.Operand(id, 0)
			n, _ = e.graph.Scalar(id)
			continue
		}
		if n.Kind == ir.Relabel && n.OperandCount() == 1 {
			outerType, outerOK := e.graph.Type(n.Layout)
			innerID, innerOK := e.graph.Operand(id, 0)
			if !outerOK || !innerOK {
				break
			}
			inner, _ := e.graph.Scalar(innerID)
			innerType, innerTypeOK := e.graph.Type(inner.Layout)
			if !innerTypeOK || !e.orderPreservingRelabel(innerType, outerType) {
				break
			}
			if outerType.ID == 1042 {
				sawBPChar = true
			}
			id, n = innerID, inner
			continue
		}
		break
	}
	if !ok || n.Kind != ir.Slot {
		return 0, false
	}
	for i, key := range e.keys {
		if !e.step() {
			return 0, false
		}
		if key.Slot == n.Slot {
			if sawBPChar && i < len(e.keyBPChar) {
				e.keyBPChar[i] = true
			}
			return i, e.supportedType(key.Column.Type)
		}
	}
	return 0, false
}

func (e *extractor) supportedType(typ model.Type) bool {
	switch typ.ID {
	case 16:
		return typ.Storage == model.TypeBoolean && typ.Collation == 0
	case 20, 21, 23:
		return typ.Storage == model.TypeInt64 && typ.Collation == 0
	case 700, 701:
		return typ.Storage == model.TypeFloat64 && typ.Collation == 0
	case 1082, 1083, 1114, 1184:
		return typ.Storage == model.TypeTimestamp && typ.Collation == 0
	case 17:
		return typ.Storage == model.TypeBytes && typ.Collation == 0
	case 25, 1042, 1043:
		return typ.Storage == model.TypeString && e.resolvedBinaryCollation(typ.Collation)
	case 2950:
		return typ.Storage == model.TypeUUID && typ.Collation == 0
	case 1700:
		return typ.Storage == model.TypeNumeric && typ.Collation == 0
	default:
		if builtin.IsOIDType(typ.ID) {
			return typ.Storage == model.TypeInt64 && typ.Collation == 0
		}
		if !isBuiltinType(typ.ID) {
			switch typ.Storage {
			case model.TypeBoolean, model.TypeInt64, model.TypeFloat64, model.TypeTimestamp, model.TypeBytes, model.TypeUUID, model.TypeNumeric:
				return typ.Collation == 0
			case model.TypeString:
				return e.resolvedBinaryCollation(typ.Collation)
			}
		}
	}
	return false
}

func validNumber(typ model.Type, v model.Value) bool {
	if v.IsNull {
		return true
	}
	switch typ.ID {
	case 16:
		return v.IntVal == 0 || v.IntVal == 1
	case 21:
		return v.IntVal >= -32768 && v.IntVal <= 32767
	case 23:
		return v.IntVal >= -2147483648 && v.IntVal <= 2147483647
	case 700:
		return math.IsNaN(v.FloatVal) || float64(float32(v.FloatVal)) == v.FloatVal
	case 2950:
		_, ok := parseUUID(v.StrVal)
		return ok
	}
	if builtin.IsOIDType(typ.ID) {
		return v.IntVal >= 0 && v.IntVal <= math.MaxUint32
	}
	if v.Type == model.TypeUUID {
		_, ok := parseUUID(v.StrVal)
		return ok
	}
	return true
}

func validBound(typ model.Type, v model.Value) bool {
	if !validNumber(typ, v) {
		return false
	}
	if v.IsNull {
		return true
	}
	switch v.Type {
	case model.TypeNumeric:
		_, ok := parseNumericBound(v.StrVal)
		return ok
	case model.TypeString:
		return utf8.ValidString(v.StrVal) && !strings.ContainsRune(v.StrVal, 0)
	}
	return true
}

func (e *extractor) constant(id ir.ScalarID) (model.Value, model.Type, bool) {
	n, ok := e.graph.Scalar(id)
	outerType, _ := e.graph.Type(n.Layout)
	relabeled := false
	for ok && e.step() {
		if n.Kind == ir.Once {
			id, ok = e.graph.Operand(id, 0)
			n, _ = e.graph.Scalar(id)
			if !relabeled {
				outerType, _ = e.graph.Type(n.Layout)
			}
			continue
		}
		if n.Kind == ir.Relabel && n.OperandCount() == 1 {
			curType, curOK := e.graph.Type(n.Layout)
			innerID, innerOK := e.graph.Operand(id, 0)
			if !curOK || !innerOK {
				break
			}
			inner, _ := e.graph.Scalar(innerID)
			innerType, innerTypeOK := e.graph.Type(inner.Layout)
			if !innerTypeOK || !e.orderPreservingRelabel(innerType, curType) {
				break
			}
			if !relabeled {
				outerType = curType
				relabeled = true
			}
			id, n = innerID, inner
			continue
		}
		break
	}
	typ, _ := e.graph.Type(n.Layout)
	if relabeled {
		typ = outerType
	}
	if !ok || !e.supportedType(typ) {
		return model.Value{}, typ, false
	}
	if n.Kind == ir.Param {
		for _, p := range e.parameters {
			if !e.step() {
				return model.Value{}, typ, false
			}
			if p.Number == n.Parameter {
				return p.Value, typ, true
			}
		}
	}
	if n.Kind != ir.Const {
		return model.Value{}, typ, false
	}
	if !e.charge(1, uint64(unsafe.Sizeof(model.Value{}))) || e.bytes == e.limits.MaxBytes {
		if e.err == nil {
			e.err = ErrLimit
		}
		return model.Value{}, typ, false
	}
	value, err := e.graph.CloneConstant(e.ctx, id, e.limits.MaxBytes-e.bytes)
	if err != nil {
		if e.ctx.Err() != nil {
			e.err = e.ctx.Err()
		} else {
			e.err = ErrLimit
		}
		return model.Value{}, typ, false
	}
	if !e.accountCopied(value) {
		return model.Value{}, typ, false
	}
	return value, typ, true
}

func (e *extractor) accountCopied(v model.Value) bool {
	for _, allocation := range [...]struct{ count, size uint64 }{
		{uint64(len(v.StrVal)), 1}, {uint64(len(v.BytesVal)), 1},
		{uint64(len(v.VectorVal)), 4},
		{uint64(len(v.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{}))},
		{uint64(len(v.ArrayVal)), uint64(unsafe.Sizeof(model.Value{}))},
	} {
		if !e.charge(allocation.count, allocation.size) {
			return false
		}
	}
	for _, child := range v.ArrayVal {
		if !e.accountCopied(child) {
			return false
		}
	}
	return true
}

type comparison uint8

const (
	unknown comparison = iota
	equal
	less
	lessEqual
	greater
	greaterEqual
)

// These are the exact primitive function identities implemented by builtin.
// Operator OIDs resolve through the same immutable inventory as scalar execution.
func comparisonKind(id model.FunctionID) comparison {
	switch id {
	case 60, 63, 65, 67, 158, 159, 184, 287, 293, 299, 305, 467, 474, 852, 1048, 1086, 1145, 1152, 1718, 1850, 1856, 1948, 2052, 2956, 3508:
		return equal
	case 56, 64, 66, 160, 161, 289, 295, 301, 307, 469, 476, 716, 740, 854, 1049, 1087, 1102, 1154, 1722, 1852, 1858, 1949, 2054, 2160, 2174, 2954:
		return less
	case 148, 149, 166, 167, 290, 296, 302, 308, 471, 478, 717, 741, 856, 1050, 1088, 1103, 1155, 1691, 1723, 1854, 1860, 1950, 2055, 2161, 2175, 2955:
		return lessEqual
	case 57, 146, 147, 162, 163, 291, 297, 303, 309, 470, 477, 742, 855, 1051, 1089, 1104, 1157, 1638, 1720, 1853, 1859, 1951, 2057, 2164, 2178, 2958:
		return greater
	case 150, 151, 168, 169, 292, 298, 304, 310, 472, 479, 743, 857, 1052, 1090, 1105, 1156, 1639, 1692, 1721, 1855, 1861, 1952, 2056, 2163, 2177, 2957:
		return greaterEqual
	}
	return unknown
}

func reverse(op comparison) comparison {
	switch op {
	case less:
		return greater
	case lessEqual:
		return greaterEqual
	case greater:
		return less
	case greaterEqual:
		return lessEqual
	}
	return op
}

func isLikeFunction(id model.FunctionID) bool {
	return id == 850 || id == 1569 || id == 1631
}

func (e *extractor) signatureBinaryCollationOK(sigID ir.SignatureID, argType model.Type) bool {
	if argType.Storage != model.TypeString {
		return true
	}
	if !e.resolvedBinaryCollation(argType.Collation) {
		return false
	}
	if sigID == 0 {
		return true
	}
	sig, ok := e.graph.Signature(sigID)
	if !ok {
		return false
	}
	return sig.InputCollation == 0 || e.resolvedBinaryCollation(sig.InputCollation)
}

func signatureArgMatches(argType model.Type, sigArg model.TypeID) bool {
	if argType.ID == sigArg {
		return true
	}
	// Polymorphic anyenum (3500) matches user-defined enum types.
	if sigArg == 3500 && argType.Storage == model.TypeInt64 && argType.Collation == 0 && !isBuiltinType(argType.ID) {
		return true
	}
	return false
}

func (e *extractor) operation(id ir.ScalarID, n ir.Scalar) (comparison, model.FunctionID) {
	if n.Kind != ir.Call || n.OperandCount() != 2 || !n.Strict || n.Effects.Volatility != ir.Immutable {
		return unknown, 0
	}
	function := n.Function
	if n.Operator != 0 {
		op, ok := builtin.LookupOperator(n.Operator)
		if !ok {
			return unknown, 0
		}
		function = op.Function
	}
	sig, ok := builtin.LookupSignature(function)
	if !ok || !sig.Strict || sig.Volatility != 'i' || sig.ArgumentCount != 2 || sig.Result != 16 {
		return unknown, 0
	}
	var types [2]model.Type
	for i := range uint32(2) {
		arg, _ := e.graph.Operand(id, i)
		operand, _ := e.graph.Scalar(arg)
		typ, _ := e.graph.Type(operand.Layout)
		if !signatureArgMatches(typ, sig.Arguments[i]) {
			return unknown, 0
		}
		types[i] = typ
	}
	if sig.Arguments[0] == 3500 && types[0] != types[1] {
		return unknown, 0
	}
	if !e.signatureBinaryCollationOK(n.Signature, types[0]) || !e.signatureBinaryCollationOK(n.Signature, types[1]) {
		return unknown, 0
	}
	return comparisonKind(function), function
}

func (e *extractor) atom(id ir.ScalarID, n ir.Scalar) fact {
	if n.Kind == ir.Const || n.Kind == ir.Param {
		v, typ, ok := e.constant(id)
		if ok && typ.ID == 16 {
			if v.IsNull || v.IntVal == 0 {
				return fact{}
			}
			return full(true)
		}
	}
	if n.Kind == ir.NullTest && n.OperandCount() == 1 {
		arg, _ := e.graph.Operand(id, 0)
		index, ok := e.key(arg)
		if !ok {
			return full(false)
		}
		null := endpoint{value: model.NullVal(e.keys[index].Column.Type.Storage), set: true, inclusive: true}
		if !n.Negated {
			return e.singleton(index, interval{lower: null, upper: null})
		}
		return e.singleton(index, e.nonNull(index))
	}
	if n.Kind == ir.ArrayQuantified {
		return e.atomArrayQuantified(id, n)
	}
	op, function := e.operation(id, n)
	if op == unknown {
		return e.atomLike(id, n)
	}
	left, _ := e.graph.Operand(id, 0)
	right, _ := e.graph.Operand(id, 1)
	index, ok := e.key(left)
	if !ok {
		index, ok = e.key(right)
		if !ok {
			return full(false)
		}
		left, right = right, left
		op = reverse(op)
	}
	if function == 3508 && index < len(e.keyEnum) {
		e.keyEnum[index] = true
	}
	value, typ, ok := e.constant(right)
	targetType := e.keys[index].Column.Type
	if !ok || !e.compatibleBoundType(typ, targetType, value) || !validBound(targetType, value) {
		return full(false)
	}
	if value.IsNull {
		return fact{}
	}
	bound := endpoint{value: value, set: true, inclusive: op == equal || op == lessEqual || op == greaterEqual}
	if op == equal {
		return e.singleton(index, interval{lower: bound, upper: bound})
	}
	if e.keys[index].Column.Descending {
		op = reverse(op)
	}
	bounds := e.nonNull(index)
	if op == less || op == lessEqual {
		bounds.upper = bound
	} else {
		bounds.lower = bound
	}
	return e.singleton(index, bounds)
}

func (e *extractor) atomLike(id ir.ScalarID, n ir.Scalar) fact {
	if n.Kind != ir.Call || n.OperandCount() != 2 || !n.Strict || n.Effects.Volatility != ir.Immutable {
		return full(false)
	}
	function := n.Function
	if n.Operator != 0 {
		op, ok := builtin.LookupOperator(n.Operator)
		if !ok {
			return full(false)
		}
		function = op.Function
	}
	if !isLikeFunction(function) {
		return full(false)
	}
	sig, ok := builtin.LookupSignature(function)
	if !ok || !sig.Strict || sig.Volatility != 'i' || sig.ArgumentCount != 2 || sig.Result != 16 {
		return full(false)
	}
	left, _ := e.graph.Operand(id, 0)
	right, _ := e.graph.Operand(id, 1)
	leftNode, _ := e.graph.Scalar(left)
	rightNode, _ := e.graph.Scalar(right)
	leftType, _ := e.graph.Type(leftNode.Layout)
	rightType, _ := e.graph.Type(rightNode.Layout)
	if leftType.ID != sig.Arguments[0] || rightType.ID != sig.Arguments[1] {
		return full(false)
	}
	if !e.signatureBinaryCollationOK(n.Signature, leftType) || !e.signatureBinaryCollationOK(n.Signature, rightType) {
		return full(false)
	}
	index, ok := e.key(left)
	if !ok {
		return full(false)
	}
	targetType := e.keys[index].Column.Type
	if targetType.Storage != model.TypeString || !e.resolvedBinaryCollation(targetType.Collation) {
		return full(false)
	}
	pattern, patType, ok := e.constant(right)
	if !ok || patType.Storage != model.TypeString || !e.resolvedBinaryCollation(patType.Collation) {
		return full(false)
	}
	if pattern.IsNull {
		return fact{}
	}
	if !utf8.ValidString(pattern.StrVal) || strings.ContainsRune(pattern.StrVal, 0) {
		return full(false)
	}
	prefix, hasWildcard, onlyTrailingPercent, valid := e.likePrefix(pattern.StrVal)
	if !valid {
		return full(false)
	}
	isBPChar := index < len(e.keyBPChar) && e.keyBPChar[index]
	if !hasWildcard {
		bound := endpoint{value: model.StringVal(prefix), set: true, inclusive: true}
		f := e.singleton(index, interval{lower: bound, upper: bound})
		if isBPChar {
			f.exact = false
		}
		return f
	}
	if prefix == "" {
		return full(false)
	}
	upper, hasUpper := nextUTF8Prefix(prefix)
	lowerBound := endpoint{value: model.StringVal(prefix), set: true, inclusive: true}
	bounds := e.nonNull(index)
	if e.keys[index].Column.Descending {
		bounds.upper = lowerBound
		if hasUpper {
			bounds.lower = endpoint{value: model.StringVal(upper), set: true, inclusive: false}
		}
	} else {
		bounds.lower = lowerBound
		if hasUpper {
			bounds.upper = endpoint{value: model.StringVal(upper), set: true, inclusive: false}
		}
	}
	f := e.singleton(index, bounds)
	if !onlyTrailingPercent || isBPChar {
		f.exact = false
	}
	return f
}

func (e *extractor) likePrefix(pattern string) (prefix string, hasWildcard, onlyTrailingPercent, valid bool) {
	var out strings.Builder
	i := 0
	for i < len(pattern) {
		if i%4096 == 0 && !e.step() {
			return "", false, false, false
		}
		c := pattern[i]
		if c == '%' || c == '_' {
			hasWildcard = true
			break
		}
		if c == '\\' {
			i++
			if i == len(pattern) {
				// Trailing escape raises 22025 at runtime; never drop residual.
				return "", false, false, false
			}
			c = pattern[i]
		}
		out.WriteByte(c)
		i++
	}
	if !hasWildcard {
		return out.String(), false, false, true
	}
	onlyTrailingPercent = true
	for ; i < len(pattern); i++ {
		if i%4096 == 0 && !e.step() {
			return "", false, false, false
		}
		if pattern[i] == '\\' {
			onlyTrailingPercent = false
			i++
			if i == len(pattern) {
				return "", false, false, false
			}
			continue
		}
		if pattern[i] != '%' {
			onlyTrailingPercent = false
		}
	}
	return out.String(), true, onlyTrailingPercent, true
}

// nextUTF8Prefix returns the smallest valid non-NUL UTF-8 string strictly
// greater than every valid UTF-8 string starting with prefix.
func nextUTF8Prefix(prefix string) (string, bool) {
	for end := len(prefix); end > 0; {
		r, size := utf8.DecodeLastRuneInString(prefix[:end])
		if r == utf8.RuneError && size == 1 {
			return "", false
		}
		if r < utf8.MaxRune {
			next := r + 1
			if next == 0xD800 {
				next = 0xE000
			}
			return prefix[:end-size] + string(next), true
		}
		end -= size
	}
	return "", false
}

func (e *extractor) atomArrayQuantified(id ir.ScalarID, n ir.Scalar) fact {
	if n.SubqueryKind != ir.AnySubquery || n.OperandCount() != 2 || !n.Strict || n.Effects.Volatility != ir.Immutable || n.Operator == 0 {
		return full(false)
	}
	op, ok := builtin.LookupOperator(n.Operator)
	if !ok {
		return full(false)
	}
	if comparisonKind(op.Function) != equal {
		return full(false)
	}
	sig, ok := builtin.LookupSignature(op.Function)
	if !ok || !sig.Strict || sig.Volatility != 'i' || sig.ArgumentCount != 2 || sig.Result != 16 {
		return full(false)
	}
	left, _ := e.graph.Operand(id, 0)
	right, _ := e.graph.Operand(id, 1)
	leftNode, _ := e.graph.Scalar(left)
	rightNode, _ := e.graph.Scalar(right)
	leftType, _ := e.graph.Type(leftNode.Layout)
	if e.graph.LayoutChildCount(rightNode.Layout) != 1 {
		return full(false)
	}
	elemLayout, _ := e.graph.LayoutChild(rightNode.Layout, 0)
	elemType, elemOK := e.graph.Type(elemLayout)
	if !elemOK || !signatureArgMatches(leftType, sig.Arguments[0]) || !signatureArgMatches(elemType, sig.Arguments[1]) {
		return full(false)
	}
	if sig.Arguments[0] == 3500 && leftType != elemType {
		return full(false)
	}
	if !e.signatureBinaryCollationOK(n.Signature, leftType) || !e.signatureBinaryCollationOK(n.Signature, elemType) {
		return full(false)
	}
	index, ok := e.key(left)
	if !ok {
		return full(false)
	}
	if op.Function == 3508 && index < len(e.keyEnum) {
		e.keyEnum[index] = true
	}
	targetType := e.keys[index].Column.Type
	for rightNode.Kind == ir.Once && e.step() {
		right, ok = e.graph.Operand(right, 0)
		if !ok {
			return full(false)
		}
		rightNode, _ = e.graph.Scalar(right)
	}
	switch rightNode.Kind {
	case ir.Param, ir.Const:
		arrayVal, ok := e.arrayConstant(right, rightNode)
		if !ok {
			return full(false)
		}
		if arrayVal.IsNull || len(arrayVal.ArrayVal) == 0 {
			return fact{}
		}
		var out fact
		for _, elem := range arrayVal.ArrayVal {
			if !e.step() {
				return fact{}
			}
			if elem.IsNull {
				continue
			}
			if !e.compatibleBoundType(elemType, targetType, elem) || !validBound(targetType, elem) {
				return full(false)
			}
			bound := endpoint{value: elem, set: true, inclusive: true}
			out = e.combine(out, e.singleton(index, interval{lower: bound, upper: bound}), ir.Or)
			if e.err != nil || out.unrestricted {
				return out
			}
		}
		return out
	case ir.Array:
		if rightNode.OperandCount() == 0 {
			return fact{}
		}
		var out fact
		for i := range rightNode.OperandCount() {
			if !e.step() {
				return fact{}
			}
			elemID, _ := e.graph.Operand(right, i)
			elem, typ, ok := e.constant(elemID)
			if !ok {
				return full(false)
			}
			if elem.IsNull {
				continue
			}
			if !e.compatibleBoundType(typ, targetType, elem) || !validBound(targetType, elem) {
				return full(false)
			}
			bound := endpoint{value: elem, set: true, inclusive: true}
			out = e.combine(out, e.singleton(index, interval{lower: bound, upper: bound}), ir.Or)
			if e.err != nil || out.unrestricted {
				return out
			}
		}
		return out
	default:
		return full(false)
	}
}

func (e *extractor) arrayConstant(id ir.ScalarID, n ir.Scalar) (model.Value, bool) {
	if n.Kind == ir.Param {
		for _, p := range e.parameters {
			if !e.step() {
				return model.Value{}, false
			}
			if p.Number == n.Parameter {
				return p.Value, true
			}
		}
		return model.Value{}, false
	}
	if n.Kind != ir.Const {
		return model.Value{}, false
	}
	if !e.charge(1, uint64(unsafe.Sizeof(model.Value{}))) || e.bytes == e.limits.MaxBytes {
		if e.err == nil {
			e.err = ErrLimit
		}
		return model.Value{}, false
	}
	value, err := e.graph.CloneConstant(e.ctx, id, e.limits.MaxBytes-e.bytes)
	if err != nil {
		if e.ctx.Err() != nil {
			e.err = e.ctx.Err()
		} else {
			e.err = ErrLimit
		}
		return model.Value{}, false
	}
	if !e.accountCopied(value) {
		return model.Value{}, false
	}
	return value, true
}

func (e *extractor) compatibleBoundType(source, target model.Type, value model.Value) bool {
	if source == target {
		return true
	}
	if source.ID == target.ID && (source.ID == 1042 || source.ID == 1043 || source.ID == 1700) {
		return e.supportedType(source) && e.supportedType(target) && e.resolveCollation(source.Collation) == e.resolveCollation(target.Collation)
	}
	if (source.ID == 25 && target.ID == 1043) || (source.ID == 1043 && target.ID == 25) {
		return e.supportedType(source) && e.supportedType(target) && e.resolveCollation(source.Collation) == e.resolveCollation(target.Collation)
	}
	temporalType := source.ID == 1083 || source.ID == 1114 || source.ID == 1184
	if temporalType && source.ID == target.ID {
		return e.supportedType(source) && e.supportedType(target)
	}
	integer := func(id model.TypeID) bool { return id == 20 || id == 21 || id == 23 }
	if integer(source.ID) && integer(target.ID) {
		return source.Storage == model.TypeInt64 && target.Storage == model.TypeInt64 && validNumber(target, value)
	}
	if !isBuiltinType(target.ID) || !isBuiltinType(source.ID) {
		if source.Storage != target.Storage || !e.supportedType(source) || !e.supportedType(target) {
			return false
		}
		if source.Storage == model.TypeString && e.resolveCollation(source.Collation) != e.resolveCollation(target.Collation) {
			return false
		}
		return validNumber(target, value)
	}
	return false
}

func (e *extractor) nonNull(index int) interval {
	bound := endpoint{value: model.NullVal(e.keys[index].Column.Type.Storage), set: true}
	if e.keys[index].Column.NullsFirst {
		return interval{lower: bound}
	}
	return interval{upper: bound}
}

func (e *extractor) compare(index int, a, b model.Value) int {
	if !e.step() {
		return 0
	}
	column := e.keys[index].Column
	if a.IsNull || b.IsNull {
		if a.IsNull == b.IsNull {
			return 0
		}
		if a.IsNull == column.NullsFirst {
			return -1
		}
		return 1
	}
	order := 0
	switch column.Type.Storage {
	case model.TypeInt64, model.TypeBoolean, model.TypeTimestamp:
		order = cmp.Compare(a.IntVal, b.IntVal)
	case model.TypeFloat64:
		if math.IsNaN(a.FloatVal) {
			if !math.IsNaN(b.FloatVal) {
				order = 1
			}
		} else if math.IsNaN(b.FloatVal) {
			order = -1
		} else {
			order = cmp.Compare(a.FloatVal, b.FloatVal)
		}
	case model.TypeString:
		left, right := a.StrVal, b.StrVal
		if column.Type.ID == 1042 || (index < len(e.keyBPChar) && e.keyBPChar[index]) {
			left = strings.TrimRight(left, " ")
			right = strings.TrimRight(right, " ")
		}
		order = e.compareText(left, right)
	case model.TypeUUID:
		left, _ := parseUUID(a.StrVal)
		right, _ := parseUUID(b.StrVal)
		order = bytes.Compare(left[:], right[:])
	case model.TypeNumeric:
		order = e.compareNumeric(a.StrVal, b.StrVal)
	case model.TypeBytes:
		limit := min(len(a.BytesVal), len(b.BytesVal))
		for i := range limit {
			if i%4096 == 0 && !e.step() {
				return 0
			}
			if a.BytesVal[i] != b.BytesVal[i] {
				order = cmp.Compare(a.BytesVal[i], b.BytesVal[i])
				break
			}
		}
		order = cmp.Or(order, cmp.Compare(len(a.BytesVal), len(b.BytesVal)))
	}
	if column.Descending {
		return -order
	}
	return order
}

func (e *extractor) compareText(a, b string) int {
	limit := min(len(a), len(b))
	for start := 0; start < limit; start += 4096 {
		if !e.step() {
			return 0
		}
		end := min(start+4096, limit)
		if order := strings.Compare(a[start:end], b[start:end]); order != 0 {
			return order
		}
	}
	return cmp.Compare(len(a), len(b))
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}

// parseUUID mirrors builtin.parseUUIDText for canonical 16-byte big-endian UUID order.
func parseUUID(text string) ([16]byte, bool) {
	var data [16]byte
	if len(text) < 32 || len(text) > 41 {
		return data, false
	}
	pos := 0
	braces := text[0] == '{'
	if braces {
		pos++
	}
	for i := range data {
		if pos+1 >= len(text) {
			return [16]byte{}, false
		}
		hi, lo := hexDigit(text[pos]), hexDigit(text[pos+1])
		if hi < 0 || lo < 0 {
			return [16]byte{}, false
		}
		data[i] = byte(hi<<4 | lo)
		pos += 2
		if i%2 == 1 && i < len(data)-1 && pos < len(text) && text[pos] == '-' {
			pos++
		}
	}
	if braces {
		if pos == len(text) || text[pos] != '}' {
			return [16]byte{}, false
		}
		pos++
	}
	return data, pos == len(text)
}

type numericCategory uint8

const (
	numNegInf numericCategory = iota
	numNeg
	numZero
	numPos
	numPosInf
	numNaN
)

type numericBound struct {
	category numericCategory
	exponent int64
	raw      string
	first    int
	last     int
}

func trimASCII(s string) string {
	start, end := 0, len(s)
	for start < end && isASCIISpace(s[start]) {
		start++
	}
	for end > start && isASCIISpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// parseNumericBound validates decimal NUMERIC text and extracts its normalized
// category, decimal exponent, and significant digit slice bounds matching
// builtin/numeric_key.go ordering.
func parseNumericBound(text string) (numericBound, bool) {
	const (
		numericMaxIntegerDigits = 131072
		numericMaxScale         = 16383
		numericWorkDigits       = 1 << 20
	)
	input := trimASCII(text)
	if len(input) == 0 {
		return numericBound{}, false
	}
	if strings.EqualFold(input, "nan") {
		return numericBound{category: numNaN}, true
	}
	negative := false
	if input[0] == '+' || input[0] == '-' {
		negative = input[0] == '-'
		input = input[1:]
	}
	if strings.EqualFold(input, "inf") || strings.EqualFold(input, "infinity") {
		if negative {
			return numericBound{category: numNegInf}, true
		}
		return numericBound{category: numPosInf}, true
	}
	if len(input) == 0 {
		return numericBound{}, false
	}
	if len(input) >= 2 && input[0] == '0' && (input[1] == 'x' || input[1] == 'X' || input[1] == 'o' || input[1] == 'O' || input[1] == 'b' || input[1] == 'B') {
		return numericBound{}, false
	}
	count, significant, scale := 0, 0, 0
	first, last := -1, -1
	point, seen := false, false
	i := 0
	for ; i < len(input); i++ {
		c := input[i]
		if c >= '0' && c <= '9' {
			if count == numericWorkDigits {
				return numericBound{}, false
			}
			count++
			if c != '0' {
				if first < 0 {
					first = i
				}
				last = i
				significant++
			} else if first >= 0 {
				significant++
			}
			seen = true
			if point {
				scale++
			}
			continue
		}
		if c == '.' && !point {
			point = true
			if i+1 < len(input) && input[i+1] == '_' {
				return numericBound{}, false
			}
			continue
		}
		if c == '_' && i > 0 && i+1 < len(input) && input[i-1] >= '0' && input[i-1] <= '9' && input[i+1] >= '0' && input[i+1] <= '9' {
			continue
		}
		break
	}
	if !seen {
		return numericBound{}, false
	}
	exp := 0
	if i < len(input) && (input[i] == 'e' || input[i] == 'E') {
		i++
		expNeg := false
		if i < len(input) && (input[i] == '+' || input[i] == '-') {
			expNeg = input[i] == '-'
			i++
		}
		start := i
		for ; i < len(input); i++ {
			c := input[i]
			if c == '_' && i > start && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9' {
				continue
			}
			if c < '0' || c > '9' {
				return numericBound{}, false
			}
			exp = exp*10 + int(c-'0')
			if exp > 1073741823 {
				return numericBound{}, false
			}
		}
		if i == start {
			return numericBound{}, false
		}
		if expNeg {
			exp = -exp
		}
	}
	if i != len(input) {
		return numericBound{}, false
	}
	scale -= exp
	if scale > numericMaxScale {
		return numericBound{}, false
	}
	if first < 0 {
		return numericBound{category: numZero}, true
	}
	decimalExp := int64(significant) - int64(scale)
	if decimalExp > numericMaxIntegerDigits {
		return numericBound{}, false
	}
	cat := numPos
	if negative {
		cat = numNeg
	}
	return numericBound{category: cat, exponent: decimalExp, raw: input, first: first, last: last}, true
}

func (e *extractor) compareNumeric(a, b string) int {
	left, _ := parseNumericBound(a)
	right, _ := parseNumericBound(b)
	if left.category != right.category {
		return cmp.Compare(left.category, right.category)
	}
	if left.category != numNeg && left.category != numPos {
		return 0
	}
	order := cmp.Compare(left.exponent, right.exponent)
	if order == 0 {
		li, ri := left.first, right.first
		steps := 0
		for li <= left.last && ri <= right.last {
			if steps%4096 == 0 && !e.step() {
				return 0
			}
			steps++
			for li <= left.last && (left.raw[li] == '.' || left.raw[li] == '_') {
				li++
			}
			for ri <= right.last && (right.raw[ri] == '.' || right.raw[ri] == '_') {
				ri++
			}
			if li > left.last || ri > right.last {
				break
			}
			if left.raw[li] != right.raw[ri] {
				order = cmp.Compare(left.raw[li], right.raw[ri])
				break
			}
			li++
			ri++
		}
		if order == 0 {
			for li <= left.last && (left.raw[li] == '.' || left.raw[li] == '_') {
				li++
			}
			for ri <= right.last && (right.raw[ri] == '.' || right.raw[ri] == '_') {
				ri++
			}
			if li <= left.last {
				order = 1
			} else if ri <= right.last {
				order = -1
			}
		}
	}
	if left.category == numNeg {
		return -order
	}
	return order
}
