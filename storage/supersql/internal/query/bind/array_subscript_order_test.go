package bind_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestArraySubscriptBindsLowerBeforeUpper(t *testing.T) {
	const sql = `SELECT (ARRAY[1])[missing_lower:missing_upper]`
	_, err := bindSQL(t, sql, nil, nil)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "42703" {
		t.Fatalf("bound column error = %v; want 42703", err)
	}
	if want := strings.Index(sql, "missing_lower") + 1; sqlErr.Position != want {
		t.Fatalf("error position = %d; want lower-bound position %d", sqlErr.Position, want)
	}
}
