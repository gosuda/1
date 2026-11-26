package supersql_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	supersql "github.com/gosuda/supersql-alpha"
	localclock "github.com/gosuda/supersql-alpha/clock/local"
	"github.com/gosuda/supersql-alpha/pgx"
)

func readExplainPlan(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", query, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN %q: %v", query, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN %q rows.Err: %v", query, err)
	}
	return strings.Join(lines, "\n")
}

func assertBoundedPKScan(t *testing.T, label, plan string) {
	t.Helper()
	if !strings.Contains(plan, `"PrimaryKeyScan"`) && !strings.Contains(plan, `"SecondaryScan"`) {
		t.Fatalf("%s: expected index/PK scan, got plan:\n%s", label, plan)
	}
	if strings.Contains(plan, `"TableScan"`) {
		t.Fatalf("%s: unexpected TableScan in plan:\n%s", label, plan)
	}
	if !strings.Contains(plan, `"Unbounded":false`) {
		t.Fatalf("%s: expected bounded key span (\"Unbounded\":false), got plan:\n%s", label, plan)
	}
}

func TestAlphaGateOLTPExplainPlans(t *testing.T) {
	ctx := t.Context()
	connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
		Directory:   t.TempDir(),
		Clock:       localclock.New(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	for _, ddl := range []string{
		`CREATE TABLE pk_int (id bigint PRIMARY KEY, payload text NOT NULL)`,
		`CREATE TABLE pk_uuid (id uuid PRIMARY KEY, payload text NOT NULL)`,
		`CREATE TABLE pk_varchar (id varchar(64) PRIMARY KEY, payload text NOT NULL)`,
		`CREATE TABLE pk_numeric (id numeric(20, 2) PRIMARY KEY, payload text NOT NULL)`,
		`CREATE TABLE join_a (id bigint PRIMARY KEY, val text NOT NULL)`,
		`CREATE TABLE join_b (id bigint PRIMARY KEY, val text NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO pk_int SELECT g, 'row-' || g::text FROM generate_series(1, 200) AS g`); err != nil {
		t.Fatalf("seed pk_int: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO join_a SELECT g, 'a-' || g::text FROM generate_series(1, 100) AS g`); err != nil {
		t.Fatalf("seed join_a: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO join_b SELECT g, 'b-' || g::text FROM generate_series(1, 100) AS g`); err != nil {
		t.Fatalf("seed join_b: %v", err)
	}
	targetUUID := "550e8400-e29b-41d4-a716-446655440042"
	if _, err := db.ExecContext(ctx, `INSERT INTO pk_uuid VALUES ('550e8400-e29b-41d4-a716-446655440001', 'u1'), ($1, 'u42')`, targetUUID); err != nil {
		t.Fatalf("seed pk_uuid: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO pk_varchar VALUES ('alpha', 'v1'), ('omega', 'v2')`); err != nil {
		t.Fatalf("seed pk_varchar: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO pk_numeric VALUES (10.25, 'n1'), (99999.99, 'n2')`); err != nil {
		t.Fatalf("seed pk_numeric: %v", err)
	}

	// 1. $1 parameterized PK lookup uses bounded PrimaryKeyScan (not full scan)
	paramPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT payload FROM pk_int WHERE id = $1`, int64(42))
	assertBoundedPKScan(t, "$1 int PK lookup", paramPlan)
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM pk_int WHERE id = $1`, int64(42)).Scan(&payload); err != nil || payload != "row-42" {
		t.Fatalf("SELECT pk_int $1 = %q, %v", payload, err)
	}

	// 2. UUID, VARCHAR, and NUMERIC PK lookups use bounded PrimaryKeyScan
	uuidPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT payload FROM pk_uuid WHERE id = $1`, targetUUID)
	assertBoundedPKScan(t, "UUID PK lookup", uuidPlan)
	if err := db.QueryRowContext(ctx, `SELECT payload FROM pk_uuid WHERE id = $1`, targetUUID).Scan(&payload); err != nil || payload != "u42" {
		t.Fatalf("SELECT pk_uuid = %q, %v", payload, err)
	}

	varcharPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT payload FROM pk_varchar WHERE id = $1`, "omega")
	assertBoundedPKScan(t, "VARCHAR PK lookup", varcharPlan)
	if err := db.QueryRowContext(ctx, `SELECT payload FROM pk_varchar WHERE id = $1`, "omega").Scan(&payload); err != nil || payload != "v2" {
		t.Fatalf("SELECT pk_varchar = %q, %v", payload, err)
	}

	numericPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT payload FROM pk_numeric WHERE id = $1`, "99999.99")
	assertBoundedPKScan(t, "NUMERIC PK lookup", numericPlan)
	if err := db.QueryRowContext(ctx, `SELECT payload FROM pk_numeric WHERE id = $1`, "99999.99").Scan(&payload); err != nil || payload != "n2" {
		t.Fatalf("SELECT pk_numeric = %q, %v", payload, err)
	}

	// 3. Reversed join keys (ON b.id = a.id and ON b.val = a.val) use MergeJoin/HashJoin (not NestedLoopJoin)
	pkJoinPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT a.id, b.val FROM join_a a JOIN join_b b ON b.id = a.id`)
	if !strings.Contains(pkJoinPlan, `"MergeJoin"`) && !strings.Contains(pkJoinPlan, `"HashJoin"`) {
		t.Fatalf("reversed PK join ON b.id = a.id: expected MergeJoin or HashJoin, got:\n%s", pkJoinPlan)
	}
	if strings.Contains(pkJoinPlan, `"NestedLoopJoin"`) {
		t.Fatalf("reversed PK join ON b.id = a.id: unexpected NestedLoopJoin:\n%s", pkJoinPlan)
	}
	nonPKJoinPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT a.id, b.val FROM join_a a JOIN join_b b ON b.val = a.val`)
	if !strings.Contains(nonPKJoinPlan, `"HashJoin"`) {
		t.Fatalf("reversed non-PK join ON b.val = a.val: expected HashJoin, got:\n%s", nonPKJoinPlan)
	}

	// 4. ORDER BY pk DESC LIMIT 5 uses reverse scan (Scan.Direction: 1) without Sort
	revPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT id FROM pk_int ORDER BY id DESC LIMIT 5`)
	if !strings.Contains(revPlan, `"Index":0,"Direction":1`) {
		t.Fatalf("ORDER BY id DESC LIMIT 5: expected reverse scan (\"Index\":0,\"Direction\":1), got:\n%s", revPlan)
	}
	if strings.Contains(revPlan, `"Sort"`) {
		t.Fatalf("ORDER BY id DESC LIMIT 5: unexpected Sort operator in plan:\n%s", revPlan)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM pk_int ORDER BY id DESC LIMIT 5`)
	if err != nil {
		t.Fatalf("ORDER BY DESC LIMIT 5 query: %v", err)
	}
	var gotDesc []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		gotDesc = append(gotDesc, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantDesc := []int64{200, 199, 198, 197, 196}
	if fmt.Sprint(gotDesc) != fmt.Sprint(wantDesc) {
		t.Fatalf("ORDER BY id DESC LIMIT 5 = %v, want %v", gotDesc, wantDesc)
	}

	// 5. ANALYZE updates estimated rows in EXPLAIN
	rowsRe := regexp.MustCompile(`"Rows":([0-9]+)`)
	beforePlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT * FROM pk_int`)
	beforeMatch := rowsRe.FindStringSubmatch(beforePlan)
	if len(beforeMatch) < 2 {
		t.Fatalf("before ANALYZE missing Rows estimate:\n%s", beforePlan)
	}
	beforeRows, _ := strconv.Atoi(beforeMatch[1])

	if _, err := db.ExecContext(ctx, `ANALYZE pk_int`); err != nil {
		t.Fatalf("ANALYZE pk_int: %v", err)
	}
	afterPlan := readExplainPlan(t, ctx, db, `EXPLAIN (FORMAT JSON) SELECT * FROM pk_int`)
	afterMatch := rowsRe.FindStringSubmatch(afterPlan)
	if len(afterMatch) < 2 {
		t.Fatalf("after ANALYZE missing Rows estimate:\n%s", afterPlan)
	}
	afterRows, _ := strconv.Atoi(afterMatch[1])
	if afterRows != 200 || afterRows == beforeRows {
		t.Fatalf("ANALYZE did not update estimated rows: before=%d after=%d want=200", beforeRows, afterRows)
	}
}

func TestAlphaGateLargeTOASTAndHotBackupRestore(t *testing.T) {
	ctx := t.Context()
	srcDir := t.TempDir()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
		Directory:   srcDir,
		Clock:       localclock.New(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE large_blobs (id bigint PRIMARY KEY, body text NOT NULL, raw bytea NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE large_blobs: %v", err)
	}

	// Build >5 MiB TEXT and >5 MiB BYTEA payloads with deterministic patterns.
	const textSize = 5*1024*1024 + 123456
	const rawSize = 5*1024*1024 + 654321
	var textBuf strings.Builder
	textBuf.Grow(textSize)
	for i := 0; textBuf.Len() < textSize; i++ {
		fmt.Fprintf(&textBuf, "line-%08d-alpha-toast-payload;", i)
	}
	wantText := textBuf.String()[:textSize]

	wantRaw := make([]byte, rawSize)
	for i := range wantRaw {
		wantRaw[i] = byte((i*31 + i/257) & 0xff)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO large_blobs (id, body, raw) VALUES ($1, $2, $3)`, int64(1), wantText, wantRaw); err != nil {
		t.Fatalf("INSERT >5 MiB TOAST row: %v", err)
	}

	var gotText string
	var gotRaw []byte
	if err := db.QueryRowContext(ctx, `SELECT body, raw FROM large_blobs WHERE id = $1`, int64(1)).Scan(&gotText, &gotRaw); err != nil {
		t.Fatalf("SELECT >5 MiB TOAST row: %v", err)
	}
	if len(gotText) != len(wantText) || gotText != wantText {
		t.Fatalf("TEXT mismatch: got len=%d, want len=%d", len(gotText), len(wantText))
	}
	if !bytes.Equal(gotRaw, wantRaw) {
		t.Fatalf("BYTEA mismatch: got len=%d, want len=%d", len(gotRaw), len(wantRaw))
	}

	// CreateHotBackup while DB is live, then RestoreCheckpoint into a fresh directory.
	if err := connector.Catalog().CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	restored, err := supersql.RestoreCheckpoint(ctx, backupDir, supersql.OpenOptions{
		Directory:   restoreDir,
		Clock:       localclock.New(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	defer restored.Close()

	restoredPool := pgx.NewPoolFromCatalog(restored)
	defer restoredPool.Close()

	var restoredText string
	var restoredRaw []byte
	if err := restoredPool.QueryRow(ctx, `SELECT body, raw FROM large_blobs WHERE id = $1`, int64(1)).Scan(&restoredText, &restoredRaw); err != nil {
		t.Fatalf("SELECT from restored checkpoint: %v", err)
	}
	if len(restoredText) != len(wantText) || restoredText != wantText {
		t.Fatalf("restored TEXT mismatch: got len=%d, want len=%d", len(restoredText), len(wantText))
	}
	if !bytes.Equal(restoredRaw, wantRaw) {
		t.Fatalf("restored BYTEA mismatch: got len=%d, want len=%d", len(restoredRaw), len(wantRaw))
	}
}

func TestAlphaGateExternalModuleErgonomics(t *testing.T) {
	ctx := t.Context()

	// 1. database/sql with localclock.New(), partial CompactionConfig override, and *supersql.Error
	sqlConn, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
		Directory:   t.TempDir(),
		Clock:       localclock.New(),
		Compaction:  supersql.CompactionConfig{ResidentBytes: 128 << 20},
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector with partial CompactionConfig: %v", err)
	}
	db := sql.OpenDB(sqlConn)
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE users (id bigint PRIMARY KEY, email text UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users VALUES (1, 'a@example.com')`); err != nil {
		t.Fatal(err)
	}
	_, dupErr := db.ExecContext(ctx, `INSERT INTO users VALUES (2, 'a@example.com')`)
	var superErr *supersql.Error
	if !errors.As(dupErr, &superErr) || superErr.SQLState() != "23505" {
		t.Fatalf("expected *supersql.Error 23505, got %T: %v", dupErr, dupErr)
	}

	// 2. pgx.OpenPool with localclock.New(), partial CompactionConfig override, and *pgx.PgError
	pool, err := pgx.OpenPool(ctx, supersql.OpenOptions{
		Directory:   t.TempDir(),
		Clock:       localclock.New(),
		Compaction:  supersql.CompactionConfig{ResidentBytes: 128 << 20},
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("pgx.OpenPool: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO accounts VALUES ($1, $2)`, int64(1), "Alice"); err != nil {
		t.Fatal(err)
	}
	_, pgDupErr := pool.Exec(ctx, `INSERT INTO accounts VALUES ($1, $2)`, int64(1), "Duplicate")
	var pgErr *pgx.PgError
	if !errors.As(pgDupErr, &pgErr) || pgErr.SQLState() != "23505" {
		t.Fatalf("expected *pgx.PgError 23505, got %T: %v", pgDupErr, pgDupErr)
	}
	if !errors.As(pgDupErr, &superErr) || superErr.SQLState() != "23505" {
		t.Fatalf("expected *pgx.PgError to unwrap to *supersql.Error 23505")
	}
}
