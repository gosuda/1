package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSoftTypeInputErrorBoundary(t *testing.T) {
	data := &model.Error{Code: "22001", Message: "value too long"}
	constraint := &model.Error{Code: "23514", Message: "domain check failed"}
	unsafe := &model.Error{Code: "42501", Message: "permission denied"}
	unique := &model.Error{Code: "23505", Message: "unique constraint violated"}
	malformed := &model.Error{Code: "22", Message: "invalid SQLSTATE"}
	resource := &model.Error{Code: "54000", Message: "budget exhausted", Cause: model.ErrValueBudget}
	for _, test := range []struct {
		name string
		err  error
		soft bool
	}{
		{name: "data exception", err: data, soft: true},
		{name: "constraint violation", err: constraint, soft: true},
		{name: "unique constraint", err: unique},
		{name: "unsafe callback", err: unsafe},
		{name: "malformed data exception", err: malformed},
		{name: "resource", err: resource},
		{name: "cancellation", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "untyped", err: errors.New("callback failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, soft := softTypeInputError(test.err)
			if soft != test.soft {
				t.Fatalf("soft=%v want %v", soft, test.soft)
			}
			if soft && state != test.err {
				t.Fatalf("state=%v want %v", state, test.err)
			}
		})
	}
}
