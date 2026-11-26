package supersql

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestTemporaryRelationLifecycle covers statements that address a
// session-temporary relation. Temporary descriptors live only in the session
// overlay, so binding, dependency declaration, key encoding and write policy all
// have to reach them without a catalog row: the corpus exercises this in
// char.sql, copy.sql, aggregates.sql and dozens of other files.
func TestTemporaryRelationLifecycle(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	steps := []struct {
		statement string
		wantError bool
	}{
		// A temporary table is created, written and read across statements.
		// Each statement runs in its own transaction, so the write resolves its
		// row policy with no state left from the creating transaction.
		{statement: "CREATE TEMP TABLE char_tbl (f1 char)"},
		{statement: "INSERT INTO char_tbl (f1) VALUES ('a')"},
		{statement: "INSERT INTO char_tbl (f1) VALUES ('A')"},
		{statement: "SELECT * FROM char_tbl"},
		// A second temporary relation in the same session, addressed while the
		// first still exists.
		{statement: "CREATE TEMP TABLE second_tbl (a int, b text)"},
		{statement: "INSERT INTO second_tbl VALUES (1, 'x'), (2, 'y')"},
		{statement: "SELECT a FROM second_tbl WHERE a > 1"},
		// An index on a temporary relation is maintained by the same write path.
		{statement: "CREATE TEMP TABLE keyed_tbl (id int PRIMARY KEY, name text)"},
		{statement: "INSERT INTO keyed_tbl VALUES (1, 'one')"},
		{statement: "INSERT INTO keyed_tbl VALUES (2, 'two')"},
		{statement: "SELECT name FROM keyed_tbl WHERE id = 2"},
		// Dropping one temporary relation leaves the others addressable: the
		// dropped id must stay recognized as session-local.
		{statement: "DROP TABLE second_tbl"},
		{statement: "SELECT * FROM char_tbl"},
		{statement: "INSERT INTO char_tbl (f1) VALUES ('b')"},
		{statement: "SELECT * FROM keyed_tbl"},
		// A dropped temporary relation is gone, and a permanent relation of the
		// same name is unaffected.
		{statement: "SELECT * FROM second_tbl", wantError: true},
		{statement: "CREATE TABLE permanent_tbl (id int)"},
		{statement: "INSERT INTO permanent_tbl VALUES (7)"},
		{statement: "SELECT * FROM permanent_tbl"},
	}
	for _, step := range steps {
		_, err := database.Exec(ctx, step.statement, 10000)
		if step.wantError {
			if err == nil {
				t.Errorf("%s: succeeded, want an error", step.statement)
			}
			continue
		}
		if err != nil {
			var typed *model.Error
			code := ""
			if errors.As(err, &typed) {
				code = typed.Code
			}
			t.Errorf("%s: %s %v", step.statement, code, err)
		}
	}
}

// TestTemporaryRelationShadowsPermanentName checks pg_temp precedence: while a
// session owns a temporary relation, an unqualified name resolves to it ahead of
// a permanent relation with the same name.
func TestTemporaryRelationShadowsPermanentName(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.Exec(ctx, "CREATE TABLE shadow_tbl (v int)", 10000); err != nil {
		t.Fatalf("create permanent: %v", err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO shadow_tbl VALUES (1)", 10000); err != nil {
		t.Fatalf("insert permanent: %v", err)
	}
	if _, err := database.Exec(ctx, "CREATE TEMP TABLE shadow_tbl (v int)", 10000); err != nil {
		t.Fatalf("create temporary: %v", err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO shadow_tbl VALUES (2)", 10000); err != nil {
		t.Fatalf("insert temporary: %v", err)
	}
	outcome, err := database.Exec(ctx, "SELECT v FROM shadow_tbl", 10000)
	if err != nil {
		t.Fatalf("select temporary: %v", err)
	}
	if len(outcome.Rows) != 1 || outcome.Rows[0][0].IntVal != 2 {
		t.Fatalf("unqualified name resolved to %v, want the temporary relation's single row 2", outcome.Rows)
	}
	outcome, err = database.Exec(ctx, "SELECT v FROM public.shadow_tbl", 10000)
	if err != nil {
		t.Fatalf("select permanent: %v", err)
	}
	if len(outcome.Rows) != 1 || outcome.Rows[0][0].IntVal != 1 {
		t.Fatalf("qualified name resolved to %v, want the permanent relation's single row 1", outcome.Rows)
	}
}
