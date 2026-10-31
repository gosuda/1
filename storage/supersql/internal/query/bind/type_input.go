package bind

import (
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) inputLiteral(s *scope, e expression, target model.TypeLayout) (expression, error) {
	if target.Type.ID == 2205 {
		return b.regclassLiteral(s, e, target)
	}
	info, sig, err := b.inputFunction(target, e.position)
	if err != nil {
		return e, err
	}
	var values [3]model.Value
	var layouts [3]model.TypeLayout
	values[0], layouts[0] = e.value, b.primitive(2275)
	if sig.ArgumentCount >= 2 {
		values[1] = model.Int64Val(int64(info.Parameter))
		layouts[1] = b.primitive(sig.Arguments[1])
	}
	if sig.ArgumentCount == 3 {
		values[2] = model.Int64Val(int64(target.Type.Modifier))
		layouts[2] = b.primitive(sig.Arguments[2])
	}
	options := b.options.Builtin
	options.Collation, options.DefaultCollation = b.options.Collation, b.options.DefaultCollation
	maximum := min(b.options.Limits.MaxBytes, uint64(math.MaxInt64))
	function, err := builtin.ResolveTyped(b.ctx, sig.ID, layouts[:sig.ArgumentCount], target, options, maximum)
	if err != nil {
		return e, err
	}
	remaining := maximum - function.OwnedBytes()
	value, err := function.Call(b.ctx, values[:sig.ArgumentCount], builtin.CallBudget{OutputBytes: int64(remaining), MemoryBytes: remaining, Temporal: b.options.Temporal})
	if err != nil {
		var sqlErr *model.Error
		if errors.As(err, &sqlErr) {
			positioned := *sqlErr
			positioned.Position = e.position
			return e, &positioned
		}
		return e, err
	}
	if err := b.recordTemporalLiteral(e, target, value); err != nil {
		return e, err
	}
	return b.addScalar(s, expression{typ: target, value: value, nullable: value.IsNull, position: e.position}, ir.Scalar{Kind: ir.Const}, nil)
}

func (b *binder) inputValue(s *scope, text expression, target model.TypeLayout) (expression, error) {
	info, sig, err := b.inputFunction(target, text.position)
	if err != nil {
		return text, err
	}
	args := []expression{text}
	types := []model.TypeLayout{text.typ}
	if sig.ArgumentCount >= 2 {
		value := int64(info.Parameter)
		typ := b.primitive(sig.Arguments[1])
		if typ.Type.Storage != model.TypeInt64 {
			return text, missing("type input metadata arguments", text.position)
		}
		arg, err := b.addScalar(s, expression{typ: typ, value: model.Int64Val(value)}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return text, err
		}
		args = append(args, arg)
		types = append(types, typ)
	}
	if sig.ArgumentCount == 3 {
		typ := b.primitive(sig.Arguments[2])
		arg, err := b.addScalar(s, expression{typ: typ, value: model.Int64Val(int64(target.Type.Modifier))}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return text, err
		}
		args = append(args, arg)
		types = append(types, typ)
	}
	signature, err := b.signature(sig, 0, types, target, 0, false)
	if err != nil {
		return text, err
	}
	return b.addScalar(s, expression{typ: target, nullable: functionNullable(sig) || text.nullable, effects: signatureEffects(sig), position: text.position}, ir.Scalar{Kind: ir.Call, Function: sig.ID, Signature: signature, Strict: sig.Strict}, args)
}

func (b *binder) inputFunction(target model.TypeLayout, pos int) (builtin.TypeIO, builtin.Signature, error) {
	info, ok := b.lookupTypeIO(target)
	if !ok || info.Input == 0 {
		return info, builtin.Signature{}, missing("resolved type input function", pos)
	}
	if err := b.validateIOLayout(target, pos); err != nil {
		return info, builtin.Signature{}, err
	}
	sig, ok := builtin.LookupSignature(info.Input)
	if !ok {
		return info, sig, missing("type input signature metadata", pos)
	}
	if err := ValidateBuiltinFunction(sig.ID, pos); err != nil {
		return info, sig, err
	}
	validArity := sig.ArgumentCount >= 1 && sig.ArgumentCount <= 3
	if !validArity || sig.Arguments[0] != 2275 {
		return info, sig, missing("type input signature form", pos)
	}
	return info, sig, nil
}
