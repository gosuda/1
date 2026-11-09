package normalize_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func TestArraySubscriptConstantErrorsArePlanningErrors(t *testing.T) {
	for _, sql := range []string{
		`SELECT (NULL::integer[])[1/0]`,
		`SELECT (NULL::integer[])[1/0:2]`,
		`SELECT (ARRAY[1])[NULL::integer][1/0]`,
		`SELECT (NULL::integer[])[1/n] FROM (VALUES(0))t(n)`,
		`SELECT (NULL::integer[])[1/n:2] FROM (SELECT 0 AS n)t`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := normalize.Normalize(t.Context(), bound(t, sql), options())
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
				t.Fatalf("constant bound error=%v, want planning 22012", err)
			}
		})
	}
}
