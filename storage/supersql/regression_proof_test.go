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

// TestSplitRegressionPipelineClient pins that mid-line psql backslash
// commands are client input: \bind and \sendpipeline (psql_pipeline.sql),
// \gset (copyencoding.sql, txid.sql, infinite_recurse.sql), and \g / \gx
// (psql.sql) execute in the client, so the engine must never see them.
func TestSplitRegressionPipelineClient(t *testing.T) {
	for _, src := range []string{
		"SELECT $1 \\bind 'val1' \\sendpipeline\n",
		"SELECT $1, $2 \\bind 'val2' 'val3' \\sendpipeline\n",
		"SELECT getdatabaseencoding() <> 'UTF8'\n       AS skip_test \\gset\n",
		"SELECT txid_current() AS committed \\gset COMMIT\n",
		"SELECT 1 as one, 2 as two \\g\n",
		"SELECT 3 as three, 4 as four \\gx\n",
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

// TestSplitRegressionRuleParen pins that semicolons inside CREATE RULE
// bodies do not split statements (rules.sql, copydml.sql): the parenthesized
// action list is one statement.
func TestSplitRegressionRuleParen(t *testing.T) {
	stmts, err := splitRegressionSQL("create rule rtest_sys_upd as on update to rtest_system do also (\n\tupdate rtest_interface set sysname = new.sysname\n\t\twhere sysname = old.sysname;\n\tupdate rtest_admin set sysname = new.sysname\n\t\twhere sysname = old.sysname\n\t);\n")
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d: %+v", len(stmts), stmts)
	}
	if stmts[0].kind != regSQL {
		t.Fatalf("expected SQL statement, got kind %d text %q", stmts[0].kind, stmts[0].text)
	}
}

// TestSplitRegressionVarBrace pins that psql :{?var} interpolation checks
// (psql.sql) are client input: definedness tests execute in the client.
func TestSplitRegressionVarBrace(t *testing.T) {
	if !regVarPattern.MatchString("SELECT :{?i} AS i_is_defined") {
		t.Fatalf("var pattern misses :{?i} interpolation")
	}
	if !regVarPattern.MatchString("SELECT NOT :{?no_such_var} AS ok") {
		t.Fatalf("var pattern misses :{?no_such_var} interpolation")
	}
	// JSON object literals contain ":{", which is not psql interpolation
	// (json.sql, sqljson_jsontable.sql): they stay engine SQL.
	if regVarPattern.MatchString("select json_extract_path('{\"f2\":{\"f3\":1}}','f4')") {
		t.Fatalf("var pattern misfires on JSON object literal")
	}
}

// TestSplitRegressionConformingStrings pins that backslash escapes apply
// inside string literals while standard_conforming_strings is off
// (strings.sql): the escaped quote must not end the literal early.
func TestSplitRegressionConformingStrings(t *testing.T) {
	src := "set standard_conforming_strings = off;\nselect 'a\\\\b\\'cd' as f2;\nset standard_conforming_strings = on;\nselect 'a\\b\\'cd' as f2;\n"
	stmts, err := splitRegressionSQL(src)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 4 {
		t.Fatalf("expected 4 statements, got %d: %+v", len(stmts), stmts)
	}
	want := "select 'a\\\\b\\'cd' as f2"
	if stmts[1].text != want {
		t.Fatalf("off literal = %q, want %q", stmts[1].text, want)
	}
}

// TestSplitRegressionStarStarClient pins that '**' query-buffer notes
// (rules.sql) are client input: psql prints them with \p and discards them
// with \r, so the server never sees them.
func TestSplitRegressionStarStarClient(t *testing.T) {
	stmts, err := splitRegressionSQL("select * from rtest_v1;\n** Remember the delete rule\n** DO INSTEAD DELETE\n\\p\n\\r\ndelete from rtest_v1 where b = 12;\n")
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 6 {
		t.Fatalf("expected 6 statements, got %d: %+v", len(stmts), stmts)
	}
	for _, s := range stmts[1:5] {
		if s.kind != regClientCommand {
			t.Fatalf("expected client command, got kind %d text %q", s.kind, s.text)
		}
	}
	if stmts[0].kind != regSQL || stmts[5].kind != regSQL {
		t.Fatalf("SQL statements misclassified: %+v", stmts)
	}
}

// TestSplitRegressionClientParen pins that parentheses inside client input
// do not affect statement splitting (psql.sql): the \if test with an
// unbalanced paren is one client blob and the following SELECT stays SQL.
func TestSplitRegressionClientParen(t *testing.T) {
	stmts, err := splitRegressionSQL("select \\if false \\\\ (bogus \\else \\\\ 42 \\endif \\\\ forty_two;\nSELECT 1;\n")
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d: %+v", len(stmts), stmts)
	}
	if stmts[0].kind != regClientCommand {
		t.Fatalf("expected client command, got kind %d text %q", stmts[0].kind, stmts[0].text)
	}
	if stmts[1].kind != regSQL || stmts[1].text != "SELECT 1" {
		t.Fatalf("expected trailing SELECT, got kind %d text %q", stmts[1].kind, stmts[1].text)
	}
}

// TestSplitRegressionUEscape pins that U& strings honor UESCAPE (strings.sql):
// with standard_conforming_strings off, the backslash in U&' \'' is ordinary
// because UESCAPE '!' replaces it, so the string ends before UESCAPE and the
// following statements stay separate.
func TestSplitRegressionUEscape(t *testing.T) {
	src := "set standard_conforming_strings = off;\nSELECT U&' \\' UESCAPE '!' AS t;\nSELECT 2;\n"
	stmts, err := splitRegressionSQL(src)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d: %+v", len(stmts), stmts)
	}
	for _, s := range stmts {
		if s.kind != regSQL {
			t.Fatalf("expected SQL statement, got kind %d text %q", s.kind, s.text)
		}
	}
}
