package span

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (e *extractor) combine(a, b fact, op ir.BoolOp) fact {
	if !e.step() {
		return fact{}
	}
	widened := a.widened || b.widened
	if op == ir.And {
		if a.empty() || b.empty() {
			return fact{widened: widened}
		}
		if a.unrestricted {
			b.exact = a.exact && b.exact
			b.widened = widened
			return b
		}
		if b.unrestricted {
			a.exact = a.exact && b.exact
			a.widened = widened
			return a
		}
		// Drop one conjunct before multiplying. Retaining either operand covers
		// the entire intersection without allocating a Cartesian product.
		if len(a.terms) > e.limits.MaxSpans/len(b.terms) {
			if len(b.terms) < len(a.terms) {
				a = b
			}
			a.exact, a.widened = false, true
			return a
		}
		capacity := len(a.terms) * len(b.terms)
		if !e.charge(uint64(capacity), uint64(unsafe.Sizeof(term{}))) {
			return fact{}
		}
		out := fact{terms: make([]term, 0, capacity), exact: a.exact && b.exact, widened: widened}
		for _, left := range a.terms {
			for _, right := range b.terms {
				if !e.step() {
					return fact{}
				}
				merged, empty := e.intersect(left, right)
				if !empty {
					out.terms = e.unique(out.terms, merged)
				}
			}
		}
		return out
	}
	if a.empty() {
		b.widened = widened
		return b
	}
	if b.empty() {
		a.widened = widened
		return a
	}
	if a.unrestricted || b.unrestricted {
		exact := a.exact && b.exact
		return fact{unrestricted: true, exact: exact, widened: widened}
	}
	capacity := e.limits.MaxSpans
	if len(a.terms) <= e.limits.MaxSpans-len(b.terms) {
		capacity = len(a.terms) + len(b.terms)
	}
	if !e.charge(uint64(capacity), uint64(unsafe.Sizeof(term{}))) {
		return fact{}
	}
	out := fact{terms: make([]term, 0, capacity), exact: a.exact && b.exact, widened: widened}
	for _, input := range [2][]term{a.terms, b.terms} {
		for _, t := range input {
			if !e.step() {
				return fact{}
			}
			if e.contains(out.terms, t) {
				continue
			}
			if len(out.terms) == e.limits.MaxSpans {
				return fact{unrestricted: true, widened: true}
			}
			out.terms = append(out.terms, t)
		}
	}
	return out
}

func (e *extractor) intersect(a, b term) (term, bool) {
	out := e.newTerm()
	if e.err != nil {
		return nil, true
	}
	for i := range e.keys {
		if !e.step() {
			return nil, true
		}
		out[i].lower = e.tighter(i, a[i].lower, b[i].lower, true)
		out[i].upper = e.tighter(i, a[i].upper, b[i].upper, false)
		lower, upper := out[i].lower, out[i].upper
		if lower.set && upper.set {
			order := e.compare(i, lower.value, upper.value)
			excludesEqual := order == 0 && (!lower.inclusive || !upper.inclusive)
			if order > 0 || excludesEqual {
				return nil, true
			}
		}
	}
	return out, false
}

func (e *extractor) tighter(index int, a, b endpoint, lower bool) endpoint {
	if !a.set {
		return b
	}
	if !b.set {
		return a
	}
	order := e.compare(index, a.value, b.value)
	if order == 0 {
		a.inclusive = a.inclusive && b.inclusive
		return a
	}
	if (order < 0) == lower {
		return b
	}
	return a
}

func (e *extractor) unique(terms []term, candidate term) []term {
	if e.contains(terms, candidate) {
		return terms
	}
	return append(terms, candidate)
}

func (e *extractor) contains(terms []term, candidate term) bool {
	for _, t := range terms {
		if !e.step() {
			return false
		}
		same := true
		for i := range t {
			if !e.sameEndpoint(i, t[i].lower, candidate[i].lower) || !e.sameEndpoint(i, t[i].upper, candidate[i].upper) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

func (e *extractor) sameEndpoint(index int, a, b endpoint) bool {
	if !e.step() {
		return false
	}
	return a.set == b.set && (!a.set || (a.inclusive == b.inclusive && e.compare(index, a.value, b.value) == 0))
}

func (e *extractor) result(predicate ir.ScalarID, f fact) Result {
	out := Result{Widened: f.widened}
	if f.empty() {
		out.Empty = true
		return out
	}
	if !f.exact {
		out.Residual = predicate
	}
	if f.unrestricted {
		if !e.charge(1, uint64(unsafe.Sizeof(storage.KeySpan{}))) {
			return Result{}
		}
		out.Spans = []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
		return out
	}
	if !e.charge(uint64(len(f.terms)), uint64(unsafe.Sizeof(storage.KeySpan{}))) {
		return Result{}
	}
	out.Spans = make([]storage.KeySpan, 0, len(f.terms))
	for _, t := range f.terms {
		if !e.step() {
			return Result{}
		}
		span, exact := e.lowerTerm(t)
		if !exact {
			out.Residual = predicate
		}
		out.Spans = append(out.Spans, span)
	}
	out.Spans = e.union(out.Spans)
	return out
}

func (e *extractor) lowerTerm(t term) (storage.KeySpan, bool) {
	prefix := 0
	for prefix < len(t) && e.step() {
		b := t[prefix]
		if !b.lower.set || !b.upper.set || !b.lower.inclusive || !b.upper.inclusive || e.compare(prefix, b.lower.value, b.upper.value) != 0 {
			break
		}
		prefix++
	}
	var lower, upper endpoint
	if prefix < len(t) {
		lower, upper = t[prefix].lower, t[prefix].upper
	}
	out := storage.KeySpan{Lower: e.bound(t, prefix, lower), Upper: e.bound(t, prefix, upper)}
	exact := true
	for i := prefix + 1; i < len(t) && e.step(); i++ {
		if t[i].lower.set || t[i].upper.set {
			exact = false
		}
	}
	return out, exact
}

func (e *extractor) bound(t term, prefix int, end endpoint) storage.KeyBound {
	count := prefix
	if end.set {
		count++
	}
	if count == 0 {
		return storage.KeyBound{Unbounded: true}
	}
	if !e.charge(uint64(count), uint64(unsafe.Sizeof(model.Value{}))) {
		return storage.KeyBound{}
	}
	out := storage.KeyBound{Values: make([]model.Value, count), Inclusive: true}
	for i := range prefix {
		out.Values[i] = e.copyValue(t[i].lower.value)
	}
	if end.set {
		out.Values[prefix] = e.copyValue(end.value)
		out.Inclusive = end.inclusive
	}
	return out
}

func (e *extractor) copyValue(v model.Value) model.Value {
	if !e.step() || e.bytes == e.limits.MaxBytes {
		if e.err == nil {
			e.err = ErrLimit
		}
		return model.Value{}
	}
	owned, used, err := model.CopyValue(e.ctx, v, e.limits.MaxBytes-e.bytes)
	if err != nil {
		if e.ctx.Err() != nil {
			e.err = e.ctx.Err()
		} else {
			e.err = ErrLimit
		}
		return model.Value{}
	}
	e.bytes += used
	return owned
}

type boundSide uint8

const (
	lowerBound boundSide = iota
	upperBound
)

// A cut is before or after every extension of its tuple. This preserves strict
// composite endpoints without inventing a successor for the last datum.
func cutSide(bound storage.KeyBound, side boundSide) int {
	if bound.Inclusive == (side == lowerBound) {
		return -1
	}
	return 1
}

func (e *extractor) compareCut(a storage.KeyBound, aSide boundSide, b storage.KeyBound, bSide boundSide) int {
	if !e.step() {
		return 0
	}
	if a.Unbounded || b.Unbounded {
		if a.Unbounded && b.Unbounded {
			if aSide == bSide {
				return 0
			}
			if aSide == lowerBound {
				return -1
			}
			return 1
		}
		if a.Unbounded {
			if aSide == lowerBound {
				return -1
			}
			return 1
		}
		if bSide == lowerBound {
			return 1
		}
		return -1
	}
	for i := range min(len(a.Values), len(b.Values)) {
		if order := e.compare(i, a.Values[i], b.Values[i]); order != 0 {
			return order
		}
	}
	if len(a.Values) < len(b.Values) {
		return cutSide(a, aSide)
	}
	if len(a.Values) > len(b.Values) {
		return -cutSide(b, bSide)
	}
	return cutSide(a, aSide) - cutSide(b, bSide)
}

func (e *extractor) union(spans []storage.KeySpan) []storage.KeySpan {
	for i := 1; i < len(spans) && e.step(); i++ {
		value := spans[i]
		j := i
		for j > 0 && e.step() && e.compareCut(value.Lower, lowerBound, spans[j-1].Lower, lowerBound) < 0 {
			spans[j] = spans[j-1]
			j--
		}
		spans[j] = value
	}
	count := 0
	for _, s := range spans {
		if !e.step() {
			return nil
		}
		if count == 0 || e.compareCut(s.Lower, lowerBound, spans[count-1].Upper, upperBound) > 0 {
			spans[count] = s
			count++
			continue
		}
		if e.compareCut(s.Upper, upperBound, spans[count-1].Upper, upperBound) > 0 {
			spans[count-1].Upper = s.Upper
		}
	}
	clear(spans[count:])
	return spans[:count]
}
