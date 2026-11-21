package supersql

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// SQLState returns the PostgreSQL SQLSTATE carried by an error produced by the
// engine, or the empty string when the error has no SQLSTATE.
func SQLState(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.SQLState()
	}
	return ""
}
