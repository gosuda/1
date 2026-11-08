package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (g *Graph) compatibleOutputs(a, b extent) bool {
	if a.count != b.count {
		return false
	}
	for i, slot := range sliceAt(g.slotRefs, a) {
		left, right := g.slots[slot-1], g.slots[g.slotRefs[int(b.start)+i]-1]
		if !g.sameLayout(left.Layout, right.Layout) || (right.Nullable && !left.Nullable) {
			return false
		}
	}
	return true
}
func (g *Graph) inputHas(n Rel, slot model.SlotID) bool {
	runtimeImage := slot == n.OldRow || slot == n.NewRow || slot == n.ExcludedRow || slot == n.MergeResultRow || slot == n.MergeActionSlot || slot == n.OldTableOID || slot == n.NewTableOID || slot == n.MergeTableOID
	if n.Kind == Modify && runtimeImage {
		return true
	}
	if n.Kind == Modify {
		for _, step := range sliceAt(g.viewMutationSteps, n.viewMutationSteps) {
			if contains(sliceAt(g.slotRefs, step.inputs), slot) {
				return true
			}
		}
	}
	for _, id := range sliceAt(g.relRefs, n.inputs) {
		if contains(sliceAt(g.slotRefs, g.rels[id-1].outputs), slot) {
			return true
		}
	}
	return contains(sliceAt(g.slotRefs, g.scopes[n.Scope-1].captures), slot)
}

func (g *Graph) scalarReturnsSet(n Scalar) bool {
	if n.Kind != Call || !validID(n.Signature, g.signatures) {
		return false
	}
	return g.signatures[n.Signature-1].SetReturning
}
func (g *Graph) expressionAvailable(ctx context.Context, n Rel, id ScalarID, depth uint32, limits Limits) error {
	remaining := uint64(limits.MaxNodes) + uint64(limits.MaxOperands)
	return g.expressionAvailableWithin(ctx, n, id, depth, limits, &remaining)
}
func (g *Graph) expressionAvailableWithin(ctx context.Context, n Rel, id ScalarID, depth uint32, limits Limits, remaining *uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= limits.MaxDepth || *remaining == 0 {
		return limitError()
	}
	*remaining -= 1
	if !validID(id, g.scalars) || g.scalars[id-1].Scope != n.Scope {
		return invalid("invalid relational expression scope or handle")
	}
	s := g.scalars[id-1]
	if g.scalarReturnsSet(s) && (n.Kind != ProjectSet || depth != 0) {
		return invalid("set-returning call must be a direct ProjectSet binding")
	}
	if s.Kind == Slot && !g.inputHas(n, s.Slot) {
		return invalid("expression reads slot not provided by inputs or captures")
	}
	if s.Kind == Subquery {
		for _, slot := range sliceAt(g.slotRefs, g.scopes[g.rels[s.Relation-1].Scope-1].captures) {
			if !g.inputHas(n, slot) {
				return invalid("subquery captures unavailable slot")
			}
		}
	}
	for _, child := range sliceAt(g.scalarRefs, s.operands) {
		if err := g.expressionAvailableWithin(ctx, n, child, depth+1, limits, remaining); err != nil {
			return err
		}
	}
	return nil
}
func (g *Graph) validateOrder(ctx context.Context, n Rel, order extent, limits Limits) error {
	if !validExtent(order, g.orders) {
		return invalid("invalid ordering range")
	}
	for _, key := range sliceAt(g.orders, order) {
		if err := g.expressionAvailable(ctx, n, key.Expr, 0, limits); err != nil {
			return err
		}
		invalidDirection := key.Direction < Ascending || key.Direction > Descending
		invalidNullOrder := key.Nulls < NullsFirst || key.Nulls > NullsLast
		if invalidDirection || invalidNullOrder || key.Operator == 0 || key.Collation != g.layouts[g.scalars[key.Expr-1].Layout-1].typ.Collation {
			return invalid("invalid resolved ordering key")
		}
	}
	return nil
}
func (g *Graph) validateRel(ctx context.Context, n Rel, limits Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.Kind < Scan || n.Kind > Sample || !validID(n.Scope, g.scopes) || n.Span.Start > n.Span.End || n.Span.End > limits.SourceBytes {
		return invalid("invalid relation metadata")
	}
	if !g.validRelPayloadRanges(&n) {
		return invalid("invalid relation payload range")
	}
	if !validExtent(n.assignments, g.bindings) || !validExtent(n.conflictAssignments, g.bindings) || !validExtent(n.conflictArbiters, g.conflictArbiters) {
		return invalid("invalid assignment payload range")
	}
	inputs := sliceAt(g.relRefs, n.inputs)
	outputs := sliceAt(g.slotRefs, n.outputs)
	for i, id := range inputs {
		if !validID(id, g.rels) {
			return invalid("forward relation handle")
		}
		input := g.rels[id-1]
		if n.Kind == Apply && i == 1 {
			if g.scopes[input.Scope-1].parent != n.Scope {
				return invalid("Apply right input requires a child scope")
			}
			for _, capture := range sliceAt(g.slotRefs, g.scopes[input.Scope-1].captures) {
				if !contains(sliceAt(g.slotRefs, g.rels[inputs[0]-1].outputs), capture) && !contains(sliceAt(g.slotRefs, g.scopes[n.Scope-1].captures), capture) {
					return invalid("Apply captures unavailable slot")
				}
			}
		} else if input.Scope != n.Scope {
			return invalid("cross-scope relation input")
		}
	}
	for i, slot := range outputs {
		if !validID(slot, g.slots) || g.slots[slot-1].Scope != n.Scope || contains(outputs[:i], slot) {
			return invalid("invalid or duplicate output slot")
		}
	}
	for _, id := range sliceAt(g.scalarRefs, n.expressions) {
		if err := g.expressionAvailable(ctx, n, id, 0, limits); err != nil {
			return err
		}
	}
	for _, id := range sliceAt(g.scalarRefs, n.partition) {
		if err := g.expressionAvailable(ctx, n, id, 0, limits); err != nil {
			return err
		}
	}
	for _, id := range [...]ScalarID{n.Predicate, n.Offset, n.Count, n.ConflictPredicate, n.Recursion.CycleValue, n.Recursion.CycleDefault, n.Recursion.CycleContinue, n.Frame.Start.Offset, n.Frame.End.Offset} {
		if id != 0 {
			if err := g.expressionAvailable(ctx, n, id, 0, limits); err != nil {
				return err
			}
		}
	}
	if n.Predicate != 0 && !g.boolean(n.Predicate) {
		return invalid("predicate is not boolean")
	}
	if err := g.validateOrder(ctx, n, n.order, limits); err != nil {
		return err
	}
	bindings := sliceAt(g.bindings, n.bindings)
	for i, binding := range bindings {
		if !contains(outputs, binding.Slot) {
			return invalid("binding destination is not an output")
		}
		for _, previous := range bindings[:i] {
			if previous.Slot == binding.Slot {
				return invalid("duplicate output binding")
			}
		}
		if err := g.expressionAvailable(ctx, n, binding.Expr, 0, limits); err != nil {
			return err
		}
		slot, expr := g.slots[binding.Slot-1], g.scalars[binding.Expr-1]
		if !g.sameLayout(slot.Layout, expr.Layout) || (expr.Nullable && !slot.Nullable) {
			return invalid("output binding type mismatch")
		}
		if g.inputHas(n, binding.Slot) && (expr.Kind != Slot || expr.Slot != binding.Slot) {
			return invalid("binding redefines an input slot; declare a new output slot")
		}
	}
	arity := 1
	switch n.Kind {
	case Scan, CatalogScan, Values, GenerateSeries, GenerateSubscripts, Unnest, CTEConsumer:
		arity = 0
	case Join, Apply, SetOp, RecursiveUnion:
		arity = 2
	case RowsFrom:
		if len(inputs) < 2 {
			return invalid("ROWS FROM requires at least two inputs")
		}
		arity = len(inputs)
	}
	if len(inputs) != arity {
		return invalid("invalid relation arity")
	}
	objectIdentity := n.Object != 0 || n.DescriptorVersion != 0 || n.Generation != 0 || n.EncodingVersion != 0
	if n.Kind != Scan && n.Kind != Modify && objectIdentity {
		return invalid("object identity on non-object relation")
	}
	if n.Kind != CatalogScan && n.Catalog != 0 {
		return invalid("catalog identity on another relation")
	}
	if n.Kind == CatalogScan && n.Catalog == 0 {
		return invalid("catalog scan needs a logical relation identity")
	}
	if n.Kind != Values && n.Rows != 0 {
		return invalid("VALUES row count on another relation")
	}
	if n.UnnestOrdinality && n.Kind != Unnest {
		return invalid("unnest ordinality on another relation")
	}
	if n.UnnestRange != (model.Type{}) && n.Kind != Unnest {
		return invalid("unnest range relationship on another relation")
	}
	jsonTablePayload := n.JSONTableRoot != 0 || n.JSONTableArguments != 0 || n.JSONTablePassing != 0 || n.JSONTableFormat != 0 || n.JSONTableEncoding != 0 || n.JSONTableFlags != 0 || n.jsonTableColumns.count != 0
	if jsonTablePayload && (n.Kind != Unnest || n.JSONTableRoot == 0 || n.RowFunction != 0 || n.RowResult == 0 || n.UnnestRange != (model.Type{}) || n.UnnestOrdinality) {
		return invalid("invalid JSON_TABLE row source relationship")
	}
	if (n.RowFunction != 0 || n.RowResult != 0 || n.RowCollation != 0 || n.RowStrict || n.RowVariadicArray) && (n.Kind != Unnest || n.RowResult == 0 || n.UnnestRange != (model.Type{}) || n.RowFunction == 0 && n.JSONTableRoot == 0) {
		return invalid("invalid row source relationship")
	}
	if n.RowsFromOrdinality && n.Kind != RowsFrom {
		return invalid("ROWS FROM ordinality on another relation")
	}
	modificationPayload := n.assignments.count != 0 || n.conflictAssignments.count != 0 || n.ExcludedRow != 0 || n.OldRow != 0 || n.NewRow != 0 || n.SourceObject != 0 || n.OldTableOID != 0 || n.NewTableOID != 0 || n.MergeTableOID != 0 || n.targetKeys.count != 0 || n.updatedColumns.count != 0 || n.MergeSourcePresent != 0 || n.MergeTargetPresent != 0 || n.MergeResultRow != 0 || n.MergeActionSlot != 0
	if n.Kind != Modify && modificationPayload {
		return invalid("modification payload on another relation")
	}
	unexpectedLateral := n.LateralOuter && (n.Kind != Apply || n.ApplyKind != LateralApply)
	unexpectedMark := n.MarkKind != 0 && (n.Kind != Join || n.JoinKind != MarkJoin)
	if unexpectedLateral || unexpectedMark {
		return invalid("unexpected lateral or mark semantics")
	}
	unexpectedMarkSlot := n.MarkSlot != 0 && n.Kind != Apply
	if n.Kind != Join && (n.JoinKind != 0 || unexpectedMarkSlot) {
		return invalid("join payload on another relation")
	}
	if n.hasForeignOperatorPayload() {
		return invalid("relation payload belongs to another kind")
	}
	if n.hasForeignStatefulPayload() {
		return invalid("stateful payload belongs to another kind")
	}
	cteKind := n.Kind == CTEProducer || n.Kind == CTEConsumer || n.Kind == RecursiveUnion
	if !cteKind && (n.CTE != 0 || n.RecursiveReference) {
		return invalid("CTE payload on another relation")
	}
	generator := n.Kind == GenerateSeries || n.Kind == GenerateSubscripts || n.Kind == Unnest
	expressionList := generator || n.Kind == Values || n.Kind == Aggregate || n.Kind == Distinct
	if !expressionList && n.expressions.count != 0 {
		return invalid("unexpected expression list")
	}
	if n.Kind != Project && n.Kind != ProjectSet && n.Kind != Aggregate && n.Kind != Window && n.Kind != Modify && n.bindings.count != 0 {
		return invalid("unexpected output bindings")
	}
	if n.Kind != Sort && n.Kind != Window && n.Kind != Distinct && n.Kind != Limit && n.order.count != 0 {
		return invalid("unexpected ordering")
	}
	if n.Kind != Filter && n.Kind != Join && n.Predicate != 0 {
		return invalid("unexpected predicate")
	}
	if n.Kind != SetOp && n.Kind != RecursiveUnion && n.All {
		return invalid("unexpected ALL")
	}
	switch n.Kind {
	case Scan:
		if n.Object == 0 || n.DescriptorVersion == 0 || n.Generation == 0 {
			return invalid("scan needs versioned object identity")
		}
		for _, slot := range outputs {
			info := g.slots[slot-1]
			if info.Object != n.Object || info.Column == 0 {
				return invalid("scan output lacks column identity")
			}
		}
	case CatalogScan:
		for _, slot := range outputs {
			info := g.slots[slot-1]
			if info.Object != 0 || info.Column == 0 {
				return invalid("catalog scan output lacks logical column identity")
			}
		}
	case Values:
		if n.Rows > uint64(limits.MaxOperands) {
			return limitError()
		}
		if len(outputs) == 0 {
			if n.expressions.count != 0 {
				return invalid("zero-column VALUES has cells")
			}
		} else if n.Rows > uint64(n.expressions.count)/uint64(len(outputs)) || n.Rows*uint64(len(outputs)) != uint64(n.expressions.count) {
			return invalid("VALUES row count or width mismatch")
		}
		for i, id := range sliceAt(g.scalarRefs, n.expressions) {
			slot := g.slots[outputs[i%len(outputs)]-1]
			expr := g.scalars[id-1]
			if !g.sameLayout(slot.Layout, expr.Layout) || (expr.Nullable && !slot.Nullable) {
				return invalid("VALUES cell type mismatch")
			}
		}
	case GenerateSeries:
		if err := g.validateSeries(n, outputs); err != nil {
			return err
		}
	case GenerateSubscripts:
		if err := g.validateSubscripts(n, outputs); err != nil {
			return err
		}
	case Unnest:
		if err := g.validateUnnest(n, outputs); err != nil {
			return err
		}
	case RowsFrom:
		if err := g.validateRowsFrom(ctx, n, outputs); err != nil {
			return err
		}
	case Filter:
		if n.Predicate == 0 {
			return invalid("filter needs predicate")
		}
	case Project, ProjectSet:
		if len(bindings) != len(outputs) {
			return invalid("projection must bind every output")
		}
		if n.Kind == ProjectSet {
			hasSet := false
			for _, binding := range bindings {
				hasSet = hasSet || g.scalarReturnsSet(g.scalars[binding.Expr-1])
			}
			if !hasSet {
				return invalid("ProjectSet needs a set-returning binding")
			}
		}
	case Join, Apply:
		if err := g.validateJoin(n); err != nil {
			return err
		}
	case Aggregate:
		if err := g.validateAggregate(ctx, n, limits); err != nil {
			return err
		}
	case Window:
		if err := g.validateWindow(ctx, n, limits); err != nil {
			return err
		}
	case Sort:
		if n.order.count == 0 {
			return invalid("sort needs keys")
		}
	case Limit:
		missingBounds := n.Count == 0 && n.Offset == 0
		invalidTies := n.WithTies && (n.Count == 0 || n.order.count == 0)
		if missingBounds || invalidTies {
			return invalid("invalid limit or FETCH WITH TIES")
		}
		for _, id := range [...]ScalarID{n.Count, n.Offset} {
			if id != 0 && g.layouts[g.scalars[id-1].Layout-1].typ.Storage != model.TypeInt64 {
				return invalid("limit expression must be integral")
			}
		}
	case Distinct:
		if n.DistinctOn != (n.expressions.count > 0) {
			return invalid("DISTINCT ON needs keys; DISTINCT uses all outputs")
		}
		if n.DistinctOn && n.order.count > 0 {
			if n.order.count < n.expressions.count {
				return invalid("DISTINCT ON order prefix too short")
			}
			for i, id := range sliceAt(g.scalarRefs, n.expressions) {
				if g.orders[int(n.order.start)+i].Expr != id {
					return invalid("DISTINCT ON does not match order prefix")
				}
			}
		}
	case SetOp, RecursiveUnion:
		if n.Kind == SetOp && (n.SetKind < Union || n.SetKind > Except) {
			return invalid("invalid set operation")
		}
		if n.Kind == SetOp && (!g.compatibleOutputs(n.outputs, g.rels[inputs[0]-1].outputs) || !g.compatibleOutputs(n.outputs, g.rels[inputs[1]-1].outputs)) {
			return invalid("set-operation schema mismatch")
		}
		if n.Kind == RecursiveUnion {
			if err := g.validateRecursion(ctx, n, limits); err != nil {
				return err
			}
		}
	case CTEProducer, CTEConsumer:
		if !validID(n.CTE, g.ctes) {
			return invalid("invalid CTE reference or schema")
		}
		cte := g.ctes[n.CTE-1]
		invalidBackedge := n.RecursiveReference && (n.Kind != CTEConsumer || !cte.Recursive)
		if !g.ancestor(cte.Scope, n.Scope) || !g.compatibleOutputs(n.outputs, cte.outputs) || invalidBackedge {
			return invalid("invalid CTE reference or schema")
		}
	case Sample:
		expressions := sliceAt(g.scalarRefs, n.expressions)
		if n.SampleMethod < BernoulliSample || n.SampleMethod > SystemSample || len(expressions) != 1 && len(expressions) != 2 || n.SampleRepeatable != (len(expressions) == 2) {
			return invalid("invalid sample specification")
		}
		for _, expression := range expressions {
			scalar := g.scalars[expression-1]
			if g.layouts[scalar.Layout-1].typ.Storage != model.TypeFloat64 {
				return invalid("sample expression is not float8")
			}
		}
	case LockRows:
		if n.locks.count == 0 {
			return invalid("LockRows needs locks")
		}
		for _, lock := range sliceAt(g.locks, n.locks) {
			slots := [...]model.SlotID{lock.ObjectSlot, lock.DescriptorVersionSlot, lock.GenerationSlot, lock.EncodingVersionSlot, lock.RowSlot}
			for _, slot := range slots {
				if !g.inputHas(n, slot) {
					return invalid("invalid row lock input")
				}
			}
			row := g.slots[lock.RowSlot-1]
			layout := g.layouts[row.Layout-1]
			count := layout.children.count
			if lock.Object == 0 || lock.Occurrence == 0 || count == 0 || count != lock.keys.count || !validExtent(lock.keys, g.lockKeys) || layout.typ.Storage != model.TypeComposite || lock.Strength < KeyShare || lock.Strength > UpdateLock || lock.Wait > SkipLocked {
				return invalid("invalid row lock")
			}
			for _, slot := range slots[:4] {
				info := g.slots[slot-1]
				if g.layouts[info.Layout-1].typ.Storage != model.TypeInt64 || info.Nullable {
					return invalid("invalid row lock identity")
				}
			}
		}
	case Modify:
		if err := g.validateModify(ctx, n, limits); err != nil {
			return err
		}
	}
	if n.SecurityBarrier && n.Kind != Project {
		return invalid("security barrier must be a projection")
	}
	switch n.Kind {
	case Filter, Sort, Limit, Distinct, Sample, LockRows, CTEProducer:
		if !equalSlots(outputs, sliceAt(g.slotRefs, g.rels[inputs[0]-1].outputs)) {
			return invalid("pass-through relation changed output slots")
		}
	}
	if n.Effects.Volatility > Volatile || n.Effects.Union(g.relationEffects(n)) != n.Effects {
		return invalid("relation effects omit dependency effects")
	}
	return nil
}
func equalSlots(a, b []model.SlotID) bool {
	if len(a) != len(b) {
		return false
	}
	for i, id := range a {
		if id != b[i] {
			return false
		}
	}
	return true
}
func (g *Graph) validateJoin(n Rel) error {
	inputs := sliceAt(g.relRefs, n.inputs)
	left, right := g.rels[inputs[0]-1], g.rels[inputs[1]-1]
	leftSlots, rightSlots := sliceAt(g.slotRefs, left.outputs), sliceAt(g.slotRefs, right.outputs)
	for _, slot := range leftSlots {
		if contains(rightSlots, slot) {
			return invalid("join inputs share slot identities; remap one input")
		}
	}
	outputs := sliceAt(g.slotRefs, n.outputs)
	width := len(leftSlots) + len(rightSlots)
	mark := false
	if n.Kind == Join {
		unexpectedPredicate := n.JoinKind == CrossJoin && n.Predicate != 0
		missingPredicate := n.JoinKind != CrossJoin && n.Predicate == 0
		if n.JoinKind < InnerJoin || n.JoinKind > MarkJoin || unexpectedPredicate || missingPredicate {
			return invalid("invalid join kind or condition")
		}
		if n.JoinKind == SemiJoin || n.JoinKind == AntiJoin {
			width = len(leftSlots)
		}
		if n.JoinKind == MarkJoin {
			width = len(leftSlots) + 1
			mark = true
		}
		if n.JoinKind == MarkJoin && (n.MarkKind < ExistsSubquery || n.MarkKind > AllSubquery) {
			return invalid("mark join needs EXISTS/ANY/ALL semantics")
		}
	} else {
		if n.ApplyKind < ScalarApply || n.ApplyKind > LateralApply {
			return invalid("invalid Apply kind")
		}
		if n.ApplyKind == ScalarApply && len(rightSlots) != 1 {
			return invalid("scalar Apply needs one right output")
		}
		if n.ApplyKind == ExistsApply {
			width = len(leftSlots) + 1
			mark = true
		}
	}
	if width != len(outputs) {
		return invalid("join output width mismatch")
	}
	for i, id := range outputs {
		out := g.slots[id-1]
		if mark && i == len(leftSlots) {
			if !g.validJoinMark(&n, id) {
				return invalid("invalid join mark slot")
			}
			continue
		}
		var source model.SlotID
		nullable := false
		if i < len(leftSlots) {
			source = leftSlots[i]
			nullable = n.Kind == Join && (n.JoinKind == RightJoin || n.JoinKind == FullJoin)
		} else {
			source = rightSlots[i-len(leftSlots)]
			nullable = n.Kind == Join && (n.JoinKind == LeftJoin || n.JoinKind == FullJoin) || n.Kind == Apply && (n.ApplyKind == ScalarApply || n.LateralOuter)
		}
		in := g.slots[source-1]
		omitsNullability := (in.Nullable || nullable) && !out.Nullable
		if !g.sameLayout(in.Layout, out.Layout) || omitsNullability {
			return invalid("join output type or null extension mismatch")
		}
	}
	if !mark && n.MarkSlot != 0 {
		return invalid("unexpected mark slot")
	}
	return nil
}

func (g *Graph) validRelPayloadRanges(n *Rel) bool {
	if !validExtent(n.inputs, g.relRefs) || !validExtent(n.expressions, g.scalarRefs) || !validExtent(n.outputs, g.slotRefs) || !validExtent(n.targetKeys, g.slotRefs) || !validExtent(n.updatedColumns, g.columnRefs) || !validExtent(n.bindings, g.bindings) || !validExtent(n.assignments, g.bindings) || !validExtent(n.conflictAssignments, g.bindings) || !validExtent(n.conflictArbiters, g.conflictArbiters) {
		return false
	}
	if !validExtent(n.order, g.orders) || !validExtent(n.groups, g.groups) || !validExtent(n.aggregates, g.aggregates) || !validExtent(n.groupings, g.groupings) || !validExtent(n.windows, g.windows) {
		return false
	}
	if !validExtent(n.partition, g.scalarRefs) || !validExtent(n.locks, g.locks) || !validExtent(n.actions, g.actions) || !validExtent(n.jsonTableColumns, g.jsonTableColumns) || !validExtent(n.viewMutationSteps, g.viewMutationSteps) {
		return false
	}
	for _, step := range sliceAt(g.viewMutationSteps, n.viewMutationSteps) {
		if !validExtent(step.inputs, g.slotRefs) || !validExtent(step.projections, g.scalarRefs) || !validExtent(step.outputColumns, g.columnRefs) {
			return false
		}
	}
	return validExtent(n.Recursion.searchKeys, g.slotRefs) && validExtent(n.Recursion.cycleKeys, g.slotRefs)
}

func (n *Rel) hasForeignOperatorPayload() bool {
	if n.Kind != Apply && n.ApplyKind != 0 {
		return true
	}
	if n.Kind != SetOp && n.SetKind != 0 {
		return true
	}
	if n.Kind != Distinct && n.DistinctOn {
		return true
	}
	if n.Kind != Window && n.Frame != (Frame{}) {
		return true
	}
	limitPayload := n.Offset != 0 || n.Count != 0 || n.WithTies
	if n.Kind != Limit && limitPayload {
		return true
	}
	modifyPayload := n.ModifyKind != 0 || n.ModifyTarget != 0 || n.Conflict != 0 || n.conflictArbiters.count != 0 || n.ConflictPredicate != 0 || n.actions.count != 0 || n.targetKeys.count != 0 || n.updatedColumns.count != 0 || n.assignments.count != 0 || n.conflictAssignments.count != 0 || n.OldRow != 0 || n.NewRow != 0 || n.ExcludedRow != 0 || n.SourceObject != 0 || n.OldTableOID != 0 || n.NewTableOID != 0 || n.LogicalView != 0 || n.MergeSourcePresent != 0 || n.MergeTargetPresent != 0 || n.MergeResultRow != 0 || n.MergeActionSlot != 0 || n.MergeTableOID != 0 || n.viewMutationSteps.count != 0
	return n.Kind != Modify && modifyPayload
}

func (n *Rel) hasForeignStatefulPayload() bool {
	aggregatePayload := n.groups.count != 0 || n.aggregates.count != 0 || n.groupings.count != 0
	if n.Kind != Aggregate && aggregatePayload {
		return true
	}
	if n.Kind != Window && (n.windows.count != 0 || n.partition.count != 0) {
		return true
	}
	if n.Kind != LockRows && n.locks.count != 0 {
		return true
	}
	return n.Kind != RecursiveUnion && n.Recursion.hasPayload()
}

func (r *Recursion) hasPayload() bool {
	search := r.Search != NoSearch || r.SearchSlot != 0 || r.searchKeys.count != 0
	cycleOutputs := r.CycleMark != 0 || r.CyclePath != 0
	cycleValues := r.CycleValue != 0 || r.CycleDefault != 0 || r.CycleContinue != 0 || r.cycleKeys.count != 0
	return search || cycleOutputs || cycleValues
}

func (g *Graph) validJoinMark(n *Rel, id model.SlotID) bool {
	out := g.slots[id-1]
	if id != n.MarkSlot || g.layouts[out.Layout-1].typ.Storage != model.TypeBoolean {
		return false
	}
	nonNullableMark := n.Kind == Apply || n.MarkKind == ExistsSubquery
	if nonNullableMark && out.Nullable {
		return false
	}
	nullableMark := n.Kind == Join && n.MarkKind != ExistsSubquery
	return !nullableMark || out.Nullable
}
