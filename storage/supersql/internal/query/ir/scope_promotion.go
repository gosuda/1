package ir

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ErrScopeDependency identifies a CTE declared below the destination scope.
var ErrScopeDependency = errors.New("scope promotion crosses a CTE declaration")

type scopeCaptureMark struct {
	id    ScopeID
	count uint32
}

// ScopeMark bounds one scalar expression's append-only construction. It belongs
// to one Builder and may be promoted once; earlier nodes are never rewritten.
type ScopeMark struct {
	builder                      *Builder
	source                       ScopeID
	scalars, rels, slots, scopes int
	captures                     []scopeCaptureMark
	used                         bool
}

// MarkScope records the arena suffix and the capture prefixes that precede an
// expression. Temporary marks do not transfer ownership of any graph arena.
func (b *Builder) MarkScope(ctx context.Context, source ScopeID) (*ScopeMark, error) {
	if err := b.check(ctx); err != nil {
		return nil, err
	}
	if !validID(source, b.graph.scopes) {
		return nil, b.fail(invalid("invalid expression scope mark"))
	}
	mark := &ScopeMark{builder: b, source: source, scalars: len(b.graph.scalars), rels: len(b.graph.rels), slots: len(b.graph.slots), scopes: len(b.graph.scopes)}
	for id := source; id != 0; id = b.graph.scopes[id-1].parent {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		if uint32(len(mark.captures)) >= b.limits.MaxDepth {
			return nil, b.fail(limitError())
		}
		mark.captures = append(mark.captures, scopeCaptureMark{id: id, count: b.graph.scopes[id-1].captures.count})
	}
	return mark, nil
}

// PromoteScope moves the marked expression to an ancestor. The suffix may
// contain child queries, but may not declare a relation or slot in the source
// scope. replacements map grouped output slots to their original input slots.
// Only rowRoots and their subquery descendants receive those replacements.
// directRoots retain grouped slots. An affected node or subquery shared by both
// phases is rejected before mutation; unaffected shared expressions are valid.
// Roots must be newly constructed scalars in the marked source scope.
// Every changed capture scope is returned so the binder can update its mirrors.
// Arena capacities and resource charges are preserved; Build still validates
// every node, including nodes that are not reachable from the final root.
func (b *Builder) PromoteScope(ctx context.Context, mark *ScopeMark, target ScopeID, replacements map[model.SlotID]model.SlotID, directRoots, rowRoots []ScalarID) ([]ScopeID, error) {
	if err := b.check(ctx); err != nil {
		return nil, err
	}
	if mark == nil || mark.builder != b || mark.used || !validID(mark.source, b.graph.scopes) || !validID(target, b.graph.scopes) ||
		!b.graph.ancestor(target, b.graph.scopes[mark.source-1].parent) ||
		mark.scalars > len(b.graph.scalars) || mark.rels > len(b.graph.rels) || mark.slots > len(b.graph.slots) || mark.scopes > len(b.graph.scopes) {
		return nil, b.fail(invalid("invalid expression scope promotion"))
	}
	for from, to := range replacements {
		if !validID(from, b.graph.slots) || !validID(to, b.graph.slots) {
			return nil, b.fail(invalid("invalid promoted slot replacement"))
		}
		old, next := b.graph.slots[from-1], b.graph.slots[to-1]
		if old.Scope != target || next.Scope != target || !b.graph.sameLayout(old.Layout, next.Layout) || next.Nullable && !old.Nullable {
			return nil, b.fail(invalid("promoted slot replacement changes its type"))
		}
	}
	for _, slot := range b.graph.slots[mark.slots:] {
		if slot.Scope == mark.source {
			return nil, b.fail(invalid("promoted scalar expression declares a local slot"))
		}
	}
	for _, rel := range b.graph.rels[mark.rels:] {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		if rel.Scope == mark.source {
			return nil, b.fail(invalid("promoted scalar expression declares a local relation"))
		}
		if rel.Kind == CTEConsumer || rel.Kind == CTEProducer {
			declaration := b.graph.ctes[rel.CTE-1].Scope
			if b.graph.ancestor(declaration, mark.source) && !b.graph.ancestor(declaration, target) {
				return nil, b.fail(ErrScopeDependency)
			}
		}
	}
	scalarPhases, scopePhases, err := b.scopePromotionPhases(ctx, mark, directRoots, rowRoots)
	if err != nil {
		return nil, err
	}
	for index, scalar := range b.graph.scalars[mark.scalars:] {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		if scalar.Kind == Slot && scalarPhases[index] == promotionDirect|promotionRow {
			if next, ok := replacements[scalar.Slot]; ok && next != scalar.Slot {
				return nil, b.fail(invalid("scope promotion shares a grouped slot across argument phases"))
			}
		}
		if scalar.Scope != mark.source {
			continue
		}
		for _, operand := range sliceAt(b.graph.scalarRefs, scalar.operands) {
			if int(operand) <= mark.scalars && b.graph.scalars[operand-1].Scope == mark.source {
				return nil, b.fail(invalid("scope promotion reuses an earlier scalar"))
			}
		}
		if scalar.Kind == Slot {
			slot := scalar.Slot
			if scalarPhases[index]&promotionRow != 0 {
				if next, ok := replacements[slot]; ok {
					slot = next
				}
			}
			if !b.graph.visible(slot, target) {
				return nil, b.fail(invalid("promoted expression captures an unavailable slot"))
			}
		}
		if scalar.Kind == Subquery && int(b.graph.rels[scalar.Relation-1].Scope) <= mark.scopes {
			return nil, b.fail(invalid("scope promotion reuses an earlier subquery"))
		}
	}
	for _, capture := range mark.captures {
		if capture.id == target {
			break
		}
		if b.graph.scopes[capture.id-1].captures.count < capture.count {
			return nil, b.fail(invalid("scope capture prefix was replaced"))
		}
	}
	for index, phase := range scopePhases {
		if phase != promotionDirect|promotionRow {
			continue
		}
		for _, slot := range sliceAt(b.graph.slotRefs, b.graph.scopes[mark.scopes+index].captures) {
			if err := b.check(ctx); err != nil {
				return nil, err
			}
			if next, ok := replacements[slot]; ok && next != slot {
				return nil, b.fail(invalid("scope promotion shares a grouped capture across argument phases"))
			}
		}
	}
	mark.used = true
	changed := make([]ScopeID, 0, len(mark.captures)+len(b.graph.scopes)-mark.scopes)
	for _, capture := range mark.captures {
		if capture.id == target {
			break
		}
		b.graph.scopes[capture.id-1].captures.count = capture.count
		changed = append(changed, capture.id)
	}
	for index := mark.scopes; index < len(b.graph.scopes); index++ {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		scope := &b.graph.scopes[index]
		if scope.parent == mark.source {
			scope.parent = target
		}
		slots := sliceAt(b.graph.slotRefs, scope.captures)
		retained := slots[:0]
		for _, slot := range slots {
			if scopePhases[index-mark.scopes]&promotionRow != 0 {
				if next, ok := replacements[slot]; ok {
					slot = next
				}
			}
			if !contains(retained, slot) {
				retained = append(retained, slot)
			}
		}
		scope.captures.count = uint32(len(retained))
		changed = append(changed, ScopeID(index+1))
	}
	for index := mark.scalars; index < len(b.graph.scalars); index++ {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		scalar := &b.graph.scalars[index]
		if scalar.Scope == mark.source {
			scalar.Scope = target
		}
		if scalar.Kind == Slot && scalarPhases[index-mark.scalars]&promotionRow != 0 {
			if next, ok := replacements[scalar.Slot]; ok {
				scalar.Slot = next
				scalar.Nullable = b.graph.slots[next-1].Nullable
			}
		}
		if scalar.Kind == Once || scalar.Kind == Collate || scalar.Kind == Relabel {
			input := b.graph.scalars[b.graph.scalarRefs[scalar.operands.start]-1]
			scalar.Nullable = input.Nullable
		}
	}
	return changed, nil
}

func (b *Builder) Rel(id RelID) (Rel, bool) { return b.graph.Rel(id) }

func (b *Builder) CaptureCount(id ScopeID) uint32 { return b.graph.CaptureCount(id) }

func (b *Builder) Capture(id ScopeID, index uint32) (model.SlotID, bool) {
	return b.graph.Capture(id, index)
}
