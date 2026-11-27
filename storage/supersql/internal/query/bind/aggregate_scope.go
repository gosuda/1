package bind

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) markAggregateScope(s *scope) (*ir.ScopeMark, error) {
	if s.parent == nil {
		return nil, nil
	}
	return b.builder.MarkScope(b.ctx, s.id)
}

// aggregateScope chooses the nearest level referenced by per-row arguments,
// FILTER and ORDER BY. Direct ordered-set arguments do not choose that level.
func (b *binder) aggregateScope(s *scope, call ir.AggregateSpec, position int) (*scope, error) {
	reads := make(map[model.SlotID]struct{})
	remaining := uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands)
	for _, argument := range call.Args {
		if err := b.aggregateReads(argument, reads, 0, &remaining); err != nil {
			return nil, err
		}
	}
	if err := b.aggregateReads(call.Filter, reads, 0, &remaining); err != nil {
		return nil, err
	}
	for _, key := range call.Order {
		if err := b.aggregateReads(key.Expr, reads, 0, &remaining); err != nil {
			return nil, err
		}
	}
	owner := s
	found := false
	for current := s; current != nil; current = current.parent {
		if current.lateralForbidden {
			continue
		}
		for slot := range reads {
			if b.slotScopes[slot] == current.id {
				owner, found = current, true
				break
			}
		}
		if found {
			break
		}
	}
	if len(reads) != 0 && !found {
		return nil, bindError("XX000", "aggregate input has no enclosing scope", position)
	}
	if !owner.allowAggregate || owner.inAggregate {
		return nil, bindError("42803", "aggregate function is not allowed in this scope", position)
	}
	for slot := range reads {
		if aggregateOutput(owner, slot) {
			return nil, bindError("42803", "aggregate calls cannot be nested", position)
		}
	}
	directReads := make(map[model.SlotID]struct{})
	for _, argument := range call.Direct {
		if err := b.aggregateReads(argument, directReads, 0, &remaining); err != nil {
			return nil, err
		}
	}
	for current := s; current != nil; current = current.parent {
		if current.lateralForbidden {
			continue
		}
		for slot := range directReads {
			if b.slotScopes[slot] != current.id {
				continue
			}
			if current.id != owner.id {
				return nil, bindError("42803", "outer-level aggregate cannot contain a lower-level variable in its direct arguments", position)
			}
			if aggregateOutput(current, slot) {
				return nil, bindError("42803", "aggregate calls cannot be nested", position)
			}
		}
		if current.id == owner.id {
			break
		}
	}
	return owner, nil
}

func aggregateOutput(s *scope, slot model.SlotID) bool {
	for _, aggregate := range s.aggregates {
		if aggregate.Result == slot {
			return true
		}
	}
	for _, grouping := range s.groupings {
		if grouping.Result == slot {
			return true
		}
	}
	return false
}

// A scalar subquery contributes only its captured slots. Its local variables
// belong to the child query and cannot choose the surrounding aggregate level.
func (b *binder) aggregateReads(id ir.ScalarID, reads map[model.SlotID]struct{}, depth uint32, remaining *uint64) error {
	if id == 0 {
		return nil
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if depth >= b.options.Limits.MaxDepth || *remaining == 0 {
		return bindError("54000", "aggregate binding resource limit exceeded", 0)
	}
	*remaining -= 1
	scalar, ok := b.builder.Scalar(id)
	if !ok {
		return bindError("XX000", "aggregate input scalar is unavailable", 0)
	}
	if scalar.Kind == ir.Slot {
		reads[scalar.Slot] = struct{}{}
	}
	if scalar.Kind == ir.Subquery {
		relation, ok := b.builder.Rel(scalar.Relation)
		if !ok {
			return bindError("XX000", "aggregate input subquery is unavailable", 0)
		}
		for _, slot := range b.captures[relation.Scope] {
			reads[slot] = struct{}{}
		}
	}
	for index := range scalar.OperandCount() {
		operand, ok := b.builder.Operand(id, index)
		if !ok {
			return bindError("XX000", "aggregate input operand is unavailable", 0)
		}
		if err := b.aggregateReads(operand, reads, depth+1, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (b *binder) promoteAggregate(mark *ir.ScopeMark, source, owner *scope, call ir.AggregateSpec, position int) error {
	if source.id == owner.id {
		return nil
	}
	// Promotion changes both argument phases' owner. Only per-row arguments,
	// FILTER and ORDER BY read the original inputs; direct arguments keep the
	// grouped outputs and their grouping-set nullability.
	replacements := make(map[model.SlotID]model.SlotID, len(owner.groupSlots))
	for input, grouped := range owner.groupSlots {
		replacements[grouped.Slot] = input
	}
	rowRoots := make([]ir.ScalarID, 0, len(call.Args)+len(call.Order)+1)
	rowRoots = append(rowRoots, call.Args...)
	if call.Filter != 0 {
		rowRoots = append(rowRoots, call.Filter)
	}
	for _, key := range call.Order {
		rowRoots = append(rowRoots, key.Expr)
	}
	scopes, err := b.builder.PromoteScope(b.ctx, mark, owner.id, replacements, call.Direct, rowRoots)
	if errors.Is(err, ir.ErrScopeDependency) {
		return bindError("0A000", "outer-level aggregate cannot use a nested CTE", position)
	}
	if err != nil {
		return err
	}
	for _, id := range scopes {
		captures := b.captures[id][:0]
		for index := range b.builder.CaptureCount(id) {
			slot, ok := b.builder.Capture(id, index)
			if !ok {
				return bindError("XX000", "promoted aggregate capture is unavailable", position)
			}
			captures = append(captures, slot)
		}
		b.captures[id] = captures
	}
	return nil
}
