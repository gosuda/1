package supersql

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

func TestCIFixExpectedErrorTrailingComments(t *testing.T) {
	source := strings.Join([]string{
		"SELECT 1; -- should work",
		"SELECT 2; -- should fail",
		"SELECT 3;",
	}, "\n")
	stmts, err := splitRegressionSQL(source)
	if err != nil {
		t.Fatalf("splitRegressionSQL: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if strings.Contains(stmts[1].text, "should work") {
		t.Fatalf("trailing comment bled onto next statement: %q", stmts[1].text)
	}
	if strings.Contains(stmts[2].text, "should fail") {
		t.Fatalf("trailing comment bled onto third statement: %q", stmts[2].text)
	}

	out := strings.Join([]string{
		"SELECT 1; -- should work",
		" ?column? ",
		"----------",
		"        1",
		"(1 row)",
		"",
		"SELECT 2; -- should fail",
		"ERROR:  boom",
		"SELECT 3;",
		" ?column? ",
		"----------",
		"        3",
		"(1 row)",
	}, "\n")
	corpus := t.TempDir()
	if err := os.Mkdir(filepath.Join(corpus, "expected"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "expected", "trailing.out"), []byte(out), 0600); err != nil {
		t.Fatal(err)
	}
	got := expectedErrorSet(t, corpus, "trailing", stmts)
	for i, want := range []bool{false, true, false} {
		if got[i] != want {
			t.Errorf("statement %d expected error = %v, want %v", i, got[i], want)
		}
	}
}

func TestCIFixDomainAndCompositeAlterPreserveCatalog(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// 1. Altering a composite type referenced by a domain must fail cleanly with
	// 2BP01 instead of mutating the composite version and bricking subsequent DDL.
	for _, sql := range []string{
		`CREATE TYPE comptype AS (r float8, i float8)`,
		`CREATE DOMAIN dcomptype AS comptype`,
		`ALTER DOMAIN dcomptype ADD CONSTRAINT c1 CHECK ((value).r > 0)`,
	} {
		if _, err := db.exec(ctx, sql, 0); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := db.exec(ctx, `ALTER TYPE comptype ALTER ATTRIBUTE r TYPE varchar`, 0); sqlStateOf(err) != "2BP01" {
		t.Fatalf("ALTER TYPE on composite used by domain: got sqlstate %q (%v), want 2BP01", sqlStateOf(err), err)
	}
	if _, err := db.exec(ctx, `DROP TYPE comptype CASCADE`, 0); err != nil {
		t.Fatalf("catalog bricked after rejected ALTER TYPE: %v", err)
	}

	// 2. Adding a constraint to a base domain must not invalidate a child domain's
	// CHECK constraint or brick subsequent statements.
	for _, sql := range []string{
		`CREATE DOMAIN posint AS int CHECK (value >= 0)`,
		`CREATE DOMAIN posint2 AS posint CHECK (value % 2 = 0)`,
		`CREATE TABLE ddtest2 (f1 posint2)`,
		`INSERT INTO ddtest2 VALUES (2)`,
		`ALTER DOMAIN posint ADD CONSTRAINT c2 CHECK (value > 0)`,
		`DROP TABLE ddtest2`,
		`DROP DOMAIN posint CASCADE`,
	} {
		if _, err := db.exec(ctx, sql, 0); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func TestCIFixSQLCompatAndFeatureIDTags(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// 1. json_populate_record / jsonb_populate_record with anonymous ROW(1,2).
	for _, sql := range []string{
		`SELECT json_populate_record(row(1,2), '{"f1": 0, "f2": 1}')`,
		`SELECT jsonb_populate_record(row(1,2), '{"f1": 0, "f2": 1}')`,
		`SELECT json_populate_recordset(row(1,2), '[{"f1": 0, "f2": 1}]')`,
		`SELECT jsonb_populate_recordset(row(1,2), '[]')`,
	} {
		if _, err := db.exec(ctx, sql, 0); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// 2. Single-row INSERT ... VALUES with SRF.
	if _, err := db.exec(ctx, `CREATE TABLE srf_vals (id int, label text)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.exec(ctx, `INSERT INTO srf_vals VALUES (generate_series(1, 3), 'ok')`, 0); err != nil {
		t.Fatalf("INSERT VALUES SRF: %v", err)
	}
	res, err := db.exec(ctx, `SELECT count(*) FROM srf_vals`, 0)
	if err != nil {
		t.Fatalf("SELECT count: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 {
		t.Fatalf("expected 3 rows inserted via VALUES SRF, got %+v", res.Rows)
	}

	// 3. jsonpath timestamptz -> timetz without _tz().
	if _, err := db.exec(ctx, `SELECT jsonb_path_query('"2023-08-15 12:34:56 +05:30"', '$.time_tz()')`, 0); err != nil {
		t.Fatalf("jsonb_path_query time_tz: %v", err)
	}

	// 4. Standalone DROP CONSTRAINT ... CASCADE.
	for _, sql := range []string{
		`CREATE TABLE cdrop (a int CONSTRAINT cdrop_chk CHECK (a > 0))`,
		`ALTER TABLE cdrop DROP CONSTRAINT cdrop_chk CASCADE`,
	} {
		if _, err := db.exec(ctx, sql, 0); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// 5. Intentional 0A000 rejections must carry their manifest FeatureID.
	if _, err := db.exec(ctx, `CREATE TABLE gin_t (a int[], b int[], j jsonb, n name)`, 0); err != nil {
		t.Fatalf("CREATE TABLE gin_t: %v", err)
	}
	for _, tc := range []struct {
		sql       string
		featureID bind.FeatureID
	}{
		{`CREATE INDEX ON gin_t USING gin (a, b)`, bind.NonBtreeIndexes},
		{`CREATE INDEX ON gin_t USING gin ((j->'k'))`, bind.NonBtreeIndexes},
		{`CREATE INDEX ON gin_t USING btree (n name_ops)`, bind.NonBtreeIndexes},
		{`SELECT pg_clear_relation_stats('public', 'gin_t')`, bind.PhysicalMaintenance},
		{`CREATE TEXT SEARCH CONFIGURATION custom_cfg (PARSER = custom_prs)`, bind.ExternalCode},
	} {
		_, err := db.exec(ctx, tc.sql, 0)
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != "0A000" || typed.FeatureID != string(tc.featureID) {
			t.Fatalf("%s: got err=%v, want SQLSTATE 0A000 FeatureID=%q", tc.sql, err, tc.featureID)
		}
	}
}
