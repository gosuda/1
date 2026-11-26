package builtin

import (
	"context"
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Timetz uses PostgreSQL's twelve-byte wire representation: local microseconds
// followed by signed seconds west of UTC, both big-endian. Arithmetic zone
// conversions can produce the full int32 displacement domain.
func timetzValue(microseconds int64, secondsWest int32) (model.Value, error) {
	if microseconds < 0 || microseconds > intervalMicrosPerDay {
		return model.Value{}, sqlError("22008", "time out of range")
	}
	output := make([]byte, 12)
	binary.BigEndian.PutUint64(output, uint64(microseconds))
	binary.BigEndian.PutUint32(output[8:], uint32(secondsWest))
	return model.BytesVal(output), nil
}

func timetzResult(ctx context.Context, microseconds int64, secondsWest int32, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if err := outputBudget(12, budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if budget.MemoryBytes < 12 {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(12)
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	return timetzValue(microseconds, secondsWest)
}

// DecodeTimetz borrows the datum and validates its wire width and local time.
// Text input's stricter timezone limit does not apply to internal values.
func DecodeTimetz(value []byte) (microseconds int64, secondsWest int32, err error) {
	if len(value) != 12 {
		return 0, 0, sqlError("22P03", "invalid binary representation for time with time zone")
	}
	microseconds = int64(binary.BigEndian.Uint64(value))
	secondsWest = int32(binary.BigEndian.Uint32(value[8:]))
	if microseconds < 0 || microseconds > intervalMicrosPerDay {
		return 0, 0, sqlError("22008", "time out of range")
	}
	return microseconds, secondsWest, nil
}
