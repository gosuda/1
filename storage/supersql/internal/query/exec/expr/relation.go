package expr

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func relationFunction(id model.FunctionID) bool {
	switch id {
	case 1079, 2218, 2219, 3495:
		return true
	}
	return false
}
func (c *compiler) resolveRelation(n ir.Scalar, in *instruction) error {
	expected, ok := builtin.LookupSignature(in.identity)
	signature, found := c.graph.Signature(n.Signature)
	if !ok || !found || !relationFunction(in.identity) {
		return ErrInvalid
	}
	result, found := c.graph.Type(signature.Result)
	if !found || result.ID != expected.Result || signature.ParameterCount() != 1 || !n.Strict || signature.VariadicArray {
		return invalid("relation OID signature mismatch")
	}
	layout, _ := c.graph.SignatureParameter(n.Signature, 0)
	arg, _ := c.graph.Type(layout)
	if arg.ID != expected.Arguments[0] {
		return invalid("relation OID argument mismatch")
	}
	in.op = opRelation
	c.p.relations = true
	return nil
}
func (p *Program) verifyRelation(in instruction) error {
	signature, ok := builtin.LookupSignature(in.identity)
	if !ok || !relationFunction(in.identity) || len(in.args) != 1 {
		return ErrInvalid
	}
	if p.registers[in.dst].layout.Type.ID != signature.Result || p.registers[in.args[0]].layout.Type.ID != signature.Arguments[0] {
		return ErrInvalid
	}
	return nil
}
func evalRelation(e *evaluation, in instruction) error {
	runtime := e.state.options.Relations
	call := func(ctx context.Context, args []model.Value, budget builtin.CallBudget) (model.Value, error) {
		if in.identity == 2219 {
			if args[0].IntVal < 0 || args[0].IntVal > math.MaxUint32 {
				return model.Value{}, &model.Error{Code: "22003", Message: "relation OID out of range"}
			}
			text, err := runtime.RelationName(ctx, model.OID(args[0].IntVal), uint64(budget.OutputBytes))
			return model.StringVal(text), err
		}
		oid, err := runtime.ResolveRelation(ctx, args[0].StrVal, budget.MemoryBytes)
		if err != nil && in.identity == 3495 {
			var sql *model.Error
			if errors.As(err, &sql) {
				switch sql.Code {
				case "42P01", "42601", "42602", "22003", "22P02":
					return model.NullVal(model.TypeInt64), nil
				}
			}
		}
		return model.Int64Val(int64(oid)), err
	}
	out := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		value := e.state.registers[in.args[0]].value(lane)
		if value.IsNull {
			out.put(lane, model.NullVal(out.vector.Type.Storage))
			continue
		}
		args := [1]model.Value{value}
		result, err := e.invokeScalar(call, args[:], e.state.program.registers[in.dst], true)
		if err != nil {
			return err
		}
		out.put(lane, result)
	}
	return e.ctx.Err()
}
