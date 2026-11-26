package supersql

import (
	"testing"
)

// TestSplitRegressionBackslashClient pins the psql `\;` invariant from
// transactions.sql: backslash-quoted semicolons do not split statements;
// the multi-statement Query is client input, not single-statement SQL.
func TestSplitRegressionBackslashClient(t *testing.T) {
	stmts, err := splitRegressionSQL("SELECT 1\\; SELECT 2\\; SELECT 3;\n")
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d: %+v", len(stmts), stmts)
	}
	if stmts[0].kind != regClientCommand {
		t.Fatalf("expected client command, got kind %d text %q", stmts[0].kind, stmts[0].text)
	}
}

// TestSplitRegressionCopyBatchClient pins that psql multi-command COPY
// batches (copyselect.sql: psql handling of COPY in multi-command strings)
// are client input even though they start with COPY: content sniffing for
// COPY must not overwrite the psql multi-statement classification.
func TestSplitRegressionCopyBatchClient(t *testing.T) {
	for _, src := range []string{
		"copy (select 1) to stdout\\; select 1/0;\n",
		"select 0\\; copy test3 from stdin\\; copy test3 from stdin\\; select 1;\n",
	} {
		stmts, err := splitRegressionSQL(src)
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		if len(stmts) != 1 {
			t.Fatalf("expected 1 statement, got %d: %+v", len(stmts), stmts)
		}
		if stmts[0].kind != regClientCommand {
			t.Fatalf("expected client command, got kind %d text %q", stmts[0].kind, stmts[0].text)
		}
	}
}

// TestDependent25P02Cascade pins the session-aware dependent rule: after one
// root failure in a session, later statements failing with 25P02 count as
// dependent, not as their own failures.
func TestDependent25P02Cascade(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer scope.close()
	if _, err := db.execIn(ctx, scope, `BEGIN;`, 10000); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `SELECT * FROM definitely_missing_table_xyz;`, 10000); err == nil {
		t.Fatalf("expected root failure")
	} else if got := regressionSQLState(err); got != "42P01" {
		t.Fatalf("root state = %q, want 42P01 (%v)", got, err)
	}
	outcome, state, err := runRegressionStatement(t, db, scope, regStatement{text: `SELECT 1;`}, unsupportedFeatureIDs(t))
	if err == nil {
		t.Fatalf("expected cascade failure, got nil")
	}
	if state != "25P02" {
		t.Fatalf("cascade state = %q, want 25P02 (%v)", state, err)
	}
	if outcome != regFailed {
		t.Fatalf("runRegressionStatement outcome = %d, want regFailed (%d)", outcome, regFailed)
	}
	if !dependentCodes[state] {
		t.Fatalf("dependentCodes[%q] = false, want true: cascade must count as dependent", state)
	}
	mapped := mapCascadeToDependent(outcome, state, true)
	if mapped != regDependent {
		t.Fatalf("schedule-mapped outcome = %d, want regDependent (%d)", mapped, regDependent)
	}
}
