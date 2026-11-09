// Package span derives conservative typed index bounds without evaluating routines.
package span

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var (
	ErrInvalid = errors.New("invalid span extraction input")
	ErrLimit   = errors.New("span extraction resource limit exceeded")
)

type Key struct {
	Slot       model.SlotID
	Expression ir.ScalarID
	Column     storage.KeyColumn
}

type Parameter struct {
	Number uint32
	Layout model.TypeLayout
	Value  model.Value
}

// MaxBytes accounts cumulative allocation, including retired intermediate slices.
// MaxNodes bounds the graph inventory; MaxWork bounds visits and comparisons.
// Callers normally supply MaxSpans=1024. Every limit must be positive.
type Limits struct {
	MaxSpans int
	MaxNodes uint32
	MaxWork  uint64
	MaxBytes uint64
}

// Empty is a proof, whereas one fully unbounded span means no useful restriction.
// Residual is the original predicate whenever the spans are not an exact filter.
// Spans and all their mutable payloads belong to the caller.
// Widened records a MaxSpans fallback; unsupported shapes use Residual alone.
type Result struct {
	Spans    []storage.KeySpan
	Empty    bool
	Residual ir.ScalarID
	Widened  bool
	// UsedBytes includes retired intermediate allocations; UsedWork counts visits.
	UsedBytes, UsedWork uint64
}

type extractor struct {
	ctx               context.Context
	graph             *ir.Graph
	keys              []Key
	parameters        []Parameter
	limits            Limits
	work, bytes       uint64
	err               error
	requireParameters bool
}

func (e *extractor) step() bool {
	if err := e.ctx.Err(); err != nil {
		e.err = err
		return false
	}
	if e.err != nil {
		return false
	}
	if e.work == e.limits.MaxWork {
		e.err = ErrLimit
		return false
	}
	e.work++
	return true
}

func (e *extractor) charge(count, size uint64) bool {
	if !e.step() {
		return false
	}
	if size != 0 && count > (e.limits.MaxBytes-e.bytes)/size {
		e.err = ErrLimit
		return false
	}
	e.bytes += count * size
	return true
}

// Extract borrows all inputs for this call. Invalid parameters and resource
// exhaustion are errors, not evidence that a predicate cannot match.
func Extract(ctx context.Context, graph *ir.Graph, predicate ir.ScalarID, keys []Key, parameters []Parameter, limits Limits) (Result, error) {
	return extract(ctx, graph, predicate, keys, parameters, limits, true)
}

// ExtractStatic derives only parameter-independent bounds. Unbound parameter
// expressions remain residual predicates; they are never treated as NULL.
func ExtractStatic(ctx context.Context, graph *ir.Graph, predicate ir.ScalarID, keys []Key, limits Limits) (Result, error) {
	return extract(ctx, graph, predicate, keys, nil, limits, false)
}

func extract(ctx context.Context, graph *ir.Graph, predicate ir.ScalarID, keys []Key, parameters []Parameter, limits Limits, requireParameters bool) (Result, error) {
	if ctx == nil {
		return Result{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if graph == nil || limits.MaxSpans <= 0 || limits.MaxNodes == 0 || limits.MaxWork == 0 || limits.MaxBytes == 0 {
		return Result{}, ErrInvalid
	}
	n, ok := graph.Scalar(predicate)
	typ, _ := graph.Type(n.Layout)
	if !ok || typ.ID != 16 {
		return Result{}, ErrInvalid
	}
	if graph.ScalarCount() > limits.MaxNodes {
		return Result{}, ErrLimit
	}
	e := extractor{ctx: ctx, graph: graph, keys: keys, parameters: parameters, limits: limits, requireParameters: requireParameters}
	if !e.validate() {
		return Result{}, e.err
	}
	count := uint64(graph.ScalarCount()) + 1
	if !e.charge(count, uint64(unsafe.Sizeof(fact{}))+1) {
		return Result{}, e.err
	}
	facts := make([]fact, int(count))
	reachable := make([]bool, int(count))
	reachable[predicate] = true
	// IR operands precede their users; reverse marking and forward evaluation
	// visit shared Boolean DAG nodes once without recursive stack growth.
	for id := predicate; id > 0 && e.step(); id-- {
		if !reachable[id] {
			continue
		}
		n, _ := graph.Scalar(id)
		if n.Kind != ir.Bool || (n.BoolOp != ir.And && n.BoolOp != ir.Or) {
			continue
		}
		for i := uint32(0); i < n.OperandCount() && e.step(); i++ {
			child, _ := graph.Operand(id, i)
			if child >= id || child == 0 {
				e.err = ErrInvalid
				break
			}
			reachable[child] = true
		}
	}
	for id := ir.ScalarID(1); id <= predicate && e.step(); id++ {
		if !reachable[id] {
			continue
		}
		n, _ := graph.Scalar(id)
		if n.Kind != ir.Bool || (n.BoolOp != ir.And && n.BoolOp != ir.Or) {
			facts[id] = e.atom(id, n)
			continue
		}
		var f fact
		if n.BoolOp == ir.And {
			f = full(true)
		}
		for i := uint32(0); i < n.OperandCount() && e.step(); i++ {
			child, _ := graph.Operand(id, i)
			f = e.combine(f, facts[child], n.BoolOp)
		}
		facts[id] = f
	}
	if e.err != nil {
		return Result{}, e.err
	}
	result := e.result(predicate, facts[predicate])
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if e.err != nil {
		return Result{}, e.err
	}
	result.UsedBytes, result.UsedWork = e.bytes, e.work
	return result, nil
}

func (e *extractor) invalid(message string) bool {
	if e.err != nil {
		return false
	}
	e.err = fmt.Errorf("%w: %s", ErrInvalid, message)
	return false
}

func (e *extractor) validate() bool {
	for i, key := range e.keys {
		if !e.step() {
			return false
		}
		var layout ir.LayoutID
		if (key.Slot == 0) == (key.Expression == 0) {
			return e.invalid("key must name one slot or expression")
		}
		if key.Slot != 0 {
			slot, ok := e.graph.Slot(key.Slot)
			if !ok {
				return e.invalid("key differs from graph slot")
			}
			layout = slot.Layout
		} else {
			scalar, ok := e.graph.Scalar(key.Expression)
			if !ok {
				return e.invalid("key differs from graph expression")
			}
			layout = scalar.Layout
		}
		if key.Column.ColumnID == 0 || !e.sameLayout(layout, model.TypeLayout{Type: key.Column.Type, Children: key.Column.Children}, 0) {
			return e.invalid("key differs from graph expression")
		}
		for _, previous := range e.keys[:i] {
			if !e.step() {
				return false
			}
			if key.Slot != 0 && previous.Slot == key.Slot || key.Expression != 0 && previous.Expression == key.Expression {
				return e.invalid("duplicate key expression")
			}
		}
	}
	for i, p := range e.parameters {
		if !e.step() {
			return false
		}
		if p.Number == 0 {
			return e.invalid("zero parameter number")
		}
		for _, previous := range e.parameters[:i] {
			if !e.step() {
				return false
			}
			if previous.Number == p.Number {
				return e.invalid("duplicate parameter")
			}
		}
		if !e.validateParameter(p) {
			return false
		}
	}
	if !e.requireParameters {
		return e.err == nil
	}
	for id := ir.ScalarID(1); uint64(id) <= uint64(e.graph.ScalarCount()) && e.step(); id++ {
		n, _ := e.graph.Scalar(id)
		if n.Kind != ir.Param {
			continue
		}
		found := false
		for _, p := range e.parameters {
			if !e.step() {
				return false
			}
			if p.Number == n.Parameter {
				found = true
				break
			}
		}
		if !found {
			return e.invalid("missing parameter")
		}
	}
	return e.err == nil
}

func (e *extractor) validateParameter(p Parameter) bool {
	declared := false
	for id := ir.ScalarID(1); uint64(id) <= uint64(e.graph.ScalarCount()) && e.step(); id++ {
		n, _ := e.graph.Scalar(id)
		if n.Kind != ir.Param || n.Parameter != p.Number {
			continue
		}
		declared = true
		invalidNull := !n.Nullable && p.Value.IsNull
		if invalidNull || !e.sameLayout(n.Layout, p.Layout, 0) || !e.validValue(p.Layout, p.Value, 0) {
			return e.invalid("parameter type or value differs from declaration")
		}
	}
	if !declared {
		return e.invalid("undeclared parameter")
	}
	return e.err == nil
}

func (e *extractor) sameLayout(id ir.LayoutID, layout model.TypeLayout, depth int) bool {
	if !e.step() {
		return false
	}
	if depth > 64 {
		return false
	}
	typ, ok := e.graph.Type(id)
	if !ok || typ != layout.Type || uint64(e.graph.LayoutChildCount(id)) != uint64(len(layout.Children)) {
		return false
	}
	for i, child := range layout.Children {
		childID, _ := e.graph.LayoutChild(id, uint32(i))
		if !e.sameLayout(childID, child, depth+1) {
			return false
		}
	}
	return true
}

func (e *extractor) validValue(layout model.TypeLayout, v model.Value, depth int) bool {
	if !e.step() || depth > 64 || v.Type != layout.Type.Storage {
		return false
	}
	if v.IsNull {
		return true
	}
	switch v.Type {
	case model.TypeArray:
		if len(layout.Children) != 1 || model.ValidateArrayShape(v.Dimensions, uint64(len(v.ArrayVal))) != nil {
			return false
		}
		for _, item := range v.ArrayVal {
			if !e.validValue(layout.Children[0], item, depth+1) {
				return false
			}
		}
	case model.TypeComposite:
		if len(v.ArrayVal) != len(layout.Children) {
			return false
		}
		for i, item := range v.ArrayVal {
			if !e.validValue(layout.Children[i], item, depth+1) {
				return false
			}
		}
	default:
		return validNumber(layout.Type, v)
	}
	return true
}
