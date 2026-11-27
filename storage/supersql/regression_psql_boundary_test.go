package supersql

import (
	"os"
	"strings"
	"testing"
)

// An inline \gset ends its query; later transaction control stays SQL.
func TestRegressionPsqlCommandKeepsFollowingTransaction(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		for _, command := range []string{"COMMIT", "ROLLBACK"} {
			for _, suffix := range []string{"", " marker_"} {
				source := strings.Join([]string{"BEGIN;", "SELECT 1 AS marker " + "\\" + "gset" + suffix, command + ";", "SELECT 2;", ""}, ending)
				statements, err := splitRegressionSQL(source)
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range statements {
					t.Logf("split kind=%d line=%d text=%q", statement.kind, statement.line, statement.text)
				}
				if len(statements) != 4 {
					t.Errorf("standalone transaction command %s was absorbed: got %d statements, want 4: %+v", command, len(statements), statements)
					continue
				}
				if statements[1].kind != regClientCommand || statements[2].kind != regSQL || statements[2].text != command || statements[2].line != 3 || statements[3].text != "SELECT 2" {
					t.Errorf("standalone transaction boundary: %+v", statements)
				}
			}
		}
	}
	source, err := os.ReadFile("testdata/postgres184/sql/txid.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitRegressionSQL(string(source))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	for _, statement := range statements {
		if statement.kind == regSQL {
			word := firstSQLWord(statement.text)
			if word == "commit" || word == "rollback" {
				seen[statement.line] = word
			}
		}
	}
	for i, line := range strings.Split(string(source), "\n") {
		text := strings.TrimSpace(line)
		if text == "COMMIT;" || text == "ROLLBACK;" {
			if seen[i+1] != strings.ToLower(strings.TrimSuffix(text, ";")) {
				t.Errorf("standalone transaction command in original txid.sql line%d missing: %s (%v)", i+1, text, seen)
			}
		}
	}
	t.Logf("original txid transaction boundaries: %v", seen)
}

// SQL quoting and comments own their embedded backslashes and newlines.
func TestRegressionPsqlCommandIgnoresSQLLexicalContents(t *testing.T) {
	cases := []struct {
		name, sql string
	}{
		{"ordinary-multiline", "SELECT\n1"},
		{"quoted-string", "SELECT '\\gset\ncontinued'"},
		{"dollar-body", "DO $body$ BEGIN\nRAISE NOTICE '\\gset';\nEND $body$"},
		{"block-comment", "SELECT /* \\gset\nstill a comment */ 1"},
		{"line-comment", "SELECT 1 -- \\gset\n+ 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statements, err := splitRegressionSQL(tc.sql + ";\nCOMMIT;\n")
			if err != nil {
				t.Fatal(err)
			}
			if len(statements) != 2 {
				t.Fatalf("SQL lexical contents changed boundaries: %+v", statements)
			}
			if statements[0].kind != regSQL || statements[0].text != tc.sql || statements[1].kind != regSQL || statements[1].text != "COMMIT" {
				t.Fatalf("SQL lexical contents became client input: %+v", statements)
			}
		})
	}
}

func TestRegressionPsqlPrintBufferStaysClient(t *testing.T) {
	// PostgreSQL18.4 proof45917871139 executes the retained query buffer
	// as rows3,4,5 before the independent SELECT9.
	input, err := os.ReadFile("testdata/postgres184/review/psql-print-buffer.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitRegressionSQL(string(input))
	if err != nil {
		t.Fatal(err)
	}
	buffers, sqlUnits := 0, 0
	for _, statement := range statements {
		if statement.kind == regClientCommand && strings.Contains(statement.text, "SELECT 3") &&
			strings.Contains(statement.text, "UNION SELECT 4") && strings.Contains(statement.text, "UNION SELECT 5") {
			buffers++
		}
		if statement.kind == regSQL {
			sqlUnits++
			if statement.text != "SELECT 9 AS after_buffer" {
				t.Errorf("query-buffer fragment became engine SQL: %q", statement.text)
			}
		}
	}
	if buffers != 1 || sqlUnits != 1 {
		t.Fatalf("query-buffer classification: buffers=%d SQL=%d, want1/1", buffers, sqlUnits)
	}
}
