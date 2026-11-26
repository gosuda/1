package opt

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (o *optimizer) column(slot model.SlotID) (statistics.Column, bool) {
	info, ok := o.graph.Slot(slot)
	if !ok || info.Object == 0 || info.Column == 0 {
		return statistics.Column{}, false
	}
	for id := ir.RelID(1); id < ir.RelID(len(o.access)); id++ {
		if !o.step(1) {
			return statistics.Column{}, false
		}
		table := o.access[id].statistics
		if table == nil || table.Target.ObjectID != info.Object {
			continue
		}
		for _, column := range table.Columns {
			if !o.step(1) {
				return statistics.Column{}, false
			}
			if column.ID == info.Column {
				return column, true
			}
		}
	}
	return statistics.Column{}, false
}

func (o *optimizer) tableStatistics(object model.ObjectID) *statistics.Table {
	for id := ir.RelID(1); id < ir.RelID(len(o.access)); id++ {
		if table := o.access[id].statistics; table != nil && table.Target.ObjectID == object {
			return table
		}
	}
	return nil
}

type extendedEqualityTerm struct {
	child    ir.ScalarID
	column   model.ColumnID
	constant ir.ScalarID
}

func (o *optimizer) extendedEqualitySelectivity(id ir.ScalarID, depth uint32) (float64, bool) {
	if o.options.ExtendedStatistics == nil || o.options.StatisticsKeyEncoder == nil || o.options.StatisticsLimits.Validate() != nil {
		return 0, false
	}
	scalar, _ := o.graph.Scalar(id)
	terms := allocate[extendedEqualityTerm](o, uint64(scalar.OperandCount()))
	if o.err != nil {
		return 0, false
	}
	terms = terms[:0]
	var object model.ObjectID
	for i := range scalar.OperandCount() {
		childID, _ := o.graph.Operand(id, i)
		child, _ := o.graph.Scalar(childID)
		if child.Kind != ir.Call || child.OperandCount() != 2 {
			return 0, false
		}
		operator, ok := builtin.LookupOperator(child.Operator)
		if !ok || operator.Name != "=" {
			return 0, false
		}
		leftID, _ := o.graph.Operand(childID, 0)
		rightID, _ := o.graph.Operand(childID, 1)
		left, _ := o.graph.Scalar(leftID)
		right, _ := o.graph.Scalar(rightID)
		slot, constant := left, right
		constantID := rightID
		if left.Kind == ir.Const && right.Kind == ir.Slot {
			slot, constant, constantID = right, left, leftID
		}
		if slot.Kind != ir.Slot || constant.Kind != ir.Const {
			return 0, false
		}
		info, ok := o.graph.Slot(slot.Slot)
		if !ok || info.Object == 0 || info.Column == 0 || object != 0 && object != info.Object {
			return 0, false
		}
		object = info.Object
		if slices.ContainsFunc(terms, func(term extendedEqualityTerm) bool { return term.column == info.Column }) {
			return 0, false
		}
		terms = append(terms, extendedEqualityTerm{child: childID, column: info.Column, constant: constantID})
	}
	if len(terms) < 2 {
		return 0, false
	}
	table := o.tableStatistics(object)
	if table == nil {
		return 0, false
	}
	definitions, found, err := o.options.ExtendedStatistics.Definitions(o.ctx, table.Target)
	if err != nil {
		o.err = err
		return 0, false
	}
	if !found {
		return 0, false
	}
	for _, definition := range definitions {
		if definition.ExpressionCount != 0 || len(definition.Keys) != len(terms) || !slices.Contains(definition.Kinds, byte('m')) && !slices.Contains(definition.Kinds, byte('d')) && !slices.Contains(definition.Kinds, byte('f')) {
			continue
		}
		ordered := allocate[extendedEqualityTerm](o, uint64(len(definition.Keys)))
		if o.err != nil {
			return 0, false
		}
		matches := true
		for i, key := range definition.Keys {
			position := slices.IndexFunc(terms, func(term extendedEqualityTerm) bool { return term.column == key })
			if position < 0 {
				matches = false
				break
			}
			ordered[i] = terms[position]
		}
		if !matches {
			continue
		}
		statisticAt, ok := slices.BinarySearchFunc(table.Extended, definition.OID, func(statistic statistics.ExtendedStatistic, oid model.OID) int {
			return cmp.Compare(statistic.OID, oid)
		})
		if !ok {
			continue
		}
		statistic := table.Extended[statisticAt]
		values := allocate[model.Value](o, uint64(len(ordered)))
		if o.err != nil {
			return 0, false
		}
		for i, term := range ordered {
			value, ok := o.statisticConstant(term.constant)
			if !ok {
				return 0, false
			}
			if value.IsNull {
				return 0, true
			}
			values[i] = value
		}
		if len(statistic.MCV) != 0 {
			limits := o.options.StatisticsLimits
			limits.MaxBytes = min(limits.MaxBytes, o.options.MaxBytes-o.bytes)
			list, err := statistics.DecodeMCVList(o.ctx, statistic.MCV, limits)
			if err != nil {
				o.err = err
				return 0, false
			}
			owned, err := statistics.MCVListBytes(o.ctx, list, limits)
			if err != nil {
				o.err = err
				return 0, false
			}
			if !o.charge(owned, 1) {
				return 0, false
			}
			if len(list.Types) != len(values) {
				o.err = statistics.ErrInvalid
				return 0, false
			}
			key, ok := o.extendedStatisticKey(list.Types, values)
			if !ok {
				return 0, false
			}
			remainingMass := 1.0
			for _, item := range list.Items {
				itemKey, ok := o.extendedStatisticKey(list.Types, item.Values)
				if !ok {
					return 0, false
				}
				if bytes.Equal(key, itemKey) {
					return item.Frequency, true
				}
				remainingMass = max(0, remainingMass-item.Frequency)
			}
			if len(statistic.NDistinct) != 0 {
				summary, err := statistics.DecodeNDistinct(o.ctx, statistic.NDistinct, o.options.StatisticsLimits)
				if err != nil {
					o.err = err
					return 0, false
				}
				return min(1, remainingMass/max(1, summary.Value-float64(len(list.Items)))), true
			}
		}
		if len(statistic.NDistinct) != 0 {
			summary, err := statistics.DecodeNDistinct(o.ctx, statistic.NDistinct, o.options.StatisticsLimits)
			if err != nil {
				o.err = err
				return 0, false
			}
			return min(1, 1/max(1, summary.Value)), true
		}
		if len(statistic.Dependencies) != 0 {
			dimensions, dependencies, err := statistics.DecodeDependencies(o.ctx, statistic.Dependencies, o.options.StatisticsLimits)
			if err != nil {
				o.err = err
				return 0, false
			}
			if dimensions != len(ordered) {
				o.err = statistics.ErrInvalid
				return 0, false
			}
			independent := 1.0
			termSelectivity := allocate[float64](o, uint64(len(ordered)))
			if o.err != nil {
				return 0, false
			}
			for i, term := range ordered {
				termSelectivity[i] = o.selectivity(term.child, depth+1)
				independent *= termSelectivity[i]
			}
			best := independent
			for _, dependency := range dependencies {
				determinant := 1.0
				for _, dimension := range dependency.Determinants {
					determinant *= termSelectivity[dimension]
				}
				best = max(best, independent*(1-dependency.Degree)+determinant*dependency.Degree)
			}
			return min(1, best), true
		}
	}
	return 0, false
}

func (o *optimizer) extendedStatisticKey(layouts []model.TypeLayout, values []model.Value) ([]byte, bool) {
	remaining := o.options.MaxBytes - o.bytes
	if remaining < 2 {
		o.err = ErrLimit
		return nil, false
	}
	keyLayout := allocate[storage.KeyColumn](o, uint64(len(layouts)))
	if o.err != nil {
		return nil, false
	}
	for i, layout := range layouts {
		keyLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children}
	}
	key, err := o.options.StatisticsKeyEncoder.EncodePrefix(o.ctx, keyLayout, values, remaining)
	if err != nil {
		o.err = err
		return nil, false
	}
	if !o.charge(uint64(cap(key)), 1) {
		return nil, false
	}
	return key, true
}

func (o *optimizer) statisticConstant(id ir.ScalarID) (model.Value, bool) {
	remaining := o.options.MaxBytes - o.bytes
	if remaining == 0 {
		o.err = ErrLimit
		return model.Value{}, false
	}
	value, err := o.graph.CloneConstant(o.ctx, id, remaining)
	if err != nil {
		o.err = err
		return model.Value{}, false
	}
	bytes, err := model.ValueBytes(o.ctx, value, remaining)
	if err != nil {
		o.err = err
		return model.Value{}, false
	}
	if !o.charge(bytes, 1) {
		return model.Value{}, false
	}
	return value, true
}
func (o *optimizer) selectivity(id ir.ScalarID, depth uint32) float64 {
	if id == 0 {
		return 1
	}
	if !o.step(1) {
		return 0
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return 0
	}
	s, _ := o.graph.Scalar(id)
		if s.BoolOp == ir.And {
			if estimate, known := o.extendedEqualitySelectivity(id, depth); known {
				return estimate
			}
		}
	if s.Kind == ir.Bool && (s.BoolOp == ir.And || s.BoolOp == ir.Or) {
		result := 1.0
		for i := range s.OperandCount() {
			child, _ := o.graph.Operand(id, i)
			selectivity := o.selectivity(child, depth+1)
			if s.BoolOp == ir.And {
				result *= selectivity
			} else {
				result *= 1 - selectivity
			}
		}
		if s.BoolOp == ir.Or {
			return 1 - result
		}
		return result
	}
	if s.Kind == ir.NullTest && s.OperandCount() == 1 {
		id, _ := o.graph.Operand(id, 0)
		input, _ := o.graph.Scalar(id)
		if input.Kind == ir.Slot {
			if column, ok := o.column(input.Slot); ok {
				if s.Negated {
					return 1 - column.NullFraction
				}
				return column.NullFraction
			}
		}
	}
	if s.Kind != ir.Call || s.OperandCount() != 2 {
		return 0.1
	}
	operator, ok := builtin.LookupOperator(s.Operator)
	if !ok {
		return 0.1
	}
	leftID, _ := o.graph.Operand(id, 0)
	rightID, _ := o.graph.Operand(id, 1)
	left, _ := o.graph.Scalar(leftID)
	right, _ := o.graph.Scalar(rightID)
	if left.Kind == ir.Slot && right.Kind == ir.Const {
		if estimate, known := o.constantSelectivity(left.Slot, rightID, operator.Name); known {
			return estimate
		}
	}
	if right.Kind == ir.Slot && left.Kind == ir.Const {
		name := operator.Name
		switch name {
		case "<":
			name = ">"
		case "<=":
			name = ">="
		case ">":
			name = "<"
		case ">=":
			name = "<="
		}
		if estimate, known := o.constantSelectivity(right.Slot, leftID, name); known {
			return estimate
		}
	}
	if !operator.HashJoinable && !operator.MergeJoinable {
		return 0.1
	}
	denominator, nonnull := 1.0, 1.0
	known := false
	for _, input := range [...]ir.Scalar{left, right} {
		if input.Kind == ir.Slot {
			if column, ok := o.column(input.Slot); ok && column.Distinct > 0 {
				denominator = max(denominator, column.Distinct)
				nonnull *= 1 - column.NullFraction
				known = true
			}
		}
	}
	if !known {
		return 0.1
	}
	return min(1, nonnull/denominator)
}

func (o *optimizer) constantSelectivity(slot model.SlotID, constantID ir.ScalarID, operator string) (float64, bool) {
	column, ok := o.column(slot)
	if !ok || column.Distinct <= 0 {
		return 0, false
	}
	value, ok := o.statisticConstant(constantID)
	if !ok {
		return 0, false
	}
	return o.statisticValueSelectivity(column, value, operator)
}

func (o *optimizer) statisticValueSelectivity(column statistics.Column, value model.Value, operator string) (float64, bool) {
	if value.IsNull {
		return 0, true
	}
	if value.Type != column.Layout.Type.Storage || column.Distinct <= 0 {
		return 0, false
	}
	switch operator {
	case "=", "<>":
		equality, known := o.equalitySelectivity(column, value)
		if !known {
			return 0, false
		}
		if operator == "<>" {
			return max(0, 1-column.NullFraction-equality), true
		}
		return equality, true
	case "<", "<=", ">", ">=":
		return o.rangeSelectivity(column, value, operator)
	default:
		return 0, false
	}
}

func (o *optimizer) statisticKey(column statistics.Column, value model.Value) ([]byte, bool) {
	if o.options.StatisticsKeyEncoder == nil {
		return nil, false
	}
	remaining := o.options.MaxBytes - o.bytes
	if remaining == 0 {
		o.err = ErrLimit
		return nil, false
	}
	layout := []storage.KeyColumn{{ColumnID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children}}
	key, err := o.options.StatisticsKeyEncoder.EncodePrefix(o.ctx, layout, []model.Value{value}, remaining)
	if err != nil {
		o.err = err
		return nil, false
	}
	if !o.charge(uint64(cap(key)), 1) {
		return nil, false
	}
	return key, true
}

func (o *optimizer) equalitySelectivity(column statistics.Column, value model.Value) (float64, bool) {
	key, ok := o.statisticKey(column, value)
	if !ok {
		return 0, false
	}
	remainingMass := max(0, 1-column.NullFraction)
	for _, common := range column.MostCommon {
		commonKey, ok := o.statisticKey(column, common.Value)
		if !ok {
			return 0, false
		}
		if bytes.Equal(key, commonKey) {
			return common.Fraction, true
		}
		remainingMass = max(0, remainingMass-common.Fraction)
	}
	remainingDistinct := max(1, column.Distinct-float64(len(column.MostCommon)))
	return min(1, remainingMass/remainingDistinct), true
}

func (o *optimizer) rangeSelectivity(column statistics.Column, value model.Value, operator string) (float64, bool) {
	if len(column.Histogram) == 0 {
		return 0, false
	}
	key, ok := o.statisticKey(column, value)
	if !ok {
		return 0, false
	}
	before, equal := 0, 0
	for _, bound := range column.Histogram {
		boundKey, ok := o.statisticKey(column, bound)
		if !ok {
			return 0, false
		}
		order := bytes.Compare(boundKey, key)
		if order < 0 {
			before++
		} else if order == 0 {
			equal++
		}
	}
	less := float64(before) / float64(len(column.Histogram))
	lessEqual := float64(before+equal) / float64(len(column.Histogram))
	nonnull := 1 - column.NullFraction
	switch operator {
	case "<":
		return nonnull * less, true
	case "<=":
		return nonnull * lessEqual, true
	case ">":
		return nonnull * (1 - lessEqual), true
	case ">=":
		return nonnull * (1 - less), true
	default:
		return 0, false
	}
}
func (o *optimizer) deriveKeys(id ir.RelID) [][]physical.Slot {
	r, _ := o.graph.Rel(id)
	g := &o.groups[id]
	var key []physical.Slot
	switch r.Kind {
	case ir.Distinct:
		if !r.DistinctOn {
			key = g.properties.output
		} else {
			key = allocate[physical.Slot](o, uint64(r.ExpressionCount()))
			if o.err != nil {
				return nil
			}
			for i := range key {
				scalar, _ := o.graph.Expression(id, uint32(i))
				expr, _ := o.graph.Scalar(scalar)
				if expr.Kind != ir.Slot {
					return nil
				}
				key[i] = o.slot(expr.Slot)
			}
		}
	case ir.Values:
		if r.Rows > 1 {
			return nil
		}
	case ir.Filter, ir.Sort, ir.Limit, ir.Sample, ir.LockRows:
		child, _ := o.graph.Input(id, 0)
		return o.groups[child].properties.keys
	case ir.Scan:
		key = o.primaryKeySlots(id, g.properties.output)
		if len(key) == 0 {
			return nil
		}
	default:
		return nil
	}
	for _, slot := range key {
		if !slices.Contains(g.properties.output, slot) {
			return nil
		}
	}
	keys := allocate[[]physical.Slot](o, 1)
	if o.err != nil {
		return nil
	}
	keys[0] = key
	return keys
}

func (o *optimizer) outputColumnSlot(outputs []physical.Slot, column model.ColumnID) physical.Slot {
	for _, output := range outputs {
		info, _ := o.graph.Slot(output.ID)
		if info.Column == column {
			return output
		}
	}
	return physical.Slot{}
}

func (o *optimizer) primaryKeySlots(id ir.RelID, outputs []physical.Slot) []physical.Slot {
	for _, path := range o.access[id].paths {
		if path.node.Algorithm != physical.PrimaryKeyScan {
			continue
		}
		key := allocate[physical.Slot](o, uint64(len(path.node.Scan.Request.KeyLayout)))
		if o.err != nil {
			return nil
		}
		for i, column := range path.node.Scan.Request.KeyLayout {
			key[i] = o.outputColumnSlot(outputs, path.node.Scan.LogicalColumn(column.ColumnID))
			if key[i].ID == 0 || key[i].Nullable {
				return nil
			}
		}
		return key
	}
	return nil
}
