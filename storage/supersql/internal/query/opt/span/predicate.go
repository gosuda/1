package span

import (
	"cmp"
	"math"
	"strings"
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

func (e *extractor) key(id ir.ScalarID) (int, bool) {
	for i, key := range e.keys {
		if !e.step() {
			return 0, false
		}
		if key.Expression != 0 && key.Expression == id {
			return i, supportedType(key.Column.Type)
		}
	}
	n, ok := e.graph.Scalar(id)
	// Once around a slot is pure and preserves its value. Calls and casts are
	// deliberately not unwrapped, even when catalog metadata says immutable.
	for ok && n.Kind == ir.Once && e.step() {
		id, ok = e.graph.Operand(id, 0)
		n, _ = e.graph.Scalar(id)
	}
	if !ok || n.Kind != ir.Slot {
		return 0, false
	}
	for i, key := range e.keys {
		if !e.step() {
			return 0, false
		}
		if key.Slot == n.Slot {
			return i, supportedType(key.Column.Type)
		}
	}
	return 0, false
}

func supportedType(typ model.Type) bool {
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
	case 25:
		return typ.Storage == model.TypeString && (typ.Collation == 950 || typ.Collation == 951)
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
	}
	return true
}

func (e *extractor) constant(id ir.ScalarID) (model.Value, model.Type, bool) {
	n, ok := e.graph.Scalar(id)
	for ok && n.Kind == ir.Once && e.step() {
		id, ok = e.graph.Operand(id, 0)
		n, _ = e.graph.Scalar(id)
	}
	typ, _ := e.graph.Type(n.Layout)
	if !ok || !supportedType(typ) {
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
	case 60, 63, 65, 67, 158, 159, 287, 293, 299, 305, 467, 474, 852, 1086, 1145, 1152, 1850, 1856, 1948, 2052:
		return equal
	case 56, 64, 66, 160, 161, 289, 295, 301, 307, 469, 476, 740, 854, 1087, 1102, 1154, 1852, 1858, 1949, 2054:
		return less
	case 148, 149, 166, 167, 290, 296, 302, 308, 471, 478, 741, 856, 1088, 1103, 1155, 1691, 1854, 1860, 1950, 2055:
		return lessEqual
	case 57, 146, 147, 162, 163, 291, 297, 303, 309, 470, 477, 742, 855, 1089, 1104, 1157, 1853, 1859, 1951, 2057:
		return greater
	case 150, 151, 168, 169, 292, 298, 304, 310, 472, 479, 743, 857, 1090, 1105, 1156, 1692, 1855, 1861, 1952, 2056:
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

func (e *extractor) operation(id ir.ScalarID, n ir.Scalar) comparison {
	if n.Kind != ir.Call || n.OperandCount() != 2 || !n.Strict || n.Effects.Volatility != ir.Immutable {
		return unknown
	}
	function := n.Function
	if n.Operator != 0 {
		op, ok := builtin.LookupOperator(n.Operator)
		if !ok {
			return unknown
		}
		function = op.Function
	}
	sig, ok := builtin.LookupSignature(function)
	if !ok || !sig.Strict || sig.Volatility != 'i' || sig.ArgumentCount != 2 || sig.Result != 16 {
		return unknown
	}
	for i := range uint32(2) {
		arg, _ := e.graph.Operand(id, i)
		operand, _ := e.graph.Scalar(arg)
		typ, _ := e.graph.Type(operand.Layout)
		if typ.ID != sig.Arguments[i] {
			return unknown
		}
	}
	return comparisonKind(function)
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
	op := e.operation(id, n)
	if op == unknown {
		return full(false)
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
	value, typ, ok := e.constant(right)
	targetType := e.keys[index].Column.Type
	if !ok || !compatibleBoundType(typ, targetType, value) || !validNumber(targetType, value) {
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

func compatibleBoundType(source, target model.Type, value model.Value) bool {
	if source == target {
		return true
	}
	temporalType := source.ID == 1083 || source.ID == 1114 || source.ID == 1184
	if temporalType && source.ID == target.ID {
		return supportedType(source) && supportedType(target)
	}
	integer := func(id model.TypeID) bool { return id == 20 || id == 21 || id == 23 }
	if !integer(source.ID) || !integer(target.ID) {
		return false
	}
	return source.Storage == model.TypeInt64 && target.Storage == model.TypeInt64 && validNumber(target, value)
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
		order = e.compareText(a.StrVal, b.StrVal)
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
