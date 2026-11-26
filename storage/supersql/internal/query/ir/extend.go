package ir

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Extend owns a copy of graph's arenas and constants, preserving every existing
// handle. Appended nodes may reference those handles; Build chooses the new root.
// The source remains immutable. The supplied limits apply to the complete result.
func Extend(ctx context.Context, graph *Graph, limits Limits) (*Builder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if graph == nil || !validID(graph.root, graph.rels) {
		return nil, invalid("invalid extension graph")
	}
	b, err := NewBuilder(limits)
	if err != nil {
		return nil, err
	}
	nodes := uint64(len(graph.scalars)) + uint64(len(graph.rels)) + uint64(len(graph.layouts)) + uint64(len(graph.scopes)) + uint64(len(graph.ctes)) + uint64(len(graph.signatures))
	if nodes > uint64(limits.MaxNodes) {
		return nil, limitError()
	}
	b.graph = Graph{
		root:       graph.root,
		scalars:    extensionArena(ctx, b, graph.scalars, limits.MaxNodes),
		rels:       extensionArena(ctx, b, graph.rels, limits.MaxNodes),
		slots:      extensionArena(ctx, b, graph.slots, limits.MaxSlots),
		scopes:     extensionArena(ctx, b, graph.scopes, limits.MaxNodes),
		layouts:    extensionArena(ctx, b, graph.layouts, limits.MaxNodes),
		ctes:       extensionArena(ctx, b, graph.ctes, limits.MaxNodes),
		signatures: extensionArena(ctx, b, graph.signatures, limits.MaxNodes),
		values:     extensionArena(ctx, b, graph.values, limits.MaxNodes),
		scalarRefs: extensionArena(ctx, b, graph.scalarRefs, limits.MaxOperands),
		comparisonRefs: extensionArena(ctx, b, graph.comparisonRefs, limits.MaxOperands),
		relRefs:    extensionArena(ctx, b, graph.relRefs, limits.MaxOperands),
		slotRefs:   extensionArena(ctx, b, graph.slotRefs, limits.MaxOperands),
		layoutRefs: extensionArena(ctx, b, graph.layoutRefs, limits.MaxOperands),
		columnRefs: extensionArena(ctx, b, graph.columnRefs, limits.MaxOperands),
		fieldNames: extensionArena(ctx, b, graph.fieldNames, limits.MaxOperands),
		bindings:   extensionArena(ctx, b, graph.bindings, limits.MaxOperands),
		conflictArbiters: extensionArena(ctx, b, graph.conflictArbiters, limits.MaxOperands),
		orders:     extensionArena(ctx, b, graph.orders, limits.MaxOperands),
		groups:     extensionArena(ctx, b, graph.groups, limits.MaxOperands),
		indices:    extensionArena(ctx, b, graph.indices, limits.MaxOperands),
		aggregates: extensionArena(ctx, b, graph.aggregates, limits.MaxOperands),
		groupings:  extensionArena(ctx, b, graph.groupings, limits.MaxOperands),
		windows:    extensionArena(ctx, b, graph.windows, limits.MaxOperands),
		locks:      extensionArena(ctx, b, graph.locks, limits.MaxOperands),
		lockKeys:   extensionArena(ctx, b, graph.lockKeys, limits.MaxOperands),
		actions:    extensionArena(ctx, b, graph.actions, limits.MaxOperands),
		jsonTableColumns: extensionArena(ctx, b, graph.jsonTableColumns, limits.MaxOperands),
		viewMutationSteps: extensionArena(ctx, b, graph.viewMutationSteps, limits.MaxOperands),
	}
	if b.err != nil {
		return nil, b.err
	}
	for i, name := range graph.fieldNames {
		if err := b.check(ctx); err != nil {
			return nil, err
		}
		if err := b.charge(uint64(len(name))); err != nil {
			return nil, err
		}
		b.graph.fieldNames[i] = strings.Clone(name)
	}
	for i, value := range graph.values {
		owned, used, err := model.CopyValue(ctx, value, limits.MaxBytes-b.bytes)
		if err == model.ErrValueBudget || err == model.ErrValueDepth {
			return nil, limitError()
		}
		if err != nil {
			return nil, err
		}
		if err := b.charge(used); err != nil {
			return nil, err
		}
		b.graph.values[i] = owned
	}
	if err := b.verifyGraph(ctx, graph.root); err != nil {
		return nil, err
	}
	return b, nil
}

func extensionArena[T any](ctx context.Context, b *Builder, source []T, maximum uint32) []T {
	if err := b.check(ctx); err != nil {
		b.err = err
		return nil
	}
	var result []T
	if _, err := appendArena(b, &result, source, maximum); err != nil {
		b.err = err
		return nil
	}
	return result
}
