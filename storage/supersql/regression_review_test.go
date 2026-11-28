package supersql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRegressionReviewSession(t *testing.T) (*Catalog, *sessionScope) {
	t.Helper()
	database, err := Open(t.Context(), OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	scope, err := database.newSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	})
	return database, scope
}

func TestRegressionFailedHoldableCursorAllowsRecoveredCommit(t *testing.T) {
	database, scope := newRegressionReviewSession(t)
	for _, query := range []struct {
		sql   string
		state string
	}{
		{"CREATE TABLE hold_commit (v integer)", ""},
		{"BEGIN", ""},
		{"INSERT INTO hold_commit VALUES (7)", ""},
		{"DECLARE failed NO SCROLL CURSOR WITH HOLD FOR VALUES (1), (2), (3)", ""},
		{"DECLARE healthy NO SCROLL CURSOR WITH HOLD FOR VALUES (9), (10)", ""},
		{"FETCH 2 FROM failed", ""},
		{"SAVEPOINT saved", ""},
		{"FETCH BACKWARD 1 FROM failed", "55000"},
		{"ROLLBACK TO SAVEPOINT saved", ""},
		{"COMMIT", ""},
	} {
		result, err := database.execIn(t.Context(), scope, query.sql, 10000)
		if got := regressionSQLState(err); got != query.state || (err != nil) != (query.state != "") {
			t.Fatalf("recovered holdable cursor %q: state=%q error=%v, want %q", query.sql, got, err, query.state)
		}
		if query.sql == "FETCH 2 FROM failed" && (len(result.Rows) != 2 || result.Rows[0][0].IntVal != 1 || result.Rows[1][0].IntVal != 2) {
			t.Fatalf("cursor did not advance before its failure: %+v", result.Rows)
		}
	}
	result, err := database.execIn(t.Context(), scope, "SELECT v FROM hold_commit", 10000)
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0].IntVal != 7 {
		t.Fatalf("recovered commit lost its insert: rows=%+v error=%v", result.Rows, err)
	}
	if _, err := database.execIn(t.Context(), scope, "FETCH 1 FROM failed", 10000); regressionSQLState(err) != "34000" {
		t.Fatalf("failed cursor survived commit: %v", err)
	}
	result, err = database.execIn(t.Context(), scope, "FETCH ALL FROM healthy", 10000)
	if err != nil || len(result.Rows) != 2 || result.Rows[0][0].IntVal != 9 || result.Rows[1][0].IntVal != 10 {
		t.Fatalf("healthy holdable cursor did not survive: rows=%+v error=%v", result.Rows, err)
	}
}

func TestRegressionHoldableMaterializationFailureStillAbortsCommit(t *testing.T) {
	database, scope := newRegressionReviewSession(t)
	for _, query := range []struct {
		sql   string
		state string
	}{
		{"CREATE TABLE hold_commit (v integer)", ""},
		{"BEGIN", ""},
		{"INSERT INTO hold_commit VALUES (7)", ""},
		{"DECLARE pending CURSOR WITH HOLD FOR SELECT 10/v FROM (VALUES (1), (0)) AS inputs(v)", ""},
		{"COMMIT", "22012"},
	} {
		_, err := database.execIn(t.Context(), scope, query.sql, 10000)
		if got := regressionSQLState(err); got != query.state || (err != nil) != (query.state != "") {
			t.Fatalf("materialization %q: state=%q error=%v, want %q", query.sql, got, err, query.state)
		}
	}
	result, err := database.execIn(t.Context(), scope, "SELECT count(*) FROM hold_commit", 10000)
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0].IntVal != 0 {
		t.Fatalf("materialization failure committed its insert: rows=%+v error=%v", result.Rows, err)
	}
	if _, err := database.execIn(t.Context(), scope, "FETCH 1 FROM pending", 10000); regressionSQLState(err) != "34000" {
		t.Fatalf("unmaterialized cursor survived failed commit: %v", err)
	}
}

func TestRegressionFixtureCopyPreservesColumnsAndFilter(t *testing.T) {
	for _, test := range []struct {
		name    string
		create  string
		copy    string
		selects string
		rows    int
		columns int
	}{
		{
			"filter", "CREATE TABLE fixture (id integer, note text)",
			"COPY fixture FROM :'filename'\nWHERE (id < 2);",
			"SELECT id, note FROM fixture ORDER BY id", 1, 2,
		},
		{
			"columns", "CREATE TABLE fixture (extra integer DEFAULT 99, id integer, note text)",
			"COPY fixture(id, note) FROM :'filename';",
			"SELECT id, note, extra FROM fixture ORDER BY id", 2, 3,
		},
		{
			"columns_and_filter", "CREATE TABLE fixture (extra integer DEFAULT 99, id integer, note text)",
			"COPY fixture(id, note) FROM :'filename' WHERE (id < 2);",
			"SELECT id, note, extra FROM fixture ORDER BY id", 1, 3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, scope := newRegressionReviewSession(t)
			if _, err := database.execIn(t.Context(), scope, test.create, 10000); err != nil {
				t.Fatal(err)
			}
			corpus := t.TempDir()
			if err := os.Mkdir(filepath.Join(corpus, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(corpus, "data", "sample.data"), []byte("1\talpha\n2\tbeta\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			statements, err := splitRegressionSQL("\\set filename :abs_srcdir '/data/sample.data'\n" + test.copy + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if err := resolveRegressionFixtureCopies(corpus, statements); err != nil {
				t.Fatal(err)
			}
			if len(statements) != 2 || statements[1].kind != regCopyIn {
				t.Fatalf("fixture COPY was not resolved: %+v", statements)
			}
			if got, want := statements[1].text, strings.TrimSuffix(strings.Replace(test.copy, ":'filename'", "stdin", 1), ";"); got != want {
				t.Fatalf("COPY syntax changed: got %q, want %q", got, want)
			}
			outcome, state, err := runRegressionStatement(t, database, scope, statements[1], unsupportedFeatureIDs(t))
			if err != nil || outcome != regPass {
				t.Fatalf("COPY did not execute: outcome=%d state=%s error=%v", outcome, state, err)
			}
			result, err := database.execIn(t.Context(), scope, test.selects, 10000)
			if err != nil || len(result.Rows) != test.rows {
				t.Fatalf("COPY result: rows=%+v error=%v, want %d rows", result.Rows, err, test.rows)
			}
			for i, row := range result.Rows {
				if len(row) != test.columns || row[0].IntVal != int64(i+1) || (test.columns == 3 && row[2].IntVal != 99) {
					t.Fatalf("COPY row %d: %+v", i, row)
				}
			}
		})
	}
}
