package expr

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type arrayCoercion struct {
	element register
	steps   []arrayCoercionStep
	maxArgs int
	context ir.CastContext
}

type arrayCoercionStep struct {
	kind       ir.ScalarKind
	arguments  []register
	result     register
	constants  []model.Value
	identity   model.FunctionID
	call       ScalarCall
	strict     bool
	ownsResult bool
}

func (c *compiler) compileArrayCoerce(ctx context.Context, id ir.ScalarID, n ir.Scalar, depth int) (*arrayCoercion, error) {
	if n.Kind != ir.ArrayCoerce || n.OperandCount() != 2 || n.CastContext < ir.ExplicitCast || n.CastContext > ir.ImplicitCast {
		return nil, invalid("invalid array coercion")
	}
	root, _ := c.graph.Operand(id, 1)
	current := root
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if depth >= c.p.limits.MaxDepth || count >= c.p.limits.MaxDepth-depth || count >= c.p.limits.MaxInstructions {
			return nil, ErrLimit
		}
		node, ok := c.graph.Scalar(current)
		if !ok {
			return nil, invalid("missing array conversion operand")
		}
		if node.Kind == ir.ArrayElement {
			if node.OperandCount() != 0 || !node.Nullable || node.Effects != (ir.Effects{}) {
				return nil, invalid("invalid array element placeholder")
			}
			break
		}
		switch node.Kind {
		case ir.Call, ir.Cast:
			if node.OperandCount() == 0 || node.OperandCount() > 8 {
				return nil, invalid("invalid array conversion arity")
			}
		case ir.Relabel, ir.Collate:
			if node.OperandCount() != 1 {
				return nil, invalid("invalid array conversion relabel")
			}
		default:
			return nil, invalid("unsupported array conversion template")
		}
		current, _ = c.graph.Operand(current, 0)
		count++
	}
	if count > c.p.limits.MaxInstructions-len(c.p.code)-c.p.templateInstructions {
		return nil, ErrLimit
	}
	c.p.templateInstructions += count
	if err := c.p.charge(uint64(unsafe.Sizeof(arrayCoercion{})) + uint64(count)*uint64(unsafe.Sizeof(arrayCoercionStep{}))); err != nil {
		return nil, err
	}
	plan := &arrayCoercion{steps: make([]arrayCoercionStep, count), context: n.CastContext}
	element, _ := c.graph.Scalar(current)
	layout, err := c.layout(ctx, element.Layout, 0)
	if err != nil {
		return nil, err
	}
	plan.element = register{layout: layout, nullable: true}
	current = root
	for i := count - 1; i >= 0; i-- {
		node, _ := c.graph.Scalar(current)
		step := &plan.steps[i]
		step.kind, step.strict = node.Kind, node.Strict
		layout, err := c.layout(ctx, node.Layout, 0)
		if err != nil {
			return nil, err
		}
		step.result = register{layout: layout, nullable: node.Nullable}
		argc := int(node.OperandCount())
		if err := c.p.charge(uint64(argc) * uint64(unsafe.Sizeof(register{}))); err != nil {
			return nil, err
		}
		step.arguments = make([]register, argc)
		constantHeaders := uint64(argc-1) * uint64(unsafe.Sizeof(model.Value{}))
		if constantHeaders > c.p.limits.MaxConstantBytes-c.p.constantBytes {
			return nil, ErrLimit
		}
		if err := c.p.charge(constantHeaders); err != nil {
			return nil, err
		}
		c.p.constantBytes += constantHeaders
		step.constants = make([]model.Value, argc-1)
		for j := range argc {
			argID, _ := c.graph.Operand(current, uint32(j))
			arg, _ := c.graph.Scalar(argID)
			argLayout, err := c.layout(ctx, arg.Layout, 0)
			if err != nil {
				return nil, err
			}
			step.arguments[j] = register{layout: argLayout, nullable: arg.Nullable}
			if j == 0 {
				continue
			}
			if arg.Kind != ir.Const {
				return nil, invalid("array conversion extra operand is not constant")
			}
			limit := min(c.p.limits.MaxConstantBytes-c.p.constantBytes, c.p.limits.MaxBytes-c.p.bytes)
			value, err := c.graph.CloneConstant(ctx, argID, limit)
			if err != nil {
				return nil, err
			}
			cost, err := valueBytes(ctx, value, c.p.limits.MaxConstantBytes-c.p.constantBytes)
			if err != nil {
				return nil, err
			}
			if err := c.p.charge(cost); err != nil {
				return nil, err
			}
			c.p.constantBytes += cost
			step.constants[j-1] = value
		}
		if node.Kind == ir.Call || node.Kind == ir.Cast {
			var resolved instruction
			if err := c.resolve(ctx, node, &resolved); err != nil {
				return nil, err
			}
			step.identity, step.call, step.ownsResult = resolved.identity, resolved.call, resolved.ownsResult
			plan.maxArgs = max(plan.maxArgs, argc)
		}
		current, _ = c.graph.Operand(current, 0)
	}
	return plan, nil
}

func (p *Program) verifyArrayCoerce(in instruction) error {
	plan := in.coercion
	if plan == nil || len(in.args) != 1 || plan.context < ir.ExplicitCast || plan.context > ir.ImplicitCast {
		return invalid("invalid array coercion plan")
	}
	source, destination := p.registers[in.args[0]], p.registers[in.dst]
	if source.layout.Type.Storage != model.TypeArray || destination.layout.Type.Storage != model.TypeArray || len(source.layout.Children) != 1 || len(destination.layout.Children) != 1 || source.nullable != destination.nullable {
		return invalid("array coercion source or result mismatch")
	}
	previous := plan.element
	if !previous.nullable || !model.TypeLayoutsEqual(previous.layout, source.layout.Children[0]) {
		return invalid("array coercion element mismatch")
	}
	maxArgs := 0
	for _, step := range plan.steps {
		if len(step.arguments) == 0 || len(step.arguments) > 8 || len(step.constants) != len(step.arguments)-1 || !model.TypeLayoutsEqual(step.arguments[0].layout, previous.layout) || step.arguments[0].nullable != previous.nullable {
			return invalid("array conversion chain mismatch")
		}
		if err := step.result.layout.Validate(); err != nil {
			return err
		}
		switch step.kind {
		case ir.Call, ir.Cast:
			if step.identity == 0 || step.call == nil {
				return invalid("unresolved array conversion")
			}
			maxArgs = max(maxArgs, len(step.arguments))
			for i, constant := range step.constants {
				argument := step.arguments[i+1]
				if constant.Type != argument.layout.Type.Storage || constant.IsNull != argument.nullable {
					return invalid("array conversion constant mismatch")
				}
			}
		case ir.Collate, ir.Relabel:
			if len(step.arguments) != 1 || step.call != nil || step.result.nullable != previous.nullable {
				return invalid("invalid array conversion relabel")
			}
			layout := previous.layout
			if step.kind == ir.Collate {
				layout.Type.Collation = step.result.layout.Type.Collation
			} else {
				modifier := step.result.layout.Type.Modifier
				if modifier != -1 && modifier != layout.Type.Modifier {
					return invalid("array conversion relabel narrows type modifier")
				}
				layout.Type.ID, layout.Type.Modifier = step.result.layout.Type.ID, modifier
			}
			if !model.TypeLayoutsEqual(layout, step.result.layout) {
				return invalid("array conversion relabel changes physical layout")
			}
		default:
			return invalid("invalid array conversion step")
		}
		previous = step.result
	}
	if plan.maxArgs != maxArgs || !model.TypeLayoutsEqual(previous.layout, destination.layout.Children[0]) {
		return invalid("array conversion result mismatch")
	}
	return nil
}

func evalArrayCoerce(e *evaluation, in instruction) error {
	plan := in.coercion
	out := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		source := e.state.registers[in.args[0]].value(lane)
		if source.IsNull {
			out.put(lane, model.NullVal(model.TypeArray))
			continue
		}
		if len(source.Dimensions) == 0 {
			out.put(lane, model.Value{Type: model.TypeArray})
			continue
		}
		dimensionBytes := uint64(len(source.Dimensions)) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
		width := uint64(unsafe.Sizeof(model.Value{}))
		if dimensionBytes > e.remaining() || uint64(len(source.ArrayVal)) > (e.remaining()-dimensionBytes)/width {
			return ErrLimit
		}
		if err := e.charge(dimensionBytes + uint64(len(source.ArrayVal))*width); err != nil {
			return err
		}
		result := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, len(source.Dimensions)), ArrayVal: make([]model.Value, len(source.ArrayVal))}
		copy(result.Dimensions, source.Dimensions)
		out.put(lane, result)
		for i, element := range source.ArrayVal {
			if err := e.ctx.Err(); err != nil {
				return err
			}
			converted, err := plan.convert(e, element)
			if err != nil {
				return err
			}
			result.ArrayVal[i] = converted
		}
	}
	return e.ctx.Err()
}

func (step *arrayCoercionStep) relabel(value *model.Value) {
	if step.kind != ir.Relabel || value.IsNull {
		return
	}
	from, to := step.arguments[0].layout.Type.ID, step.result.layout.Type.ID
	if from == 23 && builtin.IsOIDType(to) {
		value.IntVal = int64(uint32(value.IntVal))
	} else if builtin.IsOIDType(from) && to == 23 {
		value.IntVal = int64(int32(uint32(value.IntVal)))
	}
}

func (plan *arrayCoercion) convert(e *evaluation, value model.Value) (model.Value, error) {
	ownedBytes := uint64(0)
	for i := range plan.steps {
		step := &plan.steps[i]
		if err := e.ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if step.call == nil {
			step.relabel(&value)
			continue
		}
		args := e.args[:len(step.arguments)]
		args[0] = value
		copy(args[1:], step.constants)
		hasNull := false
		for _, arg := range args {
			hasNull = hasNull || arg.IsNull
		}
		before := e.used
		var next model.Value
		var err error
		if step.strict && hasNull {
			if !step.result.nullable {
				return model.Value{}, invalid("non-nullable array conversion returned NULL")
			}
			next = model.NullVal(step.result.layout.Type.Storage)
		} else {
			next, err = e.invokeScalar(step.call, args, step.result, step.ownsResult)
		}
		clear(args)
		if err != nil {
			return model.Value{}, err
		}
		nextBytes := e.used - before
		// Keep both payloads admitted until the new result owns its references.
		value = next
		next = model.Value{}
		if ownedBytes != 0 {
			if e.state.work != nil {
				if err := e.state.work.Resize(e.used - ownedBytes); err != nil {
					return model.Value{}, err
				}
			}
			e.used -= ownedBytes
		}
		ownedBytes = nextBytes
	}
	return value, nil
}
