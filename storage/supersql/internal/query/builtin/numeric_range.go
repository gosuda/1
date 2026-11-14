package builtin

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// NumericRangeWorkBytes is a hard workspace limit, not an estimated reservation.
const NumericRangeWorkBytes uint64 = 4 << 20

// NumericRange owns offset/boundary coefficients and borrows only its allocator.
type NumericRange struct {
	claim            columnar.Allocation
	work             *decimalint.Workspace
	offset, bound    decimal
	ready, ambiguous bool
}

func NewNumericRange(ctx context.Context, offset model.Value, allocator columnar.Allocator) (*NumericRange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if allocator == nil {
		return nil, sqlError("XX000", "numeric RANGE requires an allocator")
	}
	header := uint64(unsafe.Sizeof(NumericRange{}))
	claim, err := allocator.Reserve(header)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, sqlError("XX000", "numeric RANGE allocator returned no reservation")
	}
	work, err := decimalint.NewWorkspace(ctx, allocator, NumericRangeWorkBytes-header)
	if err != nil {
		claim.Release()
		return nil, numericAllocationError(err)
	}
	mark := work.Checkpoint()
	parsed, err := parseNumericRangeValue(ctx, work, offset)
	if err == nil && (parsed.special == numericNaN || numericSign(parsed) < 0) {
		err = sqlError("22013", "invalid preceding or following size in window function")
	}
	if err == nil {
		err = work.ReleaseAfter(mark, parsed.coefficient)
	}
	err = errors.Join(err, ctx.Err())
	if err != nil {
		err = errors.Join(err, work.Close())
		claim.Release()
		return nil, numericAllocationError(err)
	}
	return &NumericRange{claim: claim, work: work, offset: parsed}, nil
}

// A failed SetBase invalidates the old boundary without discarding the offset.
func (r *NumericRange) SetBase(ctx context.Context, base model.Value, subtract bool) (err error) {
	if r == nil || r.work == nil {
		return sqlError("XX000", "numeric RANGE is closed or uninitialized")
	}
	if r.bound.coefficient != nil && r.bound.coefficient != r.offset.coefficient {
		err = r.bound.coefficient.Close()
	}
	r.bound = decimal{}
	r.ready = false
	r.ambiguous = false
	if err != nil {
		return err
	}
	mark := r.work.Checkpoint()
	defer func() {
		keep := r.bound.coefficient
		if err != nil {
			keep = nil
			r.bound = decimal{}
			r.ready = false
		}
		cleanup := r.work.ReleaseAfter(mark, keep)
		if cleanup != nil {
			r.bound = decimal{}
			r.ready = false
			cleanup = errors.Join(cleanup, r.work.ReleaseAfter(mark))
		}
		err = numericAllocationError(errors.Join(err, cleanup))
	}()
	parsed, err := parseNumericRangeValue(ctx, r.work, base)
	if err != nil {
		return err
	}
	ambiguous := r.offset.special == numericPositiveInfinity && ((subtract && parsed.special == numericPositiveInfinity) || (!subtract && parsed.special == numericNegativeInfinity))
	op := opPl
	if subtract {
		op = opMi
	}
	bound, err := numericArithmetic(ctx, r.work, parsed, r.offset, op)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.bound = bound
	r.ambiguous = ambiguous
	r.ready = true
	return nil
}

func (r *NumericRange) Compare(ctx context.Context, value model.Value) (result int, err error) {
	if r == nil || r.work == nil {
		return 0, sqlError("XX000", "numeric RANGE is closed or uninitialized")
	}
	if !r.ready {
		return 0, sqlError("XX000", "numeric RANGE has no current boundary")
	}
	mark := r.work.Checkpoint()
	defer func() { err = numericAllocationError(errors.Join(err, r.work.ReleaseAfter(mark))) }()
	parsed, err := parseNumericRangeValue(ctx, r.work, value)
	if err != nil {
		return 0, err
	}
	if r.ambiguous {
		if parsed.special == numericNaN {
			return 1, ctx.Err()
		}
		return 0, ctx.Err()
	}
	return numericCompare(ctx, parsed, r.bound)
}

func (r *NumericRange) Close() error {
	if r == nil {
		return nil
	}
	work, claim := r.work, r.claim
	*r = NumericRange{}
	var err error
	if work != nil {
		err = work.Close()
	}
	if claim != nil {
		claim.Release()
	}
	return err
}

func parseNumericRangeValue(ctx context.Context, work *decimalint.Workspace, value model.Value) (decimal, error) {
	if err := ctx.Err(); err != nil {
		return decimal{}, err
	}
	if value.Type != model.TypeNumeric {
		return decimal{}, sqlError("42804", "numeric RANGE requires numeric values")
	}
	if value.IsNull {
		return decimal{}, sqlError("22004", "numeric RANGE requires non-NULL values")
	}
	parsed, err := parseNumeric(ctx, work, value.StrVal)
	if err != nil {
		return decimal{}, err
	}
	if parsed.special == numericFinite && parsed.scale > numericMaxScale {
		return decimal{}, numericOverflow()
	}
	return parsed, ctx.Err()
}
