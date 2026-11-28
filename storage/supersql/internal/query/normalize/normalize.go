// Package normalize rebuilds immutable logical graphs with effect-safe rewrites.
package normalize

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var (
	ErrInvalid = errors.New("invalid normalization options or graph")
	ErrLimit   = errors.New("normalization resource limit exceeded")
)

// MaxWork bounds visits and operand processing. MaxBytes bounds cumulative
// scratch allocation, including owned constants; Limits separately bounds the
// returned graph and Builder storage. All budgets must be positive.
type Options struct {
	Limits   ir.Limits
	MaxWork  uint64
	MaxBytes uint64
	Builtin  builtin.ResolveOptions
}

type scalarResult struct {
	id           ir.ScalarID
	n            ir.Scalar
	value        model.Value
	row          []scalarResult
	context      ir.RelID
	projection   ir.RelID
	bounds       bool
	sourceLayout ir.LayoutID
}

type relResult struct {
	id        ir.RelID
	n         ir.Rel
	input     ir.RelID
	predicate scalarResult
	outputs   []model.SlotID
	constants []constantColumn
}

type normalizer struct {
	ctx                     context.Context
	g                       *ir.Graph
	b                       *ir.Builder
	o                       Options
	err                     error
	work, bytes             uint64
	layouts                   []ir.LayoutID
	scalars                   []scalarResult
	rels                      []relResult
	scalarActive, relActive   []bool
	relUses                   []uint32
	joinKinds                 []ir.JoinKind
	scanOutputs               [][]model.SlotID
	pruneScan                 []bool
	constantInputs            []constantColumn
	scalarContext             ir.RelID
	projectInput              ir.RelID
	foldBounds                bool
}

func Normalize(ctx context.Context, graph *ir.Graph, options Options) (*ir.Graph, error) {
	if ctx == nil || graph == nil || graph.Root() == 0 {
		return nil, ErrInvalid
	}
	if options.MaxWork == 0 || options.MaxBytes == 0 || options.MaxBytes > math.MaxInt64 {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	builder, err := ir.NewBuilder(options.Limits)
	if err != nil {
		return nil, err
	}
	n := normalizer{ctx: ctx, g: graph, b: builder, o: options}
	n.layouts = allocate[ir.LayoutID](&n, uint64(graph.LayoutCount())+1)
	n.scalars = allocate[scalarResult](&n, uint64(graph.ScalarCount())+1)
	n.rels = allocate[relResult](&n, uint64(graph.RelCount())+1)
	n.scalarActive = allocate[bool](&n, uint64(graph.ScalarCount())+1)
	n.relActive = allocate[bool](&n, uint64(graph.RelCount())+1)
	n.relUses = allocate[uint32](&n, uint64(graph.RelCount())+1)
	n.joinKinds = allocate[ir.JoinKind](&n, uint64(graph.RelCount())+1)
	n.scanOutputs = allocate[[]model.SlotID](&n, uint64(graph.RelCount())+1)
	n.pruneScan = allocate[bool](&n, uint64(graph.RelCount())+1)
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()) && n.err == nil; id++ {
		relation, _ := graph.Rel(id)
		for i := range relation.InputCount() {
			input, _ := graph.Input(id, i)
			if n.relUses[input] == ^uint32(0) {
				n.err = resourceError()
				break
			}
			n.relUses[input]++
		}
	}
	if n.err != nil {
		return nil, n.err
	}
	n.metadata()
	if n.err != nil {
		return nil, n.err
	}
	root := n.relation(graph.Root(), 0)
	// Definitions are roots too: writable and explicitly materialized CTEs
	// cannot disappear merely because their consumer was simplified away.
	for id := ir.CTEID(1); id <= ir.CTEID(graph.CTECount()) && n.err == nil; id++ {
		cte, _ := graph.CTE(id)
		body := n.relation(cte.Body, 0)
		if n.err == nil {
			n.err = builder.BindCTE(ctx, id, body.id)
		}
	}
	if n.err != nil {
		return nil, n.err
	}
	return builder.Build(ctx, root.id)
}

func (n *normalizer) step(count uint64) bool {
	if n.err != nil {
		return false
	}
	if n.err = n.ctx.Err(); n.err != nil {
		return false
	}
	if count > n.o.MaxWork-n.work {
		n.err = resourceError()
		return false
	}
	n.work += count
	return true
}

func resourceError() error {
	return &model.Error{Code: "54000", Message: "normalization resource limit exceeded", Cause: ErrLimit}
}

func (n *normalizer) charge(count, size uint64) bool {
	if n.err != nil {
		return false
	}
	if size != 0 && count > (n.o.MaxBytes-n.bytes)/size {
		n.err = resourceError()
		return false
	}
	n.bytes += count * size
	return true
}

func allocate[T any](n *normalizer, count uint64) []T {
	var item T
	if count > uint64(^uint(0)>>1) {
		n.err = resourceError()
		return nil
	}
	if !n.step(count) || !n.charge(count, uint64(unsafe.Sizeof(item))) {
		return nil
	}
	return make([]T, int(count))
}

func (n *normalizer) metadata() {
	children := allocate[bool](n, uint64(n.g.LayoutCount())+1)
	if n.err != nil {
		return
	}
	for id := ir.LayoutID(1); id <= ir.LayoutID(n.g.LayoutCount()); id++ {
		for i := uint32(0); i < n.g.LayoutChildCount(id); i++ {
			if !n.step(1) {
				return
			}
			child, _ := n.g.LayoutChild(id, i)
			children[child] = true
		}
	}
	// AddLayout inserts a whole tree in postorder. Rebuilding only tree roots
	// avoids cloning each descendant again for every enclosing layout.
	var next ir.LayoutID
	for id := ir.LayoutID(1); id <= ir.LayoutID(n.g.LayoutCount()) && n.err == nil; id++ {
		if children[id] {
			continue
		}
		layout := n.layout(id, 0, &next)
		if n.err != nil {
			return
		}
		_, n.err = n.b.AddLayout(n.ctx, layout)
	}
	for id := ir.ScopeID(1); id <= ir.ScopeID(n.g.ScopeCount()) && n.err == nil; id++ {
		if !n.step(1) {
			return
		}
		parent, _ := n.g.ScopeParent(id)
		_, n.err = n.b.AddScope(n.ctx, parent, nil)
	}
	for id := model.SlotID(1); id <= model.SlotID(n.g.SlotCount()) && n.err == nil; id++ {
		if !n.step(1) {
			return
		}
		slot, _ := n.g.Slot(id)
		slot.Layout = n.layouts[slot.Layout]
		_, n.err = n.b.AddSlot(n.ctx, slot)
	}
	for id := ir.ScopeID(1); id <= ir.ScopeID(n.g.ScopeCount()) && n.err == nil; id++ {
		for i := uint32(0); i < n.g.CaptureCount(id); i++ {
			if !n.step(uint64(i) + 1) {
				return
			}
			slot, _ := n.g.Capture(id, i)
			n.err = n.b.AddScopeCapture(n.ctx, id, slot)
		}
	}
	for id := ir.SignatureID(1); id <= ir.SignatureID(n.g.SignatureCount()) && n.err == nil; id++ {
		if !n.step(1) {
			return
		}
		sig, _ := n.g.Signature(id)
		params := allocate[ir.LayoutID](n, uint64(sig.ParameterCount()))
		if n.err != nil {
			return
		}
		for i := range params {
			old, _ := n.g.SignatureParameter(id, uint32(i))
			params[i] = n.layouts[old]
		}
		meta := sig.Metadata()
		meta.Result = n.layouts[meta.Result]
		_, n.err = n.b.AddSignature(n.ctx, ir.SignatureSpec{Signature: meta, Parameters: params})
	}
	for id := ir.CTEID(1); id <= ir.CTEID(n.g.CTECount()) && n.err == nil; id++ {
		if !n.step(1) {
			return
		}
		cte, _ := n.g.CTE(id)
		outputs := allocate[model.SlotID](n, uint64(cte.OutputCount()))
		if n.err != nil {
			return
		}
		for i := range outputs {
			outputs[i], _ = n.g.CTEOutput(id, uint32(i))
		}
		_, n.err = n.b.DeclareCTE(n.ctx, ir.CTESpec{Scope: cte.Scope, Outputs: outputs, Materialization: cte.Materialization, Recursive: cte.Recursive, Effects: cte.Effects})
	}
}

func (n *normalizer) layout(id ir.LayoutID, depth uint32, next *ir.LayoutID) model.TypeLayout {
	if !n.step(1) {
		return model.TypeLayout{}
	}
	if depth >= min(n.o.Limits.MaxDepth, 65) {
		n.err = resourceError()
		return model.TypeLayout{}
	}
	typ, _ := n.g.Type(id)
	result := model.TypeLayout{Type: typ, Children: allocate[model.TypeLayout](n, uint64(n.g.LayoutChildCount(id)))}
	if n.err != nil {
		return model.TypeLayout{}
	}
	result.FieldNames = allocate[string](n, uint64(n.g.LayoutFieldNameCount(id)))
	for i := range result.FieldNames {
		if !n.step(1) {
			return model.TypeLayout{}
		}
		name, _ := n.g.LayoutFieldName(id, uint32(i))
		if !n.charge(uint64(len(name)), 1) {
			return model.TypeLayout{}
		}
		result.FieldNames[i] = name
	}
	for i := range result.Children {
		child, _ := n.g.LayoutChild(id, uint32(i))
		result.Children[i] = n.layout(child, depth+1, next)
		if n.err != nil {
			return model.TypeLayout{}
		}
	}
	(*next)++
	n.layouts[id] = *next
	return result
}

// valueBytes charges the datum's actual owned tree against the budget so each
// shared constant consumes the budget only once.
func (n *normalizer) valueBytes(v model.Value) {
	if !n.step(1) {
		return
	}
	n.charge(uint64(len(v.StrVal))+uint64(len(v.BytesVal)), 1)
	n.charge(uint64(len(v.VectorVal)), 4)
	n.charge(uint64(len(v.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{})))
	n.charge(uint64(len(v.ArrayVal)), uint64(unsafe.Sizeof(model.Value{})))
	for _, child := range v.ArrayVal {
		n.valueBytes(child)
		if n.err != nil {
			return
		}
	}
}
