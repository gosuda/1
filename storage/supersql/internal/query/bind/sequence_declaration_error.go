package bind

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// SequenceOwnershipError identifies semantic OWNED BY checks performed after
// PostgreSQL has validated allocation parameters and discarded its local cache.
// Resource and cancellation failures are not deferred.
type SequenceOwnershipError struct{ Cause error }

func (e *SequenceOwnershipError) Error() string {
	if e == nil || e.Cause == nil {
		return "sequence ownership validation failed"
	}
	return e.Cause.Error()
}
func (e *SequenceOwnershipError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func sequenceOwnershipError(err error) error {
	var sql *model.Error
	if !errors.As(err, &sql) {
		return err
	}
	switch sql.Code {
	case "42601", "42602", "42622", "42703", "42P01", "42809", "42501", "55000":
		return &SequenceOwnershipError{Cause: err}
	}
	return err
}
