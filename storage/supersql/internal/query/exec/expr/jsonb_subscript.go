package expr

import "github.com/gosuda/supersql-alpha/internal/query/builtin"

func evalJSONBStore(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	args := e.args[:len(in.args)]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		for i, register := range in.args {
			args[i] = e.state.registers[register].value(lane)
		}
		value, err := e.invokeScalar(builtin.JSONBSubscriptStore, args, e.state.program.registers[in.dst], false)
		if err != nil {
			return err
		}
		out.put(lane, value)
	}
	return e.ctx.Err()
}
