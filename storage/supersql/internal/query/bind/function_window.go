package bind

import (
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const defaultWindowFrameOptions int32 = 1058

type windowDefinition struct {
	spec *zpgparse.WindowDef
}

type boundWindow struct {
	spec        ir.RelSpec
	column      column
	reads       []model.SlotID
	keyColumns  []column
	keyBindings []ir.Binding
}

func resolveWindowDefinitions(nodes []*zpgparse.Node) (map[string]windowDefinition, error) {
	definitions := make(map[string]windowDefinition, len(nodes))
	for _, node := range nodes {
		window := node.GetWindowDef()
		if window == nil {
			return nil, bindError("42601", "invalid window definition", 0)
		}
		position := int(window.Location) + 1
		if _, exists := definitions[window.Name]; exists {
			return nil, bindError("42P20", fmt.Sprintf("window %q is already defined", window.Name), position)
		}
		resolved, err := extendWindowDefinition(window, definitions, position)
		if err != nil {
			return nil, err
		}
		definitions[window.Name] = windowDefinition{spec: resolved}
	}
	return definitions, nil
}

func resolveWindowReference(window *zpgparse.WindowDef, definitions map[string]windowDefinition, position int) (*zpgparse.WindowDef, error) {
	if window.Name != "" {
		definition, exists := definitions[window.Name]
		if !exists {
			return nil, bindError("42704", fmt.Sprintf("window %q does not exist", window.Name), position)
		}
		return definition.spec, nil
	}
	return extendWindowDefinition(window, definitions, position)
}

func extendWindowDefinition(window *zpgparse.WindowDef, definitions map[string]windowDefinition, position int) (*zpgparse.WindowDef, error) {
	if window.Refname == "" {
		return window, nil
	}
	base, exists := definitions[window.Refname]
	if !exists {
		return nil, bindError("42704", fmt.Sprintf("window %q does not exist", window.Refname), position)
	}
	if len(window.PartitionClause) != 0 {
		return nil, bindError("42P20", fmt.Sprintf("cannot override PARTITION BY clause of window %q", window.Refname), position)
	}
	if len(window.OrderClause) != 0 && len(base.spec.OrderClause) != 0 {
		return nil, bindError("42P20", fmt.Sprintf("cannot override ORDER BY clause of window %q", window.Refname), position)
	}
	if base.spec.FrameOptions != defaultWindowFrameOptions {
		return nil, bindError("42P20", fmt.Sprintf("cannot copy window %q because it has a frame clause", window.Refname), position)
	}
	resolved := proto.Clone(window).(*zpgparse.WindowDef)
	resolved.Refname = ""
	resolved.PartitionClause = base.spec.PartitionClause
	if len(resolved.OrderClause) == 0 {
		resolved.OrderClause = base.spec.OrderClause
	}
	return resolved, nil
}

func (b *binder) windowFunction(n *zpgparse.FuncCall, s *scope, sig builtin.Signature, args []expression) (expression, error) {
	return b.bindWindowedCall(n, s, &sig, nil, args)
}

// declaredWindowIdentity is the resolved call identity of a SQL-declared
// aggregate used as a window function. A declared aggregate has no pinned
// signature, so its result, argument targets, strictness, and volatility come
// from the declaration.
type declaredWindowIdentity struct {
	name     string
	function model.FunctionID
	strict   bool
	result   model.TypeLayout
	targets  []model.TypeLayout
	effects  ir.Effects
}

// bindWindowedCall binds one call in window context. Exactly one of pinned and
// declared identifies the call: pinned for the inventory, declared for a SQL
// aggregate. Both reach the same bounded frame machinery, and because the frame
// is replayed from its start there is no requirement that the aggregate declare
// an inverse transition.
func (b *binder) bindWindowedCall(n *zpgparse.FuncCall, s *scope, pinned *builtin.Signature, declared *declaredWindowIdentity, args []expression) (expression, error) {
	pos := int(n.Location) + 1
	if !s.allowWindow || s.inWindow {
		return expression{}, bindError("42P20", "window function is not allowed in this scope", pos)
	}
	if n.Over == nil {
		return expression{}, bindError("42809", "window function requires OVER", pos)
	}
	if pinned != nil && pinned.Kind != 'a' && pinned.Kind != 'w' {
		return expression{}, bindError("42809", "OVER requires a window or aggregate function", pos)
	}
	if pinned == nil && declared == nil {
		return expression{}, bindError("42809", "OVER requires a window or aggregate function", pos)
	}
	if n.AggDistinct {
		return expression{}, bindError("0A000", "DISTINCT is not implemented for window functions", pos)
	}
	if len(n.AggOrder) > 0 {
		return expression{}, bindError("0A000", "aggregate ORDER BY is not implemented for window functions", pos)
	}
	if n.AggWithinGroup {
		return expression{}, bindError("0A000", "ordered-set aggregates cannot be used as window functions", pos)
	}
	if n.AggFilter != nil && pinned != nil && pinned.Kind != 'a' {
		return expression{}, bindError("0A000", "FILTER is not implemented for non-aggregate window functions", pos)
	}
	window, err := resolveWindowReference(n.Over, s.windowDefinitions, pos)
	if err != nil {
		return expression{}, err
	}
	// Exactly one identity is present, so the call's name, function, result,
	// argument targets, and strictness are read from whichever was supplied.
	var (
		result           model.TypeLayout
		targets          []model.TypeLayout
		effects          ir.Effects
		name             string
		function         model.FunctionID
		strict, nullable bool
	)
	if declared != nil {
		result, targets = declared.result, declared.targets
		effects, name, function = declared.effects, declared.name, declared.function
		strict = declared.strict
	}
	if pinned != nil {
		result, err = b.functionResult(*pinned, args)
		if err != nil {
			return expression{}, err
		}
		if result.Type.Storage == model.TypeAny {
			return expression{}, missing("window result layout", pos)
		}
		declaredTargets, targetErr := b.functionArguments(*pinned, args, pos)
		if targetErr != nil {
			return expression{}, targetErr
		}
		// functionArguments fills exactly the call's arguments and returns a fixed
		// eight-slot array, so it is sliced to the arity actually present rather
		// than to the array's full width.
		targets = declaredTargets[:len(args)]
		effects = signatureEffects(*pinned)
		name, function = pinned.Name, pinned.ID
		strict = pinned.Strict
		nullable = functionNullable(*pinned)
	} else {
		nullable = true
	}
	if len(targets) != len(args) {
		return expression{}, bindError("42883", "function does not exist", pos)
	}
	if result.Type.Storage == model.TypeAny {
		return expression{}, missing("window result layout", pos)
	}
	types := make([]model.TypeLayout, len(args))
	ids := make([]ir.ScalarID, len(args))
	var reads []model.SlotID
	for i := range args {
		arg, err := b.coerce(s, args[i], targets[i], ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		args[i] = arg
		types[i] = arg.typ
		ids[i] = arg.id
		effects = effects.Union(arg.effects)
		reads = append(reads, arg.reads...)
	}
	var filterID ir.ScalarID
	if n.AggFilter != nil {
		filterScope := *s
		filterScope.allowAggregate = false
		filterScope.inWindow = true
		filter, err := b.scalar(n.AggFilter, &filterScope)
		if err != nil {
			return expression{}, err
		}
		filter, err = b.coerce(&filterScope, filter, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		filterID = filter.id
		effects = effects.Union(filter.effects)
		reads = append(reads, filter.reads...)
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, err
	}
	result, resultState := b.resultCollation(result, state)
	parameters := make([]ir.LayoutID, len(types))
	for i, typ := range types {
		parameters[i], err = b.layout(typ)
		if err != nil {
			return expression{}, err
		}
	}
	resultLayout, err := b.layout(result)
	if err != nil {
		return expression{}, err
	}
	for _, arg := range args {
		nullable = nullable || strict && arg.nullable
	}
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{
		Signature:  ir.Signature{Function: function, Result: resultLayout, InputCollation: state.id, Nullable: nullable, Strict: strict, Effects: effects},
		Parameters: parameters,
	})
	if err != nil {
		return expression{}, err
	}
	slot, err := b.slot(s, result, nullable, 0, 0)
	if err != nil {
		return expression{}, err
	}
	call := ir.WindowSpec{WindowCall: ir.WindowCall{Signature: signature, Function: function, Result: slot, Filter: filterID, Effects: effects}, Args: ids}
	spec := ir.RelSpec{Rel: ir.Rel{Kind: ir.Window, Scope: s.id, Effects: effects}, Windows: []ir.WindowSpec{call}}
	bound := boundWindow{}
	previous := s.inWindow
	s.inWindow = true
	defer func() { s.inWindow = previous }()
	for _, node := range window.PartitionClause {
		e, err := b.windowKeyScalar(node, s)
		if err != nil {
			return expression{}, err
		}
		e, err = b.defaultType(s, e)
		if err != nil {
			return expression{}, err
		}
		spec.Effects = spec.Effects.Union(e.effects)
		reads = append(reads, e.reads...)
		e, err = b.windowKey(s, &bound, node, e)
		if err != nil {
			return expression{}, err
		}
		spec.Partition = append(spec.Partition, e.id)
	}
	var rangeOrder orderKeyBinding
	for _, node := range window.OrderClause {
		sort := node.GetSortBy()
		if sort == nil {
			return expression{}, bindError("42601", "invalid window ordering", pos)
		}
		e, err := b.windowKeyScalar(sort.Node, s)
		if err != nil {
			return expression{}, err
		}
		e, err = b.defaultType(s, e)
		if err != nil {
			return expression{}, err
		}
		order, err := b.bindOrderKey(sort, e)
		if err != nil {
			return expression{}, err
		}
		if e.typ.Type.ID != order.input {
			e, err = b.coerce(s, e, b.primitive(order.input), ir.ImplicitCast)
			if err != nil {
				return expression{}, err
			}
		}
		order.layout = e.typ
		spec.Effects = spec.Effects.Union(e.effects)
		reads = append(reads, e.reads...)
		e, err = b.windowKey(s, &bound, sort.Node, e)
		if err != nil {
			return expression{}, err
		}
		order.key.Expr = e.id
		rangeOrder = order
		spec.Order = append(spec.Order, order.key)
	}
	frame := ir.Frame{Mode: ir.RangeFrame, Start: ir.FrameBound{Kind: ir.UnboundedPreceding}, End: ir.FrameBound{Kind: ir.CurrentRow}}
	flags := window.FrameOptions
	if flags&4 != 0 {
		frame.Mode = ir.RowsFrame
	}
	if flags&8 != 0 {
		frame.Mode = ir.GroupsFrame
	}
	for _, item := range []struct {
		mask int32
		kind ir.BoundKind
	}{{32, ir.UnboundedPreceding}, {512, ir.CurrentRow}, {2048, ir.OffsetPreceding}, {8192, ir.OffsetFollowing}} {
		if flags&item.mask != 0 {
			frame.Start.Kind = item.kind
		}
		if flags&(item.mask<<1) != 0 {
			frame.End.Kind = item.kind
		}
	}
	if flags&256 != 0 {
		frame.End.Kind = ir.UnboundedFollowing
	}
	if flags&32768 != 0 {
		frame.Exclusion = ir.ExcludeCurrent
	}
	if flags&65536 != 0 {
		frame.Exclusion = ir.ExcludeGroup
	}
	if flags&131072 != 0 {
		frame.Exclusion = ir.ExcludeTies
	}
	if frame.Mode == ir.RangeFrame && (window.StartOffset != nil || window.EndOffset != nil) && len(spec.Order) != 1 {
		return expression{}, bindError("42P20", "RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column", pos)
	}
	for _, pair := range []struct {
		node  *zpgparse.Node
		bound *ir.FrameBound
	}{{window.StartOffset, &frame.Start}, {window.EndOffset, &frame.End}} {
		if pair.node == nil {
			continue
		}
		isolated := *s
		isolated.columns = nil
		isolated.groupNodes = nil
		isolated.groupSlots = nil
		isolated.allowAggregate = false
		isolated.allowWindow = false
		e, err := b.scalar(pair.node, &isolated)
		if err != nil {
			return expression{}, err
		}
		if frame.Mode == ir.RangeFrame {
			e, pair.bound.InRange, err = b.bindRangeOffset(&isolated, e, rangeOrder)
		} else {
			e, err = b.coerce(&isolated, e, b.primitive(20), ir.ImplicitCast)
		}
		if err != nil {
			return expression{}, err
		}
		pair.bound.Offset = e.id
		spec.Effects = spec.Effects.Union(e.effects)
		if pair.bound.InRange != 0 {
			signature, _ := b.builder.Signature(pair.bound.InRange)
			spec.Effects = spec.Effects.Union(signature.Effects)
		}
		spec.Effects.MayError = true
	}
	if frame.Mode == ir.GroupsFrame && len(spec.Order) == 0 {
		return expression{}, bindError("42P20", "GROUPS mode requires an ORDER BY clause", pos)
	}
	spec.Frame = frame
	c := column{ResultColumn: ResultColumn{Name: name, Layout: result, Nullable: nullable, Slot: slot}}
	bound.spec, bound.column, bound.reads = spec, c, reads
	s.windows = append(s.windows, bound)
	expr, err := b.reference(s, c, pos)
	expr.collation = resultState
	return expr, err
}

func (b *binder) bindRangeOffset(s *scope, offset expression, order orderKeyBinding) (expression, ir.SignatureID, error) {
	nodeType := b.argumentID(offset)
	preferred := nodeType
	if preferred == 0 || preferred == 705 {
		preferred = order.input
	}
	functions, matches := 0, 0
	var selected builtin.InRangeSupport
	key := expression{typ: b.primitive(order.input), position: offset.position}
	boolean := expression{typ: b.primitive(16), position: offset.position}
	arguments := [5]expression{key, key, offset, boolean, boolean}
	for support := range builtin.InRangeSupportFunctions(order.family, order.input) {
		functions++
		signature, exists := builtin.LookupSignature(support.Function)
		if !exists {
			return expression{}, 0, bindError("XX000", "in_range support signature is missing", offset.position)
		}
		if b.overloadScore(signature, arguments[:]) < 0 {
			continue
		}
		matches++
		if selected.Offset != preferred {
			selected = support
		}
	}
	columnType := windowTypeName(order.input)
	offsetType := windowTypeName(nodeType)
	if functions == 0 {
		message := fmt.Sprintf("RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s", columnType)
		return expression{}, 0, bindError("0A000", message, offset.position)
	}
	if matches == 0 {
		message := fmt.Sprintf("RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s and offset type %s", columnType, offsetType)
		return expression{}, 0, bindError("0A000", message, offset.position)
	}
	if matches != 1 && selected.Offset != preferred {
		message := fmt.Sprintf("RANGE with offset PRECEDING/FOLLOWING has multiple interpretations for column type %s and offset type %s", columnType, offsetType)
		return expression{}, 0, bindError("0A000", message, offset.position)
	}
	coerced, err := b.coerce(s, offset, b.primitive(selected.Offset), ir.ImplicitCast)
	if err != nil {
		return expression{}, 0, err
	}
	metadata, _ := builtin.LookupSignature(selected.Function)
	types := []model.TypeLayout{order.layout, order.layout, coerced.typ, b.primitive(16), b.primitive(16)}
	signature, err := b.signature(metadata, 0, types, b.primitive(16), order.key.Collation, false)
	return coerced, signature, err
}

func windowTypeName(id model.TypeID) string {
	if id == 0 || id == 705 {
		return "unknown"
	}
	if typ, exists := builtin.LookupType(id); exists {
		return typ.Name
	}
	return fmt.Sprintf("%d", id)
}

func (b *binder) windowKey(s *scope, window *boundWindow, node *zpgparse.Node, value expression) (expression, error) {
	if err := b.requireKeyCollation(value); err != nil {
		return expression{}, err
	}
	if ref := node.GetColumnRef(); ref != nil && len(ref.Fields) <= 2 {
		scalar, exists := b.builder.Scalar(value.id)
		if exists && scalar.Kind == ir.Slot {
			return value, nil
		}
	}
	slot, err := b.slot(s, value.typ, value.nullable, 0, 0)
	if err != nil {
		return expression{}, err
	}
	c := column{ResultColumn: ResultColumn{Layout: value.typ, Nullable: value.nullable, Slot: slot}, hidden: true}
	window.keyColumns = append(window.keyColumns, c)
	window.keyBindings = append(window.keyBindings, ir.Binding{Slot: slot, Expr: value.id})
	return b.reference(s, c, value.position)
}

func (b *binder) projectWindow(input relation, s *scope, window *boundWindow) (relation, error) {
	values := make([]expression, len(window.keyBindings))
	for i, binding := range window.keyBindings {
		scalar, ok := b.builder.Scalar(binding.Expr)
		if !ok {
			return input, bindError("XX000", "window key expression is missing", 0)
		}
		values[i] = expression{id: binding.Expr, typ: window.keyColumns[i].Layout, nullable: window.keyColumns[i].Nullable, effects: scalar.Effects}
	}
	lowered, rewritten, _, err := b.projectSetExpressions(input, s, values)
	if err != nil {
		return input, err
	}
	input = lowered
	for i := range rewritten {
		window.keyBindings[i].Expr = rewritten[i].id
	}
	if len(window.keyBindings) == 0 {
		return input, nil
	}
	columns := append(slices.Clone(input.columns), window.keyColumns...)
	bindings := make([]ir.Binding, len(input.columns), len(columns))
	for i, c := range input.columns {
		// Unavailable catalog columns pass through by slot, exactly as in
		// projectSetLowering.stage: referencing them would report the feature for
		// statements that never name them, while every Project output must stay bound.
		if c.unsupported != "" {
			direct, err := b.passThrough(s, c)
			if err != nil {
				return input, err
			}
			bindings[i] = ir.Binding{Slot: c.Slot, Expr: direct}
			continue
		}
		reference, err := b.reference(s, c, 0)
		if err != nil {
			return input, err
		}
		bindings[i] = ir.Binding{Slot: c.Slot, Expr: reference.id}
	}
	bindings = append(bindings, window.keyBindings...)
	effects := input.effects.Union(window.spec.Effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: effects, namespaces: input.namespaces}, err
}
