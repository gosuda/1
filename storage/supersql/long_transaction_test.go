package supersql

import (
	"context"
	"fmt"
	"testing"
)

// TestLongTransactionCompletes runs many writing statements inside one explicit
// block. A transaction retains the normalized write set of every statement in
// it, so the transaction budget must cover what a whole block writes; the
// PostgreSQL regression corpus runs blocks of this shape (numeric.sql and
// copy.sql both do), and a budget smaller than one block's write set rejects
// them with SQLSTATE 53200.
func TestLongTransactionCompletes(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(ctx, "CREATE TABLE long_txn (id int PRIMARY KEY, v int)", 10000); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := database.Exec(ctx, "BEGIN", 10000); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	const statements, rows = 5, 200
	for i := range statements {
		statement := fmt.Sprintf("INSERT INTO long_txn SELECT g + %d, g FROM generate_series(1, %d) g", i*rows, rows)
		if _, err := database.Exec(ctx, statement, 10000); err != nil {
			t.Fatalf("statement %d of %d: %v", i+1, statements, err)
		}
	}
	if _, err := database.Exec(ctx, "COMMIT", 10000); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	outcome, err := database.Exec(ctx, "SELECT count(*) FROM long_txn", 10000)
	if err != nil {
		t.Fatalf("SELECT count(*): %v", err)
	}
	if len(outcome.Rows) != 1 || len(outcome.Rows[0]) != 1 || outcome.Rows[0][0].IntVal != statements*rows {
		t.Fatalf("rows after commit = %+v, want %d", outcome.Rows, statements*rows)
	}
}
