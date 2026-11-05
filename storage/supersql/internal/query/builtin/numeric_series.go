package builtin

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type NumericSeriesOptions struct {
	// MemoryBytes includes headers, coefficients, intermediates and pending output.
	MemoryBytes uint64
	// ValueBytes limits one logical output payload within MemoryBytes.
	ValueBytes uint64
	// Nil enforces only the local byte cap; non-nil also admits shared memory.
	Allocator columnar.Allocator
}

// NumericSeries owns its workspace and borrows its allocator through Close.
// It is single-owner and must not be copied after construction.
type NumericSeries struct {
	work                *decimalint.Workspace
	current, stop, step decimal
	next                decimal
	pending             model.Value
	claim, outputClaim  columnar.Allocation
	allocator           columnar.Allocator
	memoryBytes         uint64
	valueBytes          uint64
	ready, closed       bool
}

var errNumericSeriesAdvance = errors.New("numeric series Advance requires a successful Peek")

// NewNumericSeries borrows the input strings only during construction.
func NewNumericSeries(ctx context.Context, start, stop, step string, options NumericSeriesOptions) (_ *NumericSeries, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	header := uint64(unsafe.Sizeof(NumericSeries{}))
	if options.MemoryBytes < header || options.MemoryBytes > uint64(math.MaxInt) {
		return nil, numericResourceError()
	}
	var claim columnar.Allocation
	if options.Allocator != nil {
		claim, err = options.Allocator.Reserve(header)
		if err != nil {
			return nil, err
		}
	}
	s := &NumericSeries{claim: claim, allocator: options.Allocator, memoryBytes: options.MemoryBytes, valueBytes: options.ValueBytes}
	defer func() {
		if err != nil {
			err = numericAllocationError(errors.Join(err, s.Close()))
		}
	}()
	s.work, err = decimalint.NewWorkspace(ctx, options.Allocator, options.MemoryBytes-header)
	if err != nil {
		return nil, err
	}
	for _, input := range [...]struct {
		text string
		name string
		into *decimal
	}{{start, "start value", &s.current}, {stop, "stop value", &s.stop}, {step, "step size", &s.step}} {
		checkpoint := s.work.Checkpoint()
		value, parseErr := parseNumeric(ctx, s.work, input.text)
		if parseErr != nil {
			return nil, parseErr
		}
		if value.special == numericNaN {
			return nil, sqlError("22023", input.name+" cannot be NaN")
		}
		if value.special != numericFinite {
			return nil, sqlError("22023", input.name+" cannot be infinity")
		}
		if _, err = numericTextSize(value); err != nil {
			return nil, err
		}
		if err = s.work.ReleaseAfter(checkpoint, value.coefficient); err != nil {
			return nil, err
		}
		*input.into = value
	}
	if s.step.coefficient.Sign() == 0 {
		return nil, sqlError("22023", "step size cannot equal zero")
	}
	return s, nil
}

// Peek borrows the pending value until Advance or Close. Repeated calls do not
// increment the series or reserve additional storage.
// Exhaustion and errors close the series and release its reservations.
func (s *NumericSeries) Peek(ctx context.Context) (value model.Value, ok bool, err error) {
	if s == nil || s.closed {
		return model.Value{}, false, nil
	}
	defer func() {
		if err != nil {
			err = numericAllocationError(errors.Join(err, s.Close()))
		}
	}()
	if err = ctx.Err(); err != nil {
		return model.Value{}, false, err
	}
	if s.ready {
		return s.pending, true, nil
	}
	comparison, err := numericCompare(ctx, s.current, s.stop)
	if err != nil {
		return model.Value{}, false, err
	}
	direction := s.step.coefficient.Sign()
	pastUpper := direction > 0 && comparison > 0
	pastLower := direction < 0 && comparison < 0
	if pastUpper || pastLower {
		return model.Value{}, false, s.Close()
	}
	size, err := numericTextSize(s.current)
	if err != nil {
		return model.Value{}, false, err
	}
	if uint64(size) > s.valueBytes {
		return model.Value{}, false, numericResourceError()
	}
	// PostgreSQL increments before returning even the last in-range value.
	checkpoint := s.work.Checkpoint()
	s.next, err = numericArithmetic(ctx, s.work, s.current, s.step, opPl)
	if err != nil {
		return model.Value{}, false, err
	}
	if err = s.work.ReleaseAfter(checkpoint, s.next.coefficient); err != nil {
		return model.Value{}, false, err
	}
	// numericText's Builder rounds its backing allocation. Reserve a conservative
	// capacity for allocator rounding; no coefficient allocations follow.
	if uint64(size) > (math.MaxUint64-64)/2 {
		return model.Value{}, false, numericResourceError()
	}
	outputBytes := uint64(size)*2 + 64
	header := uint64(unsafe.Sizeof(NumericSeries{}))
	remaining := s.memoryBytes - header
	if s.work.UsedBytes() > remaining || outputBytes > remaining-s.work.UsedBytes() {
		return model.Value{}, false, numericResourceError()
	}
	if s.allocator != nil {
		s.outputClaim, err = s.allocator.Reserve(outputBytes)
		if err != nil {
			return model.Value{}, false, err
		}
	}
	s.pending, err = numericValue(ctx, s.current, size)
	if err != nil {
		return model.Value{}, false, err
	}
	s.ready = true
	return s.pending, true, nil
}

// Advance retires the pending value without allocating or repeating arithmetic.
func (s *NumericSeries) Advance() error {
	if s == nil || s.closed || !s.ready {
		return errNumericSeriesAdvance
	}
	s.pending = model.Value{}
	s.ready = false
	if s.outputClaim != nil {
		s.outputClaim.Release()
		s.outputClaim = nil
	}
	previous := s.current.coefficient
	s.current = s.next
	s.next = decimal{}
	if err := previous.Close(); err != nil {
		return numericAllocationError(errors.Join(err, s.Close()))
	}
	return nil
}

func (s *NumericSeries) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	s.ready = false
	s.pending = model.Value{}
	s.current, s.stop, s.step, s.next = decimal{}, decimal{}, decimal{}, decimal{}
	var err error
	if s.work != nil {
		err = s.work.Close()
		s.work = nil
	}
	if s.outputClaim != nil {
		s.outputClaim.Release()
		s.outputClaim = nil
	}
	s.allocator = nil
	if s.claim != nil {
		s.claim.Release()
		s.claim = nil
	}
	return err
}
