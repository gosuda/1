package expr

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// EvalBorrowedRow requires one logical input row and one output slot per program
// output. It evaluates every expression before publishing borrowed values, without
// an output-builder copy. Input and parameter payloads must remain immutable until
// the next evaluation or Close; that boundary also invalidates output payloads.
// The caller owns out, which is cleared on failure.
func (s *State) EvalBorrowedRow(ctx context.Context, input *columnar.Batch, parameters []ParameterValue, out []model.Value, mode Mode) (err error) {
	clear(out)
	if s == nil || s.closed || s.program == nil {
		return ErrClosed
	}
	if input == nil || input.Rows() != 1 || len(out) != len(s.program.outputs) {
		return errors.Join(ErrInvalid, s.release())
	}
	if err := s.release(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			clear(out)
			err = errors.Join(err, s.release())
		}
	}()
	if _, err := s.evaluate(ctx, input, parameters, 1, mode); err != nil {
		return err
	}
	for i, r := range s.program.outputs {
		value := s.registers[r].value(0)
		if value.IsNull && !s.program.registers[r].nullable {
			return invalid("non-nullable expression returned NULL")
		}
		out[i] = value
	}
	return nil
}
