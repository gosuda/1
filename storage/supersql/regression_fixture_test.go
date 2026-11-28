package supersql

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	regFixtureFilename = regexp.MustCompile("(?i)^\\\\set\\s+filename\\s+:abs_srcdir\\s+'/data/([a-z0-9_.-]+)'\\s*$")
	regFixtureCopy     = regexp.MustCompile(`(?is)^(COPY\s+[a-z_][a-z_0-9]*(?:\s*\([^)]*\))?\s+FROM\s+):'filename'(\s*(?:WHERE\s+.+)?\s*;?\s*)$`)
)

func resolveRegressionFixtureCopies(corpus string, statements []regStatement) error {
	filename := ""
	for i := range statements {
		statement := &statements[i]
		text := stripLeadingComments(statement.text)
		if statement.kind == regClientCommand {
			fields := strings.Fields(text)
			if len(fields) >= 2 && fields[0] == "\\set" && fields[1] == "filename" {
				filename = ""
				if match := regFixtureFilename.FindStringSubmatch(text); match != nil {
					filename = filepath.Join(corpus, "data", match[1])
				}
			}
			continue
		}
		if filename == "" || statement.kind != regSQL {
			continue
		}
		match := regFixtureCopy.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("fixture COPY at line %d: %w", statement.line, err)
		}
		statement.text = match[1] + "stdin" + match[2]
		statement.data = string(data)
		statement.kind = regCopyIn
	}
	return nil
}

func TestRegressionFixtureCopyUsesOnlyDeclaredLocalData(t *testing.T) {
	corpus := t.TempDir()
	if err := os.Mkdir(filepath.Join(corpus, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "data", "sample.data"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statements, err := splitRegressionSQL("\\set filename :abs_srcdir '/data/sample.data'\nCOPY sample FROM :'filename';\n\\set filename '/other/sample.data'\nCOPY sample FROM :'filename';\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveRegressionFixtureCopies(corpus, statements); err != nil {
		t.Fatal(err)
	}
	if len(statements) != 4 || statements[1].kind != regCopyIn || statements[1].data != "7\n" {
		t.Fatalf("local fixture was not resolved: %+v", statements)
	}
	if statements[3].kind != regSQL || statements[3].data != "" {
		t.Fatalf("an unsupported filename reused the previous fixture: %+v", statements[3])
	}
	if err := os.Remove(filepath.Join(corpus, "data", "sample.data")); err != nil {
		t.Fatal(err)
	}
	missing, err := splitRegressionSQL("\\set filename :abs_srcdir '/data/sample.data'\nCOPY sample FROM :'filename';\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveRegressionFixtureCopies(corpus, missing); err == nil {
		t.Fatal("missing declared fixture must fail")
	}
}

func TestRegressionFixturesAndFourWorkerCursorCapacity(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock(), Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	scope, err := database.newSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	}()
	const corpus = "testdata/postgres184"
	setup, err := os.ReadFile(filepath.Join(corpus, "sql", "test_setup.sql"))
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitRegressionSQL(string(setup))
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveRegressionFixtureCopies(corpus, statements); err != nil {
		t.Fatal(err)
	}
	unsupported := unsupportedFeatureIDs(t)
	for _, statement := range statements {
		sql := strings.ToLower(stripLeadingComments(statement.text))
		selected := strings.HasPrefix(sql, "create table onek (") ||
			strings.HasPrefix(sql, "create table onek2 as") ||
			strings.HasPrefix(sql, "create table tenk1 (") ||
			strings.HasPrefix(sql, "copy onek from ") ||
			strings.HasPrefix(sql, "copy tenk1 from ")
		if !selected {
			continue
		}
		outcome, state, err := runRegressionStatement(t, database, scope, statement, unsupported)
		if err != nil || outcome != regPass {
			t.Fatalf("setup line %d: outcome=%d state=%s: %v", statement.line, outcome, state, err)
		}
	}
	for table, want := range map[string]int64{"onek": 1000, "onek2": 1000, "tenk1": 10000} {
		result, err := database.execIn(ctx, scope, "SELECT count(*) FROM "+table, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0].IntVal != want {
			t.Fatalf("%s fixture rows = %+v, want %d", table, result.Rows, want)
		}
	}
	portals, err := os.ReadFile(filepath.Join(corpus, "sql", "portals_p2.sql"))
	if err != nil {
		t.Fatal(err)
	}
	statements, err = splitRegressionSQL(string(portals))
	if err != nil {
		t.Fatal(err)
	}
	wantRows := []int64{50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 50, 60}
	fetched := 0
	for _, statement := range statements {
		result, err := database.execIn(ctx, scope, statement.text, 10000)
		if err != nil {
			t.Fatalf("four-worker portal line %d: state=%s: %v", statement.line, regressionSQLState(err), err)
		}
		if firstSQLWord(statement.text) == "fetch" {
			if fetched >= len(wantRows) || len(result.Rows) != 1 || len(result.Rows[0]) != 16 || result.Rows[0][0].IntVal != wantRows[fetched] {
				t.Fatalf("portal fetch %d: rows=%+v", fetched, result.Rows)
			}
			fetched++
		}
	}
	if fetched != len(wantRows) || scope.txns.Active() {
		t.Fatalf("fetched=%d active=%t, want thirteen closed cursors and a committed transaction", fetched, scope.txns.Active())
	}
	for _, query := range []struct {
		sql   string
		state string
	}{
		{"BEGIN", ""},
		{"DECLARE c CURSOR FOR SELECT unique2/0 FROM tenk1 ORDER BY unique2", ""},
		{"SAVEPOINT two", ""},
		{"FETCH 10 FROM c", "22012"},
		{"ROLLBACK TO SAVEPOINT two", ""},
		{"FETCH 10 FROM c", "55000"},
		{"ROLLBACK TO SAVEPOINT two", ""},
		{"RELEASE SAVEPOINT two", ""},
		{"FETCH 10 FROM c", "55000"},
		{"ROLLBACK", ""},
	} {
		_, err := database.execIn(ctx, scope, query.sql, 10000)
		if got := regressionSQLState(err); got != query.state || (err != nil) != (query.state != "") {
			t.Fatalf("populated cursor %q: state=%q error=%v, want %q", query.sql, got, err, query.state)
		}
	}
}

func TestRegressionScheduleLoadsCopyFixtures(t *testing.T) {
	t.Setenv("SUPERSQL_REGRESS_FILES", "")
	t.Setenv("SUPERSQL_REGRESS_DUMP", "")
	t.Setenv("SUPERSQL_REGRESS_PREAMBLE", "")
	corpus := t.TempDir()
	for _, directory := range []string{"sql", "expected", "data", "baseline"} {
		if err := os.Mkdir(filepath.Join(corpus, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{
		"parallel_schedule":    "test: fixture\n",
		"sql/fixture.sql":      "CREATE TABLE fixture (v integer);\n\\set filename :abs_srcdir '/data/fixture.data'\nCOPY fixture FROM :'filename';\nSELECT 10 / v FROM fixture;\n",
		"expected/fixture.out": "CREATE TABLE fixture (v integer);\nCOPY fixture FROM :'filename';\nSELECT 10 / v FROM fixture;\nERROR: division by zero\n",
		"data/fixture.data":    "0\n",
		"baseline/fixture.txt": "1 pass\n2 client\n3 pass\n4 expected_error\n",
	} {
		if err := os.WriteFile(filepath.Join(corpus, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runRegressionSchedule(t, corpus, unsupportedFeatureIDs(t))
}
