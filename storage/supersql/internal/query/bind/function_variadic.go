package bind

import (
	"cmp"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func signatureArgument(signature builtin.Signature, index int) model.TypeID {
	if signature.Variadic != 0 && index >= int(signature.ArgumentCount)-1 {
		return signature.Arguments[signature.ArgumentCount-1]
	}
	return signature.Arguments[index]
}

func (b *binder) anyVariadicScore(signature builtin.Signature, args []expression) int {
	if signature.ArgumentCount == 0 || len(args) < int(signature.ArgumentCount) || len(args) > builtin.MaxCallArguments {
		return -1
	}
	prefix := int(signature.ArgumentCount) - 1
	signature.ArgumentCount--
	signature.Variadic = 0
	return b.overloadScore(signature, args[:prefix])
}

func (b *binder) functionCandidateScore(signature builtin.Signature, args []expression) int {
	if signature.Variadic == 0 || signature.Variadic == 2276 {
		return b.overloadScore(signature, args)
	}
	if signature.ArgumentCount == 0 || len(args) < int(signature.ArgumentCount) || len(args) > builtin.MaxCallArguments || polymorphic(signature.Variadic) {
		return -1
	}
	prefix := int(signature.ArgumentCount) - 1
	element := signature.Arguments[prefix]
	signature.Variadic = 0
	signature.ArgumentCount--
	score := b.overloadScore(signature, args[:prefix])
	if score < 0 {
		return -1
	}
	signature.ArgumentCount = 1
	signature.Arguments[0] = element
	for i := prefix; i < len(args); i++ {
		argumentScore := b.overloadScore(signature, args[i:i+1])
		if argumentScore < 0 {
			return -1
		}
		score += argumentScore
	}
	return score
}

func (b *binder) invokeVariadic(s *scope, signature builtin.Signature, args []expression, variadicArray bool, pos int) (expression, error) {
	if signature.Variadic != 2276 || signature.ArgumentCount == 0 || len(args) < int(signature.ArgumentCount) || len(args) > builtin.MaxCallArguments {
		return expression{}, bindError("42883", "function does not exist", pos)
	}
	prefix := int(signature.ArgumentCount) - 1
	if variadicArray && len(args) != prefix+1 {
		return expression{}, bindError("42883", "function does not exist", pos)
	}
	fixed := signature
	fixed.Variadic = 0
	fixed.ArgumentCount--
	fixed, err := b.specialize(fixed, args[:prefix], pos)
	if err != nil {
		return expression{}, err
	}
	fixedTargets, err := b.functionArguments(fixed, args[:prefix], pos)
	if err != nil {
		return expression{}, err
	}
	result, err := b.functionResult(fixed, args[:prefix])
	if err != nil {
		return expression{}, err
	}
	if result.Type.Storage == model.TypeAny {
		return expression{}, missing("physical layout for resolved function result", pos)
	}
	return b.invokeArguments(s, signature, 0, args, ir.Call, 0, pos, result, fixedTargets[:prefix], variadicArray)
}

func (b *binder) invokeTypedVariadic(s *scope, signature builtin.Signature, args []expression, expand bool, pos int) (expression, error) {
	if !expand {
		return b.invoke(s, signature, 0, args, ir.Call, 0, pos)
	}
	if signature.ArgumentCount == 0 || len(args) < int(signature.ArgumentCount) || len(args) > builtin.MaxCallArguments {
		return expression{}, bindError("42883", "function does not exist", pos)
	}
	prefix := int(signature.ArgumentCount) - 1
	array := b.primitive(signature.Arguments[prefix])
	if array.Type.Storage != model.TypeArray || len(array.Children) != 1 || array.Children[0].Type.ID != signature.Variadic {
		return expression{}, missing("physical layout for variadic array", pos)
	}
	fixed := signature
	fixed.Variadic = 0
	fixed.ArgumentCount--
	fixed, err := b.specialize(fixed, args[:prefix], pos)
	if err != nil {
		return expression{}, err
	}
	targets, err := b.functionArguments(fixed, args[:prefix], pos)
	if err != nil {
		return expression{}, err
	}
	// Coerce in formal order before packing, including shared parameter inference.
	for i := range args {
		target := array.Children[0]
		if i < prefix {
			target = targets[i]
		}
		args[i], err = b.coerce(s, args[i], target, ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
	}
	packed, err := b.addScalar(s, expression{typ: array, position: args[prefix].position}, ir.Scalar{Kind: ir.Array}, args[prefix:])
	if err != nil {
		return expression{}, err
	}
	args[prefix] = packed
	return b.invoke(s, signature, 0, args[:prefix+1], ir.Call, 0, pos)
}

func (b *binder) variadicArgumentType(arg expression, variadicArray bool) (model.TypeLayout, error) {
	id := b.argumentID(arg)
	if id == 0 && arg.parameter != 0 {
		return model.TypeLayout{}, bindError("42P18", "could not determine data type of parameter", arg.position)
	}
	if variadicArray && id == 0 {
		return model.TypeLayout{}, bindError("42804", "VARIADIC argument must be an array", arg.position)
	}
	target := b.argumentType(cmp.Or(id, 25), arg)
	if variadicArray && (target.Type.Storage != model.TypeArray || len(target.Children) != 1) {
		return model.TypeLayout{}, bindError("42804", "VARIADIC argument must be an array", arg.position)
	}
	return target, nil
}
