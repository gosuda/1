package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) targetScalar(node *zpgparse.Node, s *scope) (expression, error) {
	allowed, restriction := b.allowTargetSRF, b.srfRestriction
	b.allowTargetSRF, b.srfRestriction = true, ""
	defer func() { b.allowTargetSRF, b.srfRestriction = allowed, restriction }()
	return b.scalar(node, s)
}

func (b *binder) windowKeyScalar(node *zpgparse.Node, s *scope) (expression, error) {
	previous := b.allowWindowKeySRF
	b.allowWindowKeySRF = true
	defer func() { b.allowWindowKeySRF = previous }()
	return b.targetScalar(node, s)
}

func (b *binder) restrictedScalar(node *zpgparse.Node, s *scope, restriction string) (expression, error) {
	allowed, previous := b.allowTargetSRF, b.srfRestriction
	b.allowTargetSRF, b.srfRestriction = false, restriction
	defer func() { b.allowTargetSRF, b.srfRestriction = allowed, previous }()
	return b.scalar(node, s)
}

func (b *binder) restrictSRFs(restriction string) func() {
	allowed, previous := b.allowTargetSRF, b.srfRestriction
	b.allowTargetSRF, b.srfRestriction = false, restriction
	return func() { b.allowTargetSRF, b.srfRestriction = allowed, previous }
}

func (b *binder) checkTargetSRF(s *scope, pos int) error {
	if s.inAggregate {
		return bindError("0A000", "aggregate function calls cannot contain set-returning function calls", pos)
	}
	if s.inWindow && !b.allowWindowKeySRF {
		return bindError("0A000", "window function calls cannot contain set-returning function calls", pos)
	}
	if b.srfRestriction != "" {
		return bindError("0A000", "set-returning functions are not allowed in "+b.srfRestriction, pos)
	}
	if !b.allowTargetSRF {
		return bindError("0A000", "set-returning functions are not allowed in this context", pos)
	}
	return nil
}

type projectSetLowering struct {
	b            *binder
	scope        *scope
	depth        map[ir.ScalarID]uint32
	visiting     map[ir.ScalarID]bool
	sources      [][]ir.ScalarID
	replacements map[ir.ScalarID]expression
	work         uint64
}

func (b *binder) projectSetExpressions(input relation, s *scope, values []expression) (relation, []expression, bool, error) {
	lowering := projectSetLowering{
		b:            b,
		scope:        s,
		depth:        make(map[ir.ScalarID]uint32),
		visiting:     make(map[ir.ScalarID]bool),
		replacements: make(map[ir.ScalarID]expression),
		work:         uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands),
	}
	for _, value := range values {
		if value.id == 0 {
			continue
		}
		if _, err := lowering.scalarDepth(value.id); err != nil {
			return input, nil, false, err
		}
	}
	if len(lowering.sources) == 0 {
		return input, values, false, nil
	}
	var err error
	for level := range lowering.sources {
		input, err = lowering.stage(input, lowering.sources[level])
		if err != nil {
			return input, nil, false, err
		}
	}
	result := slices.Clone(values)
	for i := range result {
		if result[i].id == 0 {
			continue
		}
		id, err := lowering.rewrite(result[i].id, 0)
		if err != nil {
			return input, nil, false, err
		}
		result[i].id = id
		if replacement, ok := lowering.replacements[values[i].id]; ok {
			result[i].sourceSlot = replacement.sourceSlot
			result[i].nullable = true
			result[i].reads = []model.SlotID{replacement.sourceSlot}
		}
	}
	return input, result, true, nil
}

func (p *projectSetLowering) step() error {
	if err := p.b.ctx.Err(); err != nil {
		return err
	}
	if p.work == 0 {
		return bindError("54000", "set-returning expression resource limit exceeded", 0)
	}
	p.work--
	return nil
}

func (p *projectSetLowering) scalarDepth(id ir.ScalarID) (uint32, error) {
	if depth, ok := p.depth[id]; ok {
		return depth, nil
	}
	if err := p.step(); err != nil {
		return 0, err
	}
	if p.visiting[id] {
		return 0, bindError("XX000", "cyclic scalar expression", 0)
	}
	n, ok := p.b.builder.Scalar(id)
	if !ok {
		return 0, bindError("XX000", "set-returning expression is missing", 0)
	}
	p.visiting[id] = true
	var depth uint32
	for i := range n.OperandCount() {
		child, ok := p.b.builder.Operand(id, i)
		if !ok {
			return 0, bindError("XX000", "set-returning operand is missing", 0)
		}
		childDepth, err := p.scalarDepth(child)
		if err != nil {
			return 0, err
		}
		depth = max(depth, childDepth)
	}
	delete(p.visiting, id)
	if p.returnsSet(n) {
		depth++
		for uint32(len(p.sources)) < depth {
			p.sources = append(p.sources, nil)
		}
		p.sources[depth-1] = append(p.sources[depth-1], id)
	}
	p.depth[id] = depth
	return depth, nil
}

func (p *projectSetLowering) returnsSet(n ir.Scalar) bool {
	if n.Kind != ir.Call {
		return false
	}
	signature, ok := p.b.builder.Signature(n.Signature)
	return ok && signature.SetReturning
}

func (p *projectSetLowering) stage(input relation, sources []ir.ScalarID) (relation, error) {
	columns := slices.Clone(input.columns)
	bindings := make([]ir.Binding, 0, len(columns)+len(sources))
	for _, c := range columns {
		// A catalog column that is unavailable in this profile is passed through by
		// slot rather than by reference. Referencing it would report the feature for
		// statements that never name it; binding it directly keeps every output
		// bound, which ProjectSet requires, while leaving it unreadable.
		if c.unsupported != "" {
			direct, err := p.b.passThrough(p.scope, c)
			if err != nil {
				return input, err
			}
			bindings = append(bindings, ir.Binding{Slot: c.Slot, Expr: direct})
			continue
		}
		reference, err := p.b.reference(p.scope, c, 0)
		if err != nil {
			return input, err
		}
		bindings = append(bindings, ir.Binding{Slot: c.Slot, Expr: reference.id})
	}
	effects := input.effects
	for _, source := range sources {
		n, ok := p.b.builder.Scalar(source)
		if !ok || !p.returnsSet(n) {
			return input, bindError("XX000", "ProjectSet source is not set-returning", 0)
		}
		direct, err := p.rewrite(source, source)
		if err != nil {
			return input, err
		}
		layout, err := p.layout(n.Layout, 0)
		if err != nil {
			return input, err
		}
		slot, err := p.b.slot(p.scope, layout, true, 0, 0)
		if err != nil {
			return input, err
		}
		c := column{ResultColumn: ResultColumn{Layout: layout, Nullable: true, Slot: slot}, hidden: true, internal: true}
		if layout.Type.Storage == model.TypeComposite {
			c.record = recordVariable
			if len(layout.Children) == len(layout.FieldNames) {
				c.record = recordDeclared
				c.recordFields = make([]column, len(layout.Children))
				for i := range layout.Children {
					c.recordFields[i] = column{ResultColumn: ResultColumn{Name: layout.FieldNames[i], Layout: layout.Children[i], Nullable: true}}
				}
			}
		}
		columns = append(columns, c)
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: direct})
		reference, err := p.b.reference(p.scope, c, 0)
		if err != nil {
			return input, err
		}
		p.replacements[source] = reference
		effects = effects.Union(n.Effects)
	}
	id, err := p.b.builder.AddRel(p.b.ctx, ir.RelSpec{
		Rel:      ir.Rel{Kind: ir.ProjectSet, Scope: p.scope.id, Effects: effects},
		Inputs:   []ir.RelID{input.id},
		Outputs:  outputSlots(columns),
		Bindings: bindings,
	})
	if err != nil {
		return input, err
	}
	return relation{id: id, columns: columns, effects: effects, namespaces: input.namespaces}, nil
}

func (p *projectSetLowering) rewrite(id, direct ir.ScalarID) (ir.ScalarID, error) {
	if id != direct {
		if replacement, ok := p.replacements[id]; ok {
			return replacement.id, nil
		}
	}
	if err := p.step(); err != nil {
		return 0, err
	}
	n, ok := p.b.builder.Scalar(id)
	if !ok {
		return 0, bindError("XX000", "set-returning rewrite source is missing", 0)
	}
	operands := make([]ir.ScalarID, n.OperandCount())
	changed := id == direct
	for i := range n.OperandCount() {
		child, ok := p.b.builder.Operand(id, i)
		if !ok {
			return 0, bindError("XX000", "set-returning rewrite operand is missing", 0)
		}
		rewritten, err := p.rewrite(child, 0)
		if err != nil {
			return 0, err
		}
		operands[i] = rewritten
		changed = changed || rewritten != child
	}
	if !changed {
		return id, nil
	}
	comparisons := make([]ir.SignatureID, n.ComparisonCount())
	for i := range comparisons {
		comparisons[i], _ = p.b.builder.Comparison(id, uint32(i))
	}
	return p.b.builder.AddScalar(p.b.ctx, ir.ScalarSpec{Scalar: n.Metadata(), Operands: operands, Comparisons: comparisons})
}

func (p *projectSetLowering) layout(id ir.LayoutID, depth uint32) (model.TypeLayout, error) {
	if err := p.step(); err != nil {
		return model.TypeLayout{}, err
	}
	if depth > p.b.options.Limits.MaxDepth {
		return model.TypeLayout{}, bindError("54000", "set-returning result layout exceeds depth limit", 0)
	}
	typ, ok := p.b.builder.Type(id)
	if !ok {
		return model.TypeLayout{}, bindError("XX000", "set-returning result layout is missing", 0)
	}
	result := model.TypeLayout{Type: typ}
	count := p.b.builder.LayoutChildCount(id)
	if count != 0 {
		result.Children = make([]model.TypeLayout, count)
		for i := range count {
			child, ok := p.b.builder.LayoutChild(id, i)
			if !ok {
				return model.TypeLayout{}, bindError("XX000", "set-returning result child layout is missing", 0)
			}
			layout, err := p.layout(child, depth+1)
			if err != nil {
				return model.TypeLayout{}, err
			}
			result.Children[i] = layout
		}
	}
	nameCount := p.b.builder.LayoutFieldNameCount(id)
	if nameCount != 0 {
		result.FieldNames = make([]string, nameCount)
		for i := range nameCount {
			name, ok := p.b.builder.LayoutFieldName(id, i)
			if !ok {
				return model.TypeLayout{}, bindError("XX000", "set-returning result field name is missing", 0)
			}
			result.FieldNames[i] = name
		}
	}
	return result, nil
}
