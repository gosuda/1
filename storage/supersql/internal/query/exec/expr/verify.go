package expr

import (
	"math/bits"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Verify checks register dominance, types, indexes and structured mask targets.
// Programs are not deserializable; Compile is the only construction boundary.
func (p *Program) Verify() error {
	if p == nil {
		return ErrInvalid
	}
	if p.templateInstructions < 0 || p.templateInstructions > p.limits.MaxInstructions {
		return ErrInvalid
	}
	if len(p.code) == 0 || len(p.outputs) == 0 || len(p.code) > p.limits.MaxInstructions-p.templateInstructions || len(p.registers) > p.limits.MaxRegisters {
		return ErrInvalid
	}
	for i, capture := range p.captures {
		if capture.Slot == 0 || capture.Column < 0 {
			return invalid("invalid capture mapping")
		}
		if err := capture.layout.Validate(); err != nil {
			return err
		}
		for _, input := range p.inputs {
			if input.Slot == capture.Slot {
				return invalid("slot mapped as both input and capture")
			}
		}
		for _, previous := range p.captures[:i] {
			if previous.Slot == capture.Slot || previous.Column == capture.Column {
				return invalid("duplicate capture mapping")
			}
		}
	}
	if err := p.verifySubqueries(); err != nil {
		return err
	}
	initialized := make([]int, len(p.registers))
	for i := range initialized {
		initialized[i] = -1
	}
	parents := []int{-1}
	stack := []int{0}
	cacheStack := []int{-1}
	visible := func(r int) bool {
		if r < 0 || r >= len(initialized) || initialized[r] < 0 {
			return false
		}
		for scope := stack[len(stack)-1]; scope >= 0; scope = parents[scope] {
			if initialized[r] == scope {
				return true
			}
		}
		return false
	}
	boolean := func(r int) bool { return p.registers[r].layout.Type.Storage == model.TypeBoolean }
	templateInstructions := 0
	for _, subquery := range p.subqueries {
		for _, comparison := range subquery.comparisons {
			templateInstructions += len(comparison.left.code) + comparison.left.templateInstructions
		}
	}
	for pc, in := range p.code {
		if in.op != opArraySlice && in.op != opArrayStore && in.subscripts != (ir.ArraySubscripts{}) {
			return invalid("array subscripts on another instruction")
		}
		if in.op != opRowCompare && in.rowComparison != 0 {
			return invalid("row comparison on another instruction")
		}
		if in.op != opArrayQuantified && (in.subqueryKind != 0 || in.strict) {
			return invalid("array quantifier metadata on another instruction")
		}
		if in.op != opArrayStore && in.sliceAssignment {
			return invalid("array store metadata on another instruction")
		}
		for _, r := range in.args {
			if !visible(r) {
				return invalid("uninitialized or masked register read")
			}
		}
		if in.op != opEnter && in.op != opFilter && in.op != opLeave && in.op != opOnceEnter && in.op != opOnceStore {
			if in.dst < 0 || in.dst >= len(initialized) {
				return invalid("invalid destination")
			}
			if p.registers[in.dst].once {
				return invalid("cache register used as ordinary destination")
			}
			if in.op != opInit && !visible(in.dst) {
				return invalid("uninitialized destination")
			}
		}
		if in.op == opOnceEnter || in.op == opOnceStore || in.op == opOnceRead {
			if in.index < 0 || in.index >= len(p.registers) || !p.registers[in.index].once {
				return invalid("invalid single-evaluation cache")
			}
			if err := p.registers[in.index].layout.Validate(); err != nil {
				return err
			}
		}
		switch in.op {
		case opOnceEnter:
			if len(in.args) != 0 {
				return ErrInvalid
			}
			if len(stack) >= p.limits.MaxDepth+1 {
				return ErrLimit
			}
			if slices.Contains(cacheStack, in.index) {
				return invalid("recursive single evaluation")
			}
			parents = append(parents, stack[len(stack)-1])
			stack = append(stack, len(parents)-1)
			cacheStack = append(cacheStack, in.index)
		case opOnceStore:
			if len(stack) == 1 || cacheStack[len(cacheStack)-1] != in.index || len(in.args) != 1 {
				return invalid("unbalanced single evaluation")
			}
			cache, operand := p.registers[in.index], p.registers[in.args[0]]
			if !model.TypeLayoutsEqual(cache.layout, operand.layout) || cache.nullable != operand.nullable {
				return invalid("single evaluation operand mismatch")
			}
			stack = stack[:len(stack)-1]
			cacheStack = cacheStack[:len(cacheStack)-1]
		case opOnceRead:
			if len(in.args) != 0 || pc == 0 || p.code[pc-1].op != opOnceStore || p.code[pc-1].index != in.index {
				return invalid("single evaluation read without completed mask")
			}
			cache, destination := p.registers[in.index], p.registers[in.dst]
			if !model.TypeLayoutsEqual(cache.layout, destination.layout) || cache.nullable != destination.nullable {
				return invalid("single evaluation result mismatch")
			}
		case opInit:
			if initialized[in.dst] != -1 || len(in.args) != 0 || in.index < 0 || in.index > 2 {
				return ErrInvalid
			}
			if in.index != 0 && !boolean(in.dst) {
				return ErrInvalid
			}
			if err := p.registers[in.dst].layout.Validate(); err != nil {
				return err
			}
			initialized[in.dst] = stack[len(stack)-1]
		case opConst:
			if len(in.args) != 0 || in.index < 0 || in.index >= len(p.constants) || p.constants[in.index].Type != p.registers[in.dst].layout.Type.Storage {
				return ErrInvalid
			}
		case opInput:
			if len(in.args) != 0 || in.index < 0 || in.index >= len(p.inputs) {
				return ErrInvalid
			}
		case opCapture:
			if len(in.args) != 0 || in.index < 0 || in.index >= len(p.captures) {
				return ErrInvalid
			}
			capture, destination := p.captures[in.index], p.registers[in.dst]
			if !capture.used || !model.TypeLayoutsEqual(capture.layout, destination.layout) || capture.nullable != destination.nullable {
				return invalid("capture operand mismatch")
			}
		case opParam:
			if len(in.args) != 0 || in.index < 0 || in.index >= len(p.parameters) || !model.TypeLayoutsEqual(p.parameters[in.index].Layout, p.registers[in.dst].layout) {
				return ErrInvalid
			}
		case opCall:
			if in.call == nil || in.identity == 0 {
				return ErrInvalid
			}
		case opSQLJSON:
			if in.call == nil || in.identity != 0 || in.operator != 0 {
				return ErrInvalid
			}
		case opSubquery:
			if err := p.verifySubquery(in); err != nil {
				return err
			}
		case opArrayQuantified:
			if err := p.verifyArrayQuantified(in); err != nil {
				return err
			}
		case opSequence:
			if err := p.verifySequence(in); err != nil {
				return err
			}
		case opRelation:
			if err := p.verifyRelation(in); err != nil {
				return err
			}
		case opRowCompare:
			if err := p.verifyRowCompare(in); err != nil {
				return err
			}
		case opMinMax:
			if in.call == nil || in.identity == 0 || len(in.args) == 0 {
				return invalid("min/max lacks a comparison or operands")
			}
			for _, r := range in.args {
				if !model.TypeLayoutsEqual(p.registers[in.dst].layout, p.registers[r].layout) {
					return invalid("min/max operand layout differs from result")
				}
			}
		case opCopy:
			if len(in.args) != 1 || !model.TypeLayoutsEqual(p.registers[in.dst].layout, p.registers[in.args[0]].layout) {
				return ErrInvalid
			}
		case opCollate, opRelabel:
			if len(in.args) != 1 {
				return ErrInvalid
			}
			source, destination := p.registers[in.args[0]], p.registers[in.dst]
			if in.op == opCollate {
				source.layout.Type.Collation = destination.layout.Type.Collation
			} else {
				modifier := destination.layout.Type.Modifier
				if modifier != -1 && modifier != source.layout.Type.Modifier {
					return invalid("binary relabel narrows type modifier")
				}
				source.layout.Type.ID = destination.layout.Type.ID
				source.layout.Type.Modifier = modifier
			}
			if !model.TypeLayoutsEqual(source.layout, destination.layout) || source.nullable != destination.nullable {
				return invalid("collation relabel changes physical layout or nullability")
			}
		case opNot, opMergeAnd, opMergeOr:
			if len(in.args) != 1 || !boolean(in.dst) || !boolean(in.args[0]) {
				return ErrInvalid
			}
		case opNull:
			if len(in.args) != 1 || !boolean(in.dst) {
				return ErrInvalid
			}
		case opEnter, opFilter:
			if in.op == opFilter && cacheStack[len(cacheStack)-1] != -1 {
				return invalid("single evaluation must complete its missing-lane mask")
			}
			if err := p.verifyMask(in, len(stack)); err != nil {
				return err
			}
			if in.op == opEnter {
				parents = append(parents, stack[len(stack)-1])
				stack = append(stack, len(parents)-1)
				cacheStack = append(cacheStack, -1)
			}
		case opLeave:
			if len(stack) == 1 || len(in.args) != 0 || cacheStack[len(cacheStack)-1] != -1 {
				return invalid("unbalanced mask")
			}
			stack = stack[:len(stack)-1]
			cacheStack = cacheStack[:len(cacheStack)-1]
		case opField, opFieldStore:
			expected := 1
			if in.op == opFieldStore {
				expected = 2
			}
			if len(in.args) != expected {
				return ErrInvalid
			}
			parent := p.registers[in.args[0]].layout
			if parent.Type.Storage != model.TypeComposite || in.index < 0 || in.index >= len(parent.Children) {
				return ErrInvalid
			}
			if in.op == opField {
				if !model.TypeLayoutsEqual(p.registers[in.dst].layout, parent.Children[in.index]) {
					return ErrInvalid
				}
			} else if p.registers[in.dst].nullable || !model.TypeLayoutsEqual(p.registers[in.dst].layout, parent) || !model.TypeLayoutsEqual(p.registers[in.args[1]].layout, parent.Children[in.index]) {
				return invalid("field store layout mismatch")
			}
		case opArrayCoerce:
			if err := p.verifyArrayCoerce(in); err != nil {
				return err
			}
			if len(in.coercion.steps) > p.limits.MaxInstructions-templateInstructions {
				return ErrInvalid
			}
			templateInstructions += len(in.coercion.steps)
		case opArrayIndex, opArraySlice:
			if err := p.verifyArraySubscript(in); err != nil {
				return err
			}
		case opArrayStore:
			if err := p.verifyArrayStore(in); err != nil {
				return err
			}
		case opJSONBStore:
			if err := p.verifyJSONBStore(in); err != nil {
				return err
			}
		case opArrayStack:
			destination := p.registers[in.dst]
			if len(in.args) == 0 || destination.nullable || destination.layout.Type.Storage != model.TypeArray {
				return invalid("array stack requires array operands and a nonnull array result")
			}
			for _, r := range in.args {
				if !model.TypeLayoutsEqual(destination.layout, p.registers[r].layout) {
					return invalid("array stack operand layout differs from result")
				}
			}
		case opRow, opArray:
			if err := p.verifyConstructor(in); err != nil {
				return err
			}
		default:
			return invalid("unknown instruction")
		}
	}
	if templateInstructions != p.templateInstructions {
		return invalid("array conversion instruction accounting mismatch")
	}
	if len(stack) != 1 {
		return invalid("unterminated mask")
	}
	for _, r := range p.outputs {
		if !visible(r) {
			return invalid("masked output register")
		}
	}
	return nil
}

func (p *Program) verifyArrayQuantified(in instruction) error {
	if len(in.args) != 2 || in.call == nil || in.identity == 0 || (in.subqueryKind != ir.AnySubquery && in.subqueryKind != ir.AllSubquery) {
		return invalid("invalid quantified array comparison")
	}
	array, result := p.registers[in.args[1]], p.registers[in.dst]
	if array.layout.Type.Storage != model.TypeArray || len(array.layout.Children) != 1 || result.layout.Type.Storage != model.TypeBoolean || !result.nullable {
		return invalid("quantified array comparison layout")
	}
	return nil
}

func (p *Program) verifyMask(in instruction, depth int) error {
	if in.mask > maskZero {
		return ErrInvalid
	}
	if in.op == opFilter {
		if depth == 1 || in.mask == maskAll {
			return ErrInvalid
		}
	}
	if in.mask == maskAll {
		if len(in.args) != 0 {
			return ErrInvalid
		}
	} else {
		if len(in.args) != 1 {
			return ErrInvalid
		}
		if in.mask == maskZero {
			if p.registers[in.args[0]].layout.Type != (model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}) {
				return invalid("row comparison continuation requires int4")
			}
		} else if in.mask != maskNull && in.mask != maskNotNull && p.registers[in.args[0]].layout.Type.Storage != model.TypeBoolean {
			return ErrInvalid
		}
	}
	if in.op == opEnter && depth >= p.limits.MaxDepth+1 {
		return ErrLimit
	}
	return nil
}

func (p *Program) verifyConstructor(in instruction) error {
	l := p.registers[in.dst].layout
	if in.op == opRow {
		if l.Type.Storage != model.TypeComposite || len(in.args) != len(l.Children) {
			return ErrInvalid
		}
		for i, r := range in.args {
			if !model.TypeLayoutsEqual(p.registers[r].layout, l.Children[i]) {
				return ErrInvalid
			}
		}
		return nil
	}
	if l.Type.Storage != model.TypeArray || len(l.Children) != 1 {
		return ErrInvalid
	}
	for _, r := range in.args {
		if !model.TypeLayoutsEqual(p.registers[r].layout, l.Children[0]) {
			return ErrInvalid
		}
	}
	return nil
}

func (p *Program) verifyArraySubscript(in instruction) error {
	if len(in.args) == 0 || !p.registers[in.dst].nullable {
		return invalid("array fetch requires an array source and nullable result")
	}
	source := p.registers[in.args[0]].layout
	if source.Type.Storage != model.TypeArray || len(source.Children) != 1 {
		return invalid("array fetch source must have one element layout")
	}
	if in.op == opArrayIndex {
		if len(in.args) < 2 || len(in.args) > 7 || !model.TypeLayoutsEqual(p.registers[in.dst].layout, source.Children[0]) {
			return invalid("array index requires one to six indexes and the exact element layout")
		}
	} else {
		subscripts := in.subscripts
		if subscripts.Dimensions < 1 || subscripts.Dimensions > 6 {
			return invalid("array slice requires one to six dimensions")
		}
		mask := uint8(1<<subscripts.Dimensions) - 1
		if (subscripts.Upper|subscripts.Lower)&^mask != 0 || len(in.args) != 1+bits.OnesCount8(subscripts.Upper)+bits.OnesCount8(subscripts.Lower) {
			return invalid("array slice bound masks differ from operands")
		}
		if !model.TypeLayoutsEqual(p.registers[in.dst].layout, source) {
			return invalid("array slice result differs from source layout")
		}
	}
	for _, r := range in.args[1:] {
		layout := p.registers[r].layout
		if layout.Type.ID != 23 || layout.Type.Storage != model.TypeInt64 || len(layout.Children) != 0 {
			return invalid("array fetch indexes must be int4")
		}
	}
	return nil
}

func (p *Program) verifyArrayStore(in instruction) error {
	if len(in.args) < 3 || p.registers[in.dst].nullable || in.subscripts.Dimensions < 1 || in.subscripts.Dimensions > 6 {
		return invalid("invalid array store")
	}
	source := p.registers[in.args[0]].layout
	if source.Type.Storage != model.TypeArray || len(source.Children) != 1 || !model.TypeLayoutsEqual(source, p.registers[in.dst].layout) {
		return invalid("array store source and result layout mismatch")
	}
	mask := uint8(1<<in.subscripts.Dimensions) - 1
	if (in.subscripts.Upper|in.subscripts.Lower)&^mask != 0 {
		return invalid("array store bound mask exceeds its dimensions")
	}
	bounds := len(in.args) - 2
	replacement := p.registers[in.args[len(in.args)-1]].layout
	if in.sliceAssignment {
		if bounds != bits.OnesCount8(in.subscripts.Upper)+bits.OnesCount8(in.subscripts.Lower) || !model.TypeLayoutsEqual(replacement, source) {
			return invalid("array slice store layout mismatch")
		}
	} else if in.subscripts.Upper != 0 || in.subscripts.Lower != 0 || bounds != int(in.subscripts.Dimensions) || !model.TypeLayoutsEqual(replacement, source.Children[0]) {
		return invalid("array element store layout mismatch")
	}
	for _, r := range in.args[1 : len(in.args)-1] {
		layout := p.registers[r].layout
		if layout.Type.ID != 23 || layout.Type.Storage != model.TypeInt64 || len(layout.Children) != 0 {
			return invalid("array store indexes must be int4")
		}
	}
	return nil
}

func (p *Program) verifyJSONBStore(in instruction) error {
	if len(in.args) < 3 || p.registers[in.dst].nullable {
		return invalid("invalid jsonb store")
	}
	source := p.registers[in.args[0]].layout
	replacement := p.registers[in.args[len(in.args)-1]].layout
	if source.Type.ID != 3802 || source.Type.Storage != model.TypeJSONB || !model.TypeLayoutsEqual(source, p.registers[in.dst].layout) || !model.TypeLayoutsEqual(source, replacement) {
		return invalid("jsonb store source, replacement, and result layouts differ")
	}
	for _, r := range in.args[1 : len(in.args)-1] {
		index := p.registers[r].layout
		integer := index.Type.ID == 23 && index.Type.Storage == model.TypeInt64
		text := index.Type.ID == 25 && index.Type.Storage == model.TypeString
		if len(index.Children) != 0 || !integer && !text {
			return invalid("jsonb store indexes must be int4 or text")
		}
	}
	return nil
}
