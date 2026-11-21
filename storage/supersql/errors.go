package supersql

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Error is a structured SQL diagnostic returned by the engine and its
// database/sql driver. Code carries the five-character PostgreSQL SQLSTATE.
type Error struct {
	Code       string
	Message    string
	Detail     string
	Hint       string
	Constraint string
	Table      string
	Column     string
	FeatureID  string
	Position   int
	Cause      error
}

// Error returns the diagnostic message.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

// Unwrap returns the underlying cause error, if any.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SQLState returns the five-character PostgreSQL SQLSTATE code carried by the
// diagnostic.
func (e *Error) SQLState() string {
	if e == nil {
		return ""
	}
	return e.Code
}

// SQLState returns the PostgreSQL SQLSTATE carried by an error produced by the
// engine, or the empty string when the error has no SQLSTATE.
func SQLState(err error) string {
	var sqlErr *Error
	if errors.As(err, &sqlErr) {
		return sqlErr.SQLState()
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.SQLState()
	}
	return ""
}

// wrapError converts internal SQL diagnostics into the exported *Error type at
// public API and database/sql driver boundaries while preserving errors.Is and
// errors.As chains.
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return err
	}
	var typed *model.Error
	if !errors.As(err, &typed) {
		return err
	}
	return &Error{
		Code:       typed.Code,
		Message:    typed.Message,
		Detail:     typed.Detail,
		Hint:       typed.Hint,
		Constraint: typed.Constraint,
		Table:      typed.Table,
		Column:     typed.Column,
		FeatureID:  typed.FeatureID,
		Position:   typed.Position,
		Cause:      err,
	}
}
