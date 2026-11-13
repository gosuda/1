package expr

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func evalArrayQuantified(e *evaluation, in instruction) error {
	left := &e.state.registers[in.args[0]]
	array := &e.state.registers[in.args[1]]
	output := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		leftValue, arrayValue := left.value(lane), array.value(lane)
		if arrayValue.IsNull {
			output.put(lane, model.NullVal(model.TypeBoolean))
			continue
		}
		result := model.BoolVal(in.subqueryKind == ir.AllSubquery)
		for i, element := range arrayValue.ArrayVal {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			comparison := model.NullVal(model.TypeBoolean)
			if !in.strict || !leftValue.IsNull && !element.IsNull {
				args := [2]model.Value{leftValue, element}
				var err error
				comparison, err = e.invokeScalar(in.call, args[:], e.state.program.registers[in.dst], in.ownsResult)
				if err != nil {
					return err
				}
			}
			if comparison.IsNull {
				result = comparison
			} else if (comparison.IntVal != 0) == (in.subqueryKind == ir.AnySubquery) {
				result = comparison
				break
			}
		}
		output.put(lane, result)
	}
	return e.ctx.Err()
}
