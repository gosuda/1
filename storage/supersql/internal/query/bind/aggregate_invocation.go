package bind

import (
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// userAggregate resolves a call to a SQL-defined aggregate in the search path.
// Aggregates share PostgreSQL's pg_proc namespace with routines, so the routine
// resolver skips them; this finds the descriptor whose Aggregate payload matches
// the call and produces the same aggregate call shape the builtin path uses.
func (b *binder) userAggregate(schema, name string, n *zpgparse.FuncCall, call callArguments, s *scope, pos, directCount int) (expression, bool, error) {
	if b.catalog == nil || n == nil {
		return expression{}, false, nil
	}
	namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
	if isMissingObject(err) {
		return expression{}, false, nil
	}
	if err != nil {
		return expression{}, false, err
	}
	// PostgreSQL does not accept aggregate syntax on a routine, and the reverse
	// is equally true, so the call's shape decides which form is admissible.
	var selected *model.Descriptor
	var metadata *model.AggregateMetadata
	ambiguous := false
	err = b.catalog.ScanNamespace(b.ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(p catalog.PublishedDescriptor) error {
		if p.Descriptor.Name().Name != name {
			return nil
		}
		spec, specErr := b.descriptorSpec(p.Descriptor)
		if specErr != nil {
			return specErr
		}
		if spec.Aggregate == nil {
			return nil
		}
		if !aggregateCallMatches(spec.Aggregate, n, call, directCount) {
			return nil
		}
		if selected != nil {
			ambiguous = true
		}
		selected, metadata = p.Descriptor, spec.Aggregate
		return nil
	})
	if err != nil {
		return expression{}, false, err
	}
	if ambiguous {
		return expression{}, false, bindError("42725", "function call is ambiguous", pos)
	}
	if selected == nil {
		return expression{}, false, nil
	}
	b.dependency(namespace.Descriptor, 0)
	b.dependency(selected, 0)
	// The declaration's support routines execute through the procedural resolver,
	// which can only run a routine the invocation's definition set contains. The
	// definition set is seeded from the statement's dependencies, so the
	// transition and final routines must be recorded here or a user-written
	// aggregate would bind but fail to execute. Builtin support routines need no
	// edge: they resolve from the pinned inventory.
	recordAggregateSupportDependency(b, metadata.Transition)
	recordAggregateSupportDependency(b, metadata.Final)
	// A window call uses the surrounding scope's frames, so it is bound through
	// the window machinery rather than the grouping path. PostgreSQL replays the
	// frame from its start, so a window aggregate needs no inverse transition.
	if n.Over != nil {
		expr, err := b.declaredWindowAggregate(n, s, name, selected, metadata, call, pos)
		return expr, true, err
	}
	if !s.allowAggregate || s.inAggregate {
		return expression{}, false, bindError("42803", "aggregate function is not allowed in this scope", pos)
	}
	inputs := make([]model.TypeLayout, len(metadata.Arguments))
	copy(inputs, metadata.Arguments)
	args := call.values
	if len(args) != len(inputs) {
		return expression{}, false, bindError("42883", "function does not exist", pos)
	}
	// Each argument is coerced to the declared type, exactly as the builtin path
	// does, so a declared aggregate participates in implicit coercion the same way.
	for i := range args {
		coerced, coerceErr := b.coerce(s, args[i], inputs[i], ir.ImplicitCast)
		if coerceErr != nil {
			return expression{}, false, coerceErr
		}
		args[i] = coerced
	}
	types := make([]model.TypeLayout, len(args))
	ids := make([]ir.ScalarID, len(args))
	effects := ir.Effects{MayError: true}
	for i, arg := range args {
		types[i] = arg.typ
		ids[i] = arg.id
		effects = effects.Union(arg.effects)
	}
	switch aggregateVolatility(metadata.Volatility) {
	case ir.Stable:
		effects.Volatility = ir.Stable
	case ir.Volatile:
		effects.Volatility = ir.Volatile
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, false, err
	}
	result := metadata.Result
	if result.Type.ID == 0 {
		return expression{}, false, missing("aggregate result layout", pos)
	}
	if result.Type.Storage == model.TypeAny && len(inputs) != 0 {
		result = inputs[0]
	}
	result, resultState := b.resultCollation(result, state)
	if n.AggDistinct {
		for _, arg := range args {
			if err := b.requireKeyCollation(arg); err != nil {
				return expression{}, false, err
			}
		}
	}
	parameters := make([]ir.LayoutID, len(types))
	for i, typ := range types {
		parameters[i], err = b.layout(typ)
		if err != nil {
			return expression{}, false, err
		}
	}
	resultLayout, err := b.layout(result)
	if err != nil {
		return expression{}, false, err
	}
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{
		Signature:  ir.Signature{Function: model.FunctionID(selected.OID()), Result: resultLayout, InputCollation: state.id, Nullable: true, Effects: effects},
		Parameters: parameters,
	})
	if err != nil {
		return expression{}, false, err
	}
	aggregate := ir.AggregateSpec{
		AggregateCall: ir.AggregateCall{Signature: signature, Function: model.FunctionID(selected.OID()), Distinct: n.AggDistinct, Effects: effects},
		Direct:        ids[:directCount],
		Args:          ids[directCount:],
	}
	// The aggregate body may not itself contain aggregate or window calls, and it
	// evaluates in the surrounding scope, which is why the flags are saved here.
	previousAggregate, previousWindow, previousInput := s.allowAggregate, s.allowWindow, s.inAggregate
	s.allowAggregate, s.allowWindow, s.inAggregate = false, false, true
	defer func() {
		s.allowAggregate, s.allowWindow, s.inAggregate = previousAggregate, previousWindow, previousInput
	}()
	if n.AggFilter != nil {
		filter, filterErr := b.scalar(n.AggFilter, s)
		if filterErr != nil {
			return expression{}, false, filterErr
		}
		filter, filterErr = b.coerce(s, filter, b.primitive(16), ir.ImplicitCast)
		if filterErr != nil {
			return expression{}, false, filterErr
		}
		aggregate.Filter = filter.id
		aggregate.Effects = aggregate.Effects.Union(filter.effects)
	}
	// An ordered-set declaration's ordering is the declared order of its
	// aggregated arguments, so it needs no separate ORDER BY resolution.
	var order []ir.OrderKey
	var orderEffects ir.Effects
	if metadata.Kind == model.AggregateOrdinary {
		order, orderEffects, err = b.aggregateOrder(n, s, args)
	} else {
		order, orderEffects, err = b.orderedSetOrder(n, args[directCount:])
	}
	if err != nil {
		return expression{}, false, err
	}
	aggregate.Order = order
	aggregate.Effects = aggregate.Effects.Union(orderEffects)
	slot, err := b.slot(s, result, true, 0, 0)
	if err != nil {
		return expression{}, false, err
	}
	aggregate.Result = slot
	c := column{ResultColumn: ResultColumn{Name: name, Layout: result, Nullable: true, Slot: slot}}
	s.aggregates = append(s.aggregates, aggregate)
	s.aggregateColumns = append(s.aggregateColumns, c)
	s.aggregateSeen = true
	expr, err := b.reference(s, c, pos)
	expr.collation = resultState
	return expr, true, err
}

// declaredWindowAggregate binds a SQL-declared aggregate in window context.
// A window aggregate's arguments are its declared aggregated arguments; the
// declaration's direct arguments and ORDER BY belong to the within-group form,
// which PostgreSQL rejects under OVER.
func (b *binder) declaredWindowAggregate(n *zpgparse.FuncCall, s *scope, name string, descriptor *model.Descriptor, metadata *model.AggregateMetadata, call callArguments, pos int) (expression, error) {
	if metadata.Kind != model.AggregateOrdinary {
		// Rejected by PostgreSQL itself, so this reuses the same wording the
		// pinned path produces for the identical condition.
		return expression{}, bindError("0A000", "ordered-set aggregates cannot be used as window functions", pos)
	}
	if n.AggDistinct {
		return expression{}, bindError("0A000", "DISTINCT is not implemented for window functions", pos)
	}
	if n.AggWithinGroup || len(n.AggOrder) != 0 {
		return expression{}, bindError("0A000", "aggregate ORDER BY is not implemented for window functions", pos)
	}
	result := metadata.Result
	if result.Type.ID == 0 {
		return expression{}, missing("window result layout", pos)
	}
	if result.Type.Storage == model.TypeAny && len(metadata.Arguments) != 0 {
		result = metadata.Arguments[0]
	}
	identity := declaredWindowIdentity{
		name:     name,
		function: model.FunctionID(descriptor.OID()),
		result:   result,
		targets:  metadata.Arguments,
		effects:  ir.Effects{MayError: true},
	}
	switch aggregateVolatility(metadata.Volatility) {
	case ir.Stable:
		identity.effects.Volatility = ir.Stable
	case ir.Volatile:
		identity.effects.Volatility = ir.Volatile
	}
	if strict, known := b.declaredAggregateStrictness(descriptor); known {
		identity.strict = strict
	}
	return b.bindWindowedCall(n, s, nil, &identity, call.values)
}

// recordAggregateSupportDependency records a declaration's support routine as a
// binding dependency when it is a user routine. The scheduler loads the
// invocation's procedural definitions from exactly this dependency set, so a
// user-written transition or final function is executable only if the call site
// recorded it. A pinned builtin is deliberately skipped: its OID resolves from
// the builtin inventory, and recording it would add a catalog edge to an object
// that has no descriptor.
func recordAggregateSupportDependency(b *binder, id model.FunctionID) {
	if id == 0 {
		return
	}
	if _, pinned := b.lookupSignature(id); pinned {
		return
	}
	published, err := resolveDefinitionFunctionDescriptor(b, id)
	if err != nil || published.Descriptor == nil {
		return
	}
	b.dependency(published.Descriptor, 0)
}

// declaredAggregateStrictness reports the declared transition function's
// strictness, which decides whether NULL inputs reach the aggregate.
func (b *binder) declaredAggregateStrictness(descriptor *model.Descriptor) (bool, bool) {
	metadata, ok := descriptor.Aggregate()
	if !ok || metadata.Transition == 0 {
		return false, false
	}
	if signature, pinned := b.lookupSignature(metadata.Transition); pinned {
		return signature.Strict, true
	}
	published, err := resolveDefinitionFunctionDescriptor(b, metadata.Transition)
	if err != nil || published.Descriptor == nil {
		return false, false
	}
	spec, err := b.descriptorSpec(published.Descriptor)
	if err != nil || spec.Routine == nil {
		return false, false
	}
	b.dependency(published.Descriptor, 0)
	return spec.Routine.Strict, true
}

// aggregateCallMatches reports whether a declared aggregate accepts this call.
// A WITHIN GROUP call carries the direct arguments followed by the ORDER BY
// arguments, which is exactly how the declaration stores them; an ordinary call
// carries its arguments alone and may add ORDER BY as a sort specification over
// those same arguments rather than as extra arguments.
func aggregateCallMatches(metadata *model.AggregateMetadata, n *zpgparse.FuncCall, call callArguments, directCount int) bool {
	if n.AggStar {
		// A zero-argument aggregate is keyed by PostgreSQL's "*" spelling.
		return len(metadata.Arguments) == 0 && directCount == 0
	}
	if len(call.values) != len(metadata.Arguments) {
		return false
	}
	if n.AggWithinGroup {
		ordered := metadata.Kind == model.AggregateOrderedSet || metadata.Kind == model.AggregateHypotheticalSet
		if !ordered || directCount != int(metadata.DirectArguments) {
			return false
		}
		if len(n.AggOrder) != len(metadata.Arguments)-int(metadata.DirectArguments) {
			return false
		}
	} else if metadata.Kind != model.AggregateOrdinary || directCount != 0 {
		return false
	}
	for i, value := range call.values {
		if value.typ.Type.ID != 0 && !sameType(value.typ, metadata.Arguments[i]) {
			return false
		}
	}
	return true
}

// aggregateVolatility maps the persisted volatility spelling to its IR effect.
func aggregateVolatility(volatility string) ir.Volatility {
	switch volatility {
	case "stable":
		return ir.Stable
	case "volatile":
		return ir.Volatile
	default:
		return ir.Immutable
	}
}
