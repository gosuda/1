package ir

import (
	"context"
	"errors"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrFrozen  = errors.New("IR builder is frozen")
	ErrInvalid = errors.New("invalid typed IR")
	ErrLimit   = errors.New("IR resource limit exceeded")
)

// Builder is single-owner, not concurrent. Any failed mutation poisons it; Build
// transfers its arenas and permanently closes it. Graph is safe for concurrent reads.
type Builder struct {
	graph           Graph
	limits          Limits
	bytes           uint64
	err             error
	frozen          bool
	self            *Builder
	declaredMarkers bool
}
type Graph struct {
	root              RelID
	bytes             uint64
	scalars           []Scalar
	rels              []Rel
	slots             []SlotInfo
	scopes            []scope
	layouts           []layout
	ctes              []CTE
	signatures        []Signature
	values            []model.Value
	scalarRefs        []ScalarID
	comparisonRefs    []SignatureID
	relRefs           []RelID
	slotRefs          []model.SlotID
	layoutRefs        []LayoutID
	columnRefs        []model.ColumnID
	fieldNames        []string
	bindings          []Binding
	conflictArbiters  []model.ConflictArbiter
	orders            []OrderKey
	groups            []extent
	indices           []uint32
	aggregates        []AggregateCall
	groupings         []GroupingCall
	windows           []WindowCall
	locks             []RowLock
	lockKeys          []RowLockKey
	actions           []MergeAction
	jsonTableColumns  []JSONTableColumn
	viewMutationSteps []ViewMutationStep
}

func NewBuilder(limits Limits) (*Builder, error) {
	if limits.MaxDepth > 512 {
		return nil, limitError()
	}
	emptyHandles := limits.MaxNodes == 0 || limits.MaxOperands == 0 || limits.MaxSlots == 0
	emptyResources := limits.MaxDepth == 0 || limits.MaxBytes == 0 || limits.SourceBytes == 0
	exhaustedHandles := limits.MaxNodes == math.MaxUint32 || limits.MaxSlots == math.MaxUint32 || limits.MaxOperands == math.MaxUint32
	if emptyHandles || emptyResources || exhaustedHandles {
		return nil, invalid("limits must be positive and leave zero handles reserved")
	}
	b := &Builder{limits: limits}
	b.self = b
	return b, nil
}

// NewDeclaredBuilder is NewBuilder plus admission of declared pseudo-type
// markers ({pseudo-type ID, TypeAny storage, modifier -1, childless} or
// internal's bounded byte payload) as expression and result layouts. Routine
// declarations bind their bodies with signature types still unresolved — a
// polymorphic parameter is a marker until a call site resolves it — so
// declaration binding needs this mode. Executable statement graphs must use
// NewBuilder: a marker reaching execution is a defect.
func NewDeclaredBuilder(limits Limits) (*Builder, error) {
	b, err := NewBuilder(limits)
	if err != nil {
		return nil, err
	}
	b.declaredMarkers = true
	return b, nil
}
func invalid(message string) error {
	return &model.Error{Code: "XX000", Message: message, Cause: ErrInvalid}
}
func limitError() error {
	return &model.Error{Code: "54000", Message: "IR resource limit exceeded", Cause: ErrLimit}
}
func (b *Builder) fail(err error) error {
	if b.err == nil {
		b.err = err
	}
	return err
}
func (b *Builder) check(ctx context.Context) error {
	if b.self != b {
		return invalid("IR builder must not be copied")
	}
	if b.frozen {
		return ErrFrozen
	}
	if b.err != nil {
		return b.err
	}
	if err := ctx.Err(); err != nil {
		return b.fail(err)
	}
	return nil
}
func (b *Builder) charge(n uint64) error {
	if n > b.limits.MaxBytes-b.bytes {
		return b.fail(limitError())
	}
	b.bytes += n
	return nil
}

// Arena growth accounts for capacity, including old+new buffers during copying.
func reserveArena[T any](b *Builder, arena *[]T, count uint64, maximum uint32) (extent, error) {
	old := *arena
	if count > uint64(maximum)-uint64(len(old)) {
		return extent{}, b.fail(limitError())
	}
	if uint64(len(old))+count > uint64(math.MaxInt) {
		return extent{}, b.fail(limitError())
	}
	needed := len(old) + int(count)
	if count == 0 {
		// An empty reservation has no location. Returning {0,0} rather than
		// {len(old),0} keeps extents canonical: graph consumers compare extents
		// structurally, and a zero-count range is never distinct from the zero value.
		return extent{}, nil
	}
	if needed > cap(old) {
		capacity := min(uint64(maximum), max(uint64(needed), uint64(cap(old))*2))
		if capacity > uint64(math.MaxInt) {
			return extent{}, b.fail(limitError())
		}
		var element T
		size := uint64(unsafe.Sizeof(element))
		if size != 0 && capacity > (b.limits.MaxBytes-b.bytes)/size {
			return extent{}, b.fail(limitError())
		}
		if err := b.charge(capacity * size); err != nil {
			return extent{}, err
		}
		next := make([]T, len(old), int(capacity))
		copy(next, old)
		b.bytes -= uint64(cap(old)) * size
		old = next
	}
	result := extent{uint32(len(old)), uint32(count)}
	*arena = old[:needed]
	return result, nil
}
func appendArena[T any](b *Builder, arena *[]T, values []T, maximum uint32) (extent, error) {
	r, err := reserveArena(b, arena, uint64(len(values)), maximum)
	if err != nil {
		return extent{}, err
	}
	copy(sliceAt(*arena, r), values)
	return r, nil
}
func appendOne[T any](b *Builder, arena *[]T, value T, maximum uint32) (uint32, error) {
	r, err := appendArena(b, arena, []T{value}, maximum)
	return r.start + 1, err
}
func (b *Builder) nodeBudget() error {
	n := uint64(len(b.graph.scalars)) + uint64(len(b.graph.rels)) + uint64(len(b.graph.layouts)) + uint64(len(b.graph.scopes)) + uint64(len(b.graph.ctes)) + uint64(len(b.graph.signatures))
	if n >= uint64(b.limits.MaxNodes) {
		return b.fail(limitError())
	}
	return nil
}
func (b *Builder) AddScope(ctx context.Context, parent ScopeID, captures []model.SlotID) (ScopeID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	if parent != 0 && !validID(parent, b.graph.scopes) {
		return 0, b.fail(invalid("invalid scope parent"))
	}
	if uint64(len(captures)) > uint64(b.limits.MaxOperands)-uint64(len(b.graph.slotRefs)) {
		return 0, b.fail(limitError())
	}
	for id, depth := parent, uint32(1); id != 0; id, depth = b.graph.scopes[id-1].parent, depth+1 {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		if depth >= b.limits.MaxDepth {
			return 0, b.fail(limitError())
		}
	}
	for i, id := range captures {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		if !validID(id, b.graph.slots) || !b.graph.ancestor(b.graph.slots[id-1].Scope, parent) {
			return 0, b.fail(invalid("capture outside parent scope"))
		}
		for _, previous := range captures[:i] {
			if previous == id {
				return 0, b.fail(invalid("duplicate capture"))
			}
		}
	}
	r, err := appendArena(b, &b.graph.slotRefs, captures, b.limits.MaxOperands)
	if err != nil {
		return 0, err
	}
	id, err := appendOne(b, &b.graph.scopes, scope{parent: parent, captures: r, captureCapacity: r.count}, b.limits.MaxNodes)
	return ScopeID(id), err
}

// AddScopeCapture is idempotent. The slot must belong to an ancestor scope and
// must be captured before constructing any scalar that refers to it.
func (b *Builder) AddScopeCapture(ctx context.Context, scopeID ScopeID, slot model.SlotID) error {
	if err := b.check(ctx); err != nil {
		return err
	}
	if !validID(scopeID, b.graph.scopes) || !validID(slot, b.graph.slots) {
		return b.fail(invalid("invalid capture scope or slot"))
	}
	scope := &b.graph.scopes[scopeID-1]
	if !b.graph.ancestor(b.graph.slots[slot-1].Scope, scope.parent) {
		return b.fail(invalid("capture outside parent scope"))
	}
	for _, captured := range sliceAt(b.graph.slotRefs, scope.captures) {
		if err := b.check(ctx); err != nil {
			return err
		}
		if captured == slot {
			return nil
		}
	}
	if scope.captures.count == scope.captureCapacity {
		// Retired blocks stay quota-accounted. Doubling bounds their total size
		// and copies by O(n), while Graph keeps constant-time indexed captures.
		capacity := max(uint64(1), uint64(scope.captureCapacity)*2)
		capacity = min(capacity, uint64(b.limits.MaxOperands)-uint64(len(b.graph.slotRefs)))
		if capacity <= uint64(scope.captures.count) {
			return b.fail(limitError())
		}
		block, err := reserveArena(b, &b.graph.slotRefs, capacity, b.limits.MaxOperands)
		if err != nil {
			return err
		}
		copy(sliceAt(b.graph.slotRefs, block), sliceAt(b.graph.slotRefs, scope.captures))
		scope.captures.start = block.start
		scope.captureCapacity = block.count
	}
	b.graph.slotRefs[scope.captures.start+scope.captures.count] = slot
	scope.captures.count++
	return nil
}
func (b *Builder) AddLayout(ctx context.Context, value model.TypeLayout) (LayoutID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	return b.addLayout(ctx, value, 0)
}
func (b *Builder) addLayout(ctx context.Context, value model.TypeLayout, depth uint32) (LayoutID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if depth >= min(b.limits.MaxDepth, 65) {
		return 0, b.fail(limitError())
	}
	invalidArray := value.Type.Storage == model.TypeArray && len(value.Children) != 1
	scalarStorage := value.Type.Storage != model.TypeArray && value.Type.Storage != model.TypeComposite
	unexpectedChildren := scalarStorage && len(value.Children) != 0
	invalidNames := len(value.FieldNames) != 0 && (value.Type.Storage != model.TypeComposite || len(value.FieldNames) != len(value.Children))
	if value.Type.ID == 0 || !value.Type.Storage.Valid() || invalidArray || unexpectedChildren || invalidNames {
		declared := b.declaredMarkers && len(value.Children) == 0 && len(value.FieldNames) == 0 &&
			value.Type.Modifier == -1 && model.IsPseudoTypeID(value.Type.ID) &&
			(value.Type.Storage == model.TypeAny || value.Type.ID == 2281 && value.Type.Storage == model.TypeBytes)
		if !declared {
			return 0, b.fail(invalid("invalid result type layout"))
		}
	}
	// Reserve a contiguous child range before descending; descendants may grow it.
	if uint64(len(value.Children)) > uint64(b.limits.MaxOperands)-uint64(len(b.graph.layoutRefs)) {
		return 0, b.fail(limitError())
	}
	start := uint32(len(b.graph.layoutRefs))
	for range value.Children {
		if _, err := appendOne(b, &b.graph.layoutRefs, LayoutID(0), b.limits.MaxOperands); err != nil {
			return 0, err
		}
	}
	for i, child := range value.Children {
		id, err := b.addLayout(ctx, child, depth+1)
		if err != nil {
			return 0, err
		}
		b.graph.layoutRefs[int(start)+i] = id
	}
	names, err := reserveArena(b, &b.graph.fieldNames, uint64(len(value.FieldNames)), b.limits.MaxOperands)
	if err != nil {
		return 0, err
	}
	for i, name := range value.FieldNames {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		if err := b.charge(uint64(len(name))); err != nil {
			return 0, err
		}
		b.graph.fieldNames[int(names.start)+i] = strings.Clone(name)
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	id, err := appendOne(b, &b.graph.layouts, layout{typ: value.Type, children: extent{start, uint32(len(value.Children))}, fieldNames: names}, b.limits.MaxNodes)
	return LayoutID(id), err
}
func (b *Builder) AddSlot(ctx context.Context, slot SlotInfo) (model.SlotID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if !validID(slot.Scope, b.graph.scopes) || !validID(slot.Layout, b.graph.layouts) {
		return 0, b.fail(invalid("invalid slot scope or type"))
	}
	id, err := appendOne(b, &b.graph.slots, slot, b.limits.MaxSlots)
	return model.SlotID(id), err
}
func (b *Builder) AddScalar(ctx context.Context, spec ScalarSpec) (ScalarID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	n := spec.Scalar
	if n.operands != (extent{}) || n.comparisons != (extent{}) || n.constant != 0 {
		return 0, b.fail(invalid("scalar contains arena payload"))
	}
	r, err := appendArena(b, &b.graph.scalarRefs, spec.Operands, b.limits.MaxOperands)
	if err != nil {
		return 0, err
	}
	n.operands = r
	comparisons, err := appendArena(b, &b.graph.comparisonRefs, spec.Comparisons, b.limits.MaxOperands)
	if err != nil {
		return 0, err
	}
	n.comparisons = comparisons
	if n.Kind == Const {
		if !validID(n.Layout, b.graph.layouts) {
			return 0, b.fail(invalid("invalid constant layout"))
		}
		if err := b.graph.validateValue(ctx, n.Layout, spec.Value, 0, b.limits.MaxDepth); err != nil {
			return 0, b.fail(err)
		}
		value, used, err := model.CopyValue(ctx, spec.Value, b.limits.MaxBytes-b.bytes)
		if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
			err = limitError()
		}
		if err != nil {
			return 0, b.fail(err)
		}
		if err := b.charge(used); err != nil {
			return 0, err
		}
		n.constant, err = appendOne(b, &b.graph.values, value, b.limits.MaxNodes)
		if err != nil {
			return 0, err
		}
	}
	if err := b.graph.validateScalar(ctx, n, b.limits); err != nil {
		return 0, b.fail(err)
	}
	id, err := appendOne(b, &b.graph.scalars, n, b.limits.MaxNodes)
	return ScalarID(id), err
}
func (b *Builder) DeclareCTE(ctx context.Context, spec CTESpec) (CTEID, error) {
	scope, outputs, materialization, recursive := spec.Scope, spec.Outputs, spec.Materialization, spec.Recursive
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	invalidMaterialization := materialization > MaterializeNever || (recursive && materialization == MaterializeNever)
	if !validID(scope, b.graph.scopes) || invalidMaterialization {
		return 0, b.fail(invalid("invalid CTE declaration"))
	}
	if spec.Effects.Volatility > Volatile {
		return 0, b.fail(invalid("invalid CTE effects"))
	}
	if uint64(len(outputs)) > uint64(b.limits.MaxOperands)-uint64(len(b.graph.slotRefs)) {
		return 0, b.fail(limitError())
	}
	for i, id := range outputs {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		if !validID(id, b.graph.slots) || b.graph.slots[id-1].Scope != scope || contains(outputs[:i], id) {
			return 0, b.fail(invalid("invalid or duplicate CTE output"))
		}
	}
	r, err := appendArena(b, &b.graph.slotRefs, outputs, b.limits.MaxOperands)
	if err != nil {
		return 0, err
	}
	id, err := appendOne(b, &b.graph.ctes, CTE{Scope: scope, Materialization: materialization, Recursive: recursive, Effects: spec.Effects, outputs: r}, b.limits.MaxNodes)
	return CTEID(id), err
}

// BindCTE accepts a same-scope body or an independent direct-child query scope.
// A child body cannot capture row slots owned by the declaration scope.
func (b *Builder) BindCTE(ctx context.Context, id CTEID, body RelID) error {
	if err := b.check(ctx); err != nil {
		return err
	}
	if !validID(id, b.graph.ctes) || !validID(body, b.graph.rels) {
		return b.fail(invalid("invalid CTE binding"))
	}
	cte := &b.graph.ctes[id-1]
	if cte.Body != 0 || !b.graph.compatibleOutputs(cte.outputs, b.graph.rels[body-1].outputs) {
		return b.fail(invalid("CTE body schema mismatch or duplicate binding"))
	}
	if err := b.graph.validateCTEBodyScope(ctx, *cte, b.graph.rels[body-1]); err != nil {
		return b.fail(err)
	}
	if cte.Recursive && (b.graph.rels[body-1].Kind != RecursiveUnion || b.graph.rels[body-1].CTE != id) {
		return b.fail(invalid("recursive CTE needs its recursive union"))
	}
	if cte.Effects.Union(b.graph.rels[body-1].Effects) != cte.Effects {
		return b.fail(invalid("CTE body effects exceed declaration"))
	}
	cte.Body = body
	return nil
}

func (b *Builder) Build(ctx context.Context, root RelID) (*Graph, error) {
	if err := b.verifyGraph(ctx, root); err != nil {
		return nil, err
	}
	b.graph.root = root
	b.graph.bytes = b.bytes
	graph := b.graph
	b.graph = Graph{}
	b.frozen = true
	return &graph, nil
}

func (b *Builder) verifyGraph(ctx context.Context, root RelID) error {
	if err := b.check(ctx); err != nil {
		return err
	}
	if !validID(root, b.graph.rels) {
		return b.fail(invalid("invalid root relation"))
	}
	scopeID := b.graph.rels[root-1].Scope
	if !validID(scopeID, b.graph.scopes) || b.graph.scopes[scopeID-1].parent != 0 {
		return b.fail(invalid("root relation has an invalid or unresolved outer scope"))
	}
	verificationBytes := uint64(len(b.graph.rels)) + uint64(len(b.graph.scalars))*uint64(unsafe.Sizeof(uint32(0))) + uint64(len(b.graph.ctes))*(1+uint64(unsafe.Sizeof(uint64(0))))
	if err := b.charge(verificationBytes); err != nil {
		return err
	}
	accepted := make([]bool, len(b.graph.rels))
	scalarHeights := make([]uint32, len(b.graph.scalars))
	activeCTEs := make([]bool, len(b.graph.ctes))
	countedCTEs := make([]uint64, len(b.graph.ctes))
	if err := b.graph.verify(ctx, root, b.limits, accepted, scalarHeights, activeCTEs, countedCTEs); err != nil {
		return b.fail(err)
	}
	b.bytes -= verificationBytes
	return nil
}
