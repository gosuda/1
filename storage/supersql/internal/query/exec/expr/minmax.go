package expr

import "github.com/gosuda/supersql-alpha/internal/model"

func evalMinMax(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	args := e.args[:2]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		selected := model.NullVal(out.vector.Type.Storage)
		for i, r := range in.args {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			candidate := e.state.registers[r].value(lane)
			if candidate.IsNull {
				continue
			}
			if selected.IsNull {
				selected = candidate
				continue
			}
			args[0], args[1] = candidate, selected
			comparison, err := e.invokeScalar(in.call, args, register{layout: model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}, nullable: true}, true)
			if err != nil {
				return err
			}
			invalidBoolean := !comparison.IsNull && comparison.IntVal != 0 && comparison.IntVal != 1
			if comparison.Type != model.TypeBoolean || invalidBoolean {
				return invalid("min/max comparison returned a non-boolean value")
			}
			if booleanIs(comparison, true) {
				selected = candidate
			}
		}
		// Operand registers remain live until the output builder owns their payloads.
		out.put(lane, selected)
	}
	return e.ctx.Err()
}
