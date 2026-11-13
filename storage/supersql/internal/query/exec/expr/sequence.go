package expr

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func sequenceFunction(id model.FunctionID) bool {
	switch id {
	case 1574, 1575, 1576, 1765, 2559:
		return true
	}
	return false
}

func (c *compiler) resolveSequence(n ir.Scalar, in *instruction) error {
	expected, ok := builtin.LookupSignature(in.identity)
	signature, found := c.graph.Signature(n.Signature)
	if !ok || !found || !sequenceFunction(in.identity) {
		return ErrInvalid
	}
	result, found := c.graph.Type(signature.Result)
	if !found || result.ID != expected.Result || result.Storage != model.TypeInt64 {
		return invalid("sequence result type mismatch")
	}
	if int(signature.ParameterCount()) != int(expected.ArgumentCount) || n.Strict != expected.Strict || signature.VariadicArray {
		return invalid("sequence signature mismatch")
	}
	if n.Effects.Volatility != ir.Volatile {
		return invalid("sequence call lacks volatile effects")
	}
	for i := range int(expected.ArgumentCount) {
		layout, _ := c.graph.SignatureParameter(n.Signature, uint32(i))
		actual, _ := c.graph.Type(layout)
		storage := model.TypeInt64
		if expected.Arguments[i] == 16 {
			storage = model.TypeBoolean
		}
		if actual.ID != expected.Arguments[i] || actual.Storage != storage {
			return invalid("sequence argument type mismatch")
		}
	}
	in.op = opSequence
	in.negated = expected.Strict
	c.p.sequences = true
	return nil
}

func (p *Program) verifySequence(in instruction) error {
	signature, ok := builtin.LookupSignature(in.identity)
	if !ok || !sequenceFunction(in.identity) || len(in.args) != int(signature.ArgumentCount) || in.negated != signature.Strict {
		return ErrInvalid
	}
	if p.registers[in.dst].layout.Type.ID != 20 || p.registers[in.dst].layout.Type.Storage != model.TypeInt64 {
		return ErrInvalid
	}
	for i, operand := range in.args {
		if p.registers[operand].layout.Type.ID != signature.Arguments[i] {
			return ErrInvalid
		}
	}
	return nil
}

func evalSequence(e *evaluation, in instruction) error {
	runtime := e.state.options.Sequences
	registers := e.state.registers
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		var args [3]model.Value
		null := false
		for i, operand := range in.args {
			args[i] = registers[operand].value(lane)
			null = null || args[i].IsNull
		}
		if in.negated && null {
			registers[in.dst].put(lane, model.NullVal(model.TypeInt64))
			continue
		}
		var oid model.OID
		if len(in.args) != 0 {
			if args[0].IntVal < 0 || args[0].IntVal > math.MaxUint32 {
				return &model.Error{Code: "22003", Message: "sequence OID out of range"}
			}
			oid = model.OID(args[0].IntVal)
		}
		var value int64
		var err error
		switch in.identity {
		case 1574:
			value, err = runtime.NextSequence(e.ctx, oid)
		case 1575:
			value, err = runtime.CurrentSequence(e.ctx, oid)
		case 2559:
			value, err = runtime.LastSequence(e.ctx)
		case 1576:
			value, err = runtime.SetSequence(e.ctx, oid, args[1].IntVal, true)
		case 1765:
			value, err = runtime.SetSequence(e.ctx, oid, args[1].IntVal, args[2].IntVal != 0)
		default:
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		registers[in.dst].put(lane, model.Int64Val(value))
	}
	return e.ctx.Err()
}
