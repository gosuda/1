package zpgparse_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestParserStackCapSQLOverflow(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{
			name: "nested_parens_5000",
			sql:  "SELECT " + strings.Repeat("(", 5000) + "1" + strings.Repeat(")", 5000),
		},
		{
			name: "nested_not_5000",
			sql:  "SELECT " + strings.Repeat("NOT ", 5000) + "true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := zpgparse.Parse(tc.sql)
			if err == nil {
				t.Fatalf("expected parser stack overflow error, got result %+v", res)
			}
			if !strings.Contains(err.Error(), "parser stack overflow") {
				t.Fatalf("err = %q, want substring %q", err.Error(), "parser stack overflow")
			}
			var parseErr *zpgparse.Error
			if !errors.As(err, &parseErr) {
				t.Fatalf("expected *zpgparse.Error, got %T", err)
			}
			if parseErr.SQLState() != "42601" {
				t.Fatalf("SQLState = %q, want %q", parseErr.SQLState(), "42601")
			}
		})
	}
}

func TestParserStackCapPLpgSQLOverflow(t *testing.T) {
	body := strings.Repeat("BEGIN ", 5000) + "NULL;" + strings.Repeat(" END;", 5000)
	cfg := zpgparse.PLpgSQLConfig{Inline: true, ReturnType: "void", Name: "inline_code_block"}

	t.Run("ParsePLpgSQL", func(t *testing.T) {
		fn, err := zpgparse.ParsePLpgSQL(body, cfg)
		if err == nil {
			t.Fatalf("expected parser stack overflow error, got function %+v", fn)
		}
		if !strings.Contains(err.Error(), "parser stack overflow") {
			t.Fatalf("err = %q, want substring %q", err.Error(), "parser stack overflow")
		}
		var parseErr *zpgparse.Error
		if !errors.As(err, &parseErr) {
			t.Fatalf("expected *zpgparse.Error, got %T", err)
		}
	})

	t.Run("ParsePlPgSqlToJSON", func(t *testing.T) {
		out, err := zpgparse.ParsePlPgSqlToJSON("DO $$ " + body + " $$;")
		if err == nil {
			t.Fatalf("expected parser stack overflow error, got JSON output of len %d", len(out))
		}
		if !strings.Contains(err.Error(), "parser stack overflow") {
			t.Fatalf("err = %q, want substring %q", err.Error(), "parser stack overflow")
		}
	})
}

func TestParserStackCapNormalNesting(t *testing.T) {
	t.Run("sql_50_parens", func(t *testing.T) {
		sql := "SELECT " + strings.Repeat("(", 50) + "1" + strings.Repeat(")", 50)
		res, err := zpgparse.Parse(sql)
		if err != nil {
			t.Fatalf("unexpected error parsing 50-level nested parens: %v", err)
		}
		if len(res.Stmts) != 1 {
			t.Fatalf("got %d statements, want 1", len(res.Stmts))
		}
	})

	t.Run("sql_500_parens_grows_stack", func(t *testing.T) {
		sql := "SELECT " + strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500)
		res, err := zpgparse.Parse(sql)
		if err != nil {
			t.Fatalf("unexpected error parsing 500-level nested parens: %v", err)
		}
		if len(res.Stmts) != 1 {
			t.Fatalf("got %d statements, want 1", len(res.Stmts))
		}
	})

	t.Run("plpgsql_50_blocks", func(t *testing.T) {
		body := strings.Repeat("BEGIN ", 50) + "NULL;" + strings.Repeat(" END;", 50)
		cfg := zpgparse.PLpgSQLConfig{Inline: true, ReturnType: "void", Name: "inline_code_block"}
		fn, err := zpgparse.ParsePLpgSQL(body, cfg)
		if err != nil {
			t.Fatalf("unexpected error parsing 50-level nested PL/pgSQL blocks: %v", err)
		}
		if fn == nil || fn.Action == nil {
			t.Fatalf("expected non-nil parsed PL/pgSQL function and action")
		}
	})

	t.Run("plpgsql_200_blocks_grows_stack", func(t *testing.T) {
		body := strings.Repeat("BEGIN ", 200) + "NULL;" + strings.Repeat(" END;", 200)
		cfg := zpgparse.PLpgSQLConfig{Inline: true, ReturnType: "void", Name: "inline_code_block"}
		fn, err := zpgparse.ParsePLpgSQL(body, cfg)
		if err != nil {
			t.Fatalf("unexpected error parsing 200-level nested PL/pgSQL blocks: %v", err)
		}
		if fn == nil || fn.Action == nil {
			t.Fatalf("expected non-nil parsed PL/pgSQL function and action")
		}
	})
}
