package supersql

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPhase4PerfForUpdateCursorFetchNoDeadlock verifies that FETCH on a
// FOR UPDATE cursor uses the active FETCH statement's transaction token instead
// of the expired DECLARE token (no 2-minute self-deadlock), and that session
// lock_timeout is enforced cleanly during cursor FETCH.
func TestPhase4PerfForUpdateCursorFetchNoDeadlock(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	db.SetMaxOpenConns(4)

	if _, err := db.ExecContext(ctx, `CREATE TABLE p4_cursor (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE p4_cursor: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO p4_cursor VALUES (1, 'a'), (2, 'b'), (3, 'c')`); err != nil {
		t.Fatalf("INSERT p4_cursor: %v", err)
	}

	t.Run("FetchForUpdateCompletesPromptly", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer conn.Close()

		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		start := time.Now()
		if _, err := conn.ExecContext(fetchCtx, `BEGIN`); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := conn.ExecContext(fetchCtx, `DECLARE c1 CURSOR FOR SELECT id, val FROM p4_cursor ORDER BY id FOR UPDATE`); err != nil {
			t.Fatalf("DECLARE c1: %v", err)
		}

		var id int64
		var val string
		if err := conn.QueryRowContext(fetchCtx, `FETCH 1 FROM c1`).Scan(&id, &val); err != nil {
			t.Fatalf("first FETCH 1 FROM c1: %v", err)
		}
		if id != 1 || val != "a" {
			t.Fatalf("first FETCH = (%d, %q), want (1, %q)", id, val, "a")
		}

		if err := conn.QueryRowContext(fetchCtx, `FETCH 1 FROM c1`).Scan(&id, &val); err != nil {
			t.Fatalf("second FETCH 1 FROM c1: %v", err)
		}
		if id != 2 || val != "b" {
			t.Fatalf("second FETCH = (%d, %q), want (2, %q)", id, val, "b")
		}

		if err := conn.QueryRowContext(fetchCtx, `FETCH 1 FROM c1`).Scan(&id, &val); err != nil {
			t.Fatalf("third FETCH 1 FROM c1: %v", err)
		}
		if id != 3 || val != "c" {
			t.Fatalf("third FETCH = (%d, %q), want (3, %q)", id, val, "c")
		}

		if _, err := conn.ExecContext(fetchCtx, `CLOSE c1`); err != nil {
			t.Fatalf("CLOSE c1: %v", err)
		}
		if _, err := conn.ExecContext(fetchCtx, `COMMIT`); err != nil {
			t.Fatalf("COMMIT: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("FOR UPDATE cursor FETCH took %v, want < 2s", elapsed)
		}
	})

	t.Run("FetchRespectsLockTimeout", func(t *testing.T) {
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("holder conn: %v", err)
		}
		defer holder.Close()
		waiter, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("waiter conn: %v", err)
		}
		defer waiter.Close()

		if _, err := holder.ExecContext(ctx, `BEGIN`); err != nil {
			t.Fatalf("holder BEGIN: %v", err)
		}
		defer func() { _, _ = holder.ExecContext(ctx, `ROLLBACK`) }()

		if _, err := waiter.ExecContext(ctx, `SET lock_timeout = '150ms'`); err != nil {
			t.Fatalf("waiter SET lock_timeout: %v", err)
		}
		if _, err := waiter.ExecContext(ctx, `BEGIN`); err != nil {
			t.Fatalf("waiter BEGIN: %v", err)
		}
		defer func() { _, _ = waiter.ExecContext(ctx, `ROLLBACK`) }()
		if _, err := waiter.ExecContext(ctx, `DECLARE c2 CURSOR FOR SELECT id, val FROM p4_cursor WHERE id = 1 FOR UPDATE`); err != nil {
			t.Fatalf("waiter DECLARE c2: %v", err)
		}

		var heldID int64
		if err := holder.QueryRowContext(ctx, `SELECT id FROM p4_cursor WHERE id = 1 FOR SHARE`).Scan(&heldID); err != nil {
			t.Fatalf("holder SELECT FOR SHARE: %v", err)
		}

		start := time.Now()
		var id int64
		var val string
		fetchErr := waiter.QueryRowContext(ctx, `FETCH 1 FROM c2`).Scan(&id, &val)
		elapsed := time.Since(start)
		if fetchErr == nil {
			t.Fatalf("FETCH unexpectedly acquired locked row (%d, %q)", id, val)
		}
		if got := SQLState(fetchErr); got != "55P03" {
			t.Fatalf("FETCH lock timeout SQLSTATE = %q (%v), want 55P03", got, fetchErr)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("FETCH lock_timeout took %v, want prompt timeout", elapsed)
		}
	})
}

// TestPhase4PerfBulkDMLAndAddPrimaryKey verifies that bulk 10k+ row INSERT,
// bulk DELETE, and ALTER TABLE ADD PRIMARY KEY (table rewrite) complete
// promptly without O(N^2) lock scans, unbuffered spill syscalls, or redundant
// per-row snapshot lookups.
func TestPhase4PerfBulkDMLAndAddPrimaryKey(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	const totalRows = 12000
	const deleteRows = 10000

	if _, err := db.ExecContext(ctx, `CREATE TABLE p4_bulk (id bigint NOT NULL, payload text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE p4_bulk: %v", err)
	}

	insertStart := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO p4_bulk SELECT g, 'payload-' || g::text FROM generate_series(1, 12000) AS g`); err != nil {
		t.Fatalf("bulk INSERT %d rows: %v", totalRows, err)
	}
	insertElapsed := time.Since(insertStart)
	t.Logf("bulk INSERT %d rows: %v", totalRows, insertElapsed)
	if insertElapsed > 20*time.Second {
		t.Fatalf("bulk INSERT took %v, expected much faster", insertElapsed)
	}

	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_bulk`).Scan(&count); err != nil {
		t.Fatalf("count after INSERT: %v", err)
	}
	if count != totalRows {
		t.Fatalf("count after INSERT = %d, want %d", count, totalRows)
	}

	deleteStart := time.Now()
	if _, err := db.ExecContext(ctx, `DELETE FROM p4_bulk WHERE id <= 10000`); err != nil {
		t.Fatalf("bulk DELETE %d rows: %v", deleteRows, err)
	}
	deleteElapsed := time.Since(deleteStart)
	t.Logf("bulk DELETE %d rows: %v", deleteRows, deleteElapsed)
	if deleteElapsed > 20*time.Second {
		t.Fatalf("bulk DELETE took %v, expected much faster", deleteElapsed)
	}

	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_bulk`).Scan(&count); err != nil {
		t.Fatalf("count after DELETE: %v", err)
	}
	if count != totalRows-deleteRows {
		t.Fatalf("count after DELETE = %d, want %d", count, totalRows-deleteRows)
	}

	pkStart := time.Now()
	if _, err := db.ExecContext(ctx, `ALTER TABLE p4_bulk ADD PRIMARY KEY (id)`); err != nil {
		t.Fatalf("ALTER TABLE ADD PRIMARY KEY: %v", err)
	}
	pkElapsed := time.Since(pkStart)
	t.Logf("ALTER TABLE ADD PRIMARY KEY (%d rows): %v", totalRows-deleteRows, pkElapsed)
	if pkElapsed > 20*time.Second {
		t.Fatalf("ALTER TABLE ADD PRIMARY KEY took %v, expected much faster", pkElapsed)
	}

	// Verify duplicate detection still returns SQLSTATE 23505 on rewrite.
	if _, err := db.ExecContext(ctx, `CREATE TABLE p4_dup_pk (id bigint NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE p4_dup_pk: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO p4_dup_pk VALUES (1), (2), (1)`); err != nil {
		t.Fatalf("INSERT p4_dup_pk: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE p4_dup_pk ADD PRIMARY KEY (id)`); err == nil {
		t.Fatal("ALTER TABLE ADD PRIMARY KEY on duplicates unexpectedly succeeded")
	} else if got := SQLState(err); got != "23505" {
		t.Fatalf("ADD PRIMARY KEY duplicate SQLSTATE = %q (%v), want 23505", got, err)
	}

	// Verify point lookup via the new primary key index.
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM p4_bulk WHERE id = 11500`).Scan(&payload); err != nil {
		t.Fatalf("SELECT by PK after ADD PRIMARY KEY: %v", err)
	}
	if payload != "payload-11500" {
		t.Fatalf("payload = %q, want %q", payload, "payload-11500")
	}
}

// TestPhase4PerfSpillFileEncryptedAndRotatesIVOnSavepointRollback verifies
// Storage P1 #3c end-to-end: transaction spill files on disk contain no
// plaintext values, and savepoint rollback (Truncate) rotates the AES-256-CTR
// IV and re-encrypts retained prefix bytes while preserving exact data
// integrity across commit.
func TestPhase4PerfSpillFileEncryptedAndRotatesIVOnSavepointRollback(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	connector, err := OpenConnector(ctx, OpenOptions{
		Directory:              dir,
		Clock:                  newTestClock(),
		TransactionMemoryBytes: 2 << 20,
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("db.Close: %v", closeErr)
		}
		if closeErr := connector.Close(); closeErr != nil {
			t.Errorf("connector.Close: %v", closeErr)
		}
	})

	if _, err := db.ExecContext(ctx, `CREATE TABLE p4_spill (id bigint PRIMARY KEY, secret text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE p4_spill: %v", err)
	}

	const marker1 = "SUPERSECRET_SPILL_PLAINTEXT_MARKER_ALPHA"
	const marker2 = "SUPERSECRET_ROLLED_BACK_MARKER_BETA"
	const marker3 = "SUPERSECRET_REPLACEMENT_MARKER_GAMMA"

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SET work_mem = '16MB'`); err != nil {
		t.Fatalf("SET work_mem: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// Insert ~3.2 MiB of rows so the mutation log exceeds its 2 MiB in-memory
	// budget, spills to disk, and flushes multiple 64 KiB write buffers.
	payload1 := strings.Repeat(marker1+"|", 200) // ~8.2 KiB per row
	if _, err := conn.ExecContext(ctx, `INSERT INTO p4_spill SELECT g, $1 FROM generate_series(1, 400) AS g`, payload1); err != nil {
		t.Fatalf("prefix bulk INSERT: %v", err)
	}

	spillDir := filepath.Join(dir, nativeSpoolDirectory)
	findSpillFile := func() (string, []byte) {
		t.Helper()
		matches, globErr := filepath.Glob(filepath.Join(spillDir, "supersql-txn-*"))
		if globErr != nil {
			t.Fatalf("glob spill files: %v", globErr)
		}
		if len(matches) == 0 {
			t.Fatalf("expected spilled supersql-txn-* file in %s, found none", spillDir)
		}
		raw, readErr := os.ReadFile(matches[0])
		if readErr != nil {
			t.Fatalf("read spill file %s: %v", matches[0], readErr)
		}
		return matches[0], raw
	}

	spillPath, beforeRollbackRaw := findSpillFile()
	if len(beforeRollbackRaw) < 64<<10 {
		t.Fatalf("spill file size before savepoint = %d, want >= 64 KiB", len(beforeRollbackRaw))
	}
	if bytes.Contains(beforeRollbackRaw, []byte(marker1)) {
		t.Fatalf("spill file %s contains plaintext marker %q", spillPath, marker1)
	}

	if _, err := conn.ExecContext(ctx, `SAVEPOINT sp1`); err != nil {
		t.Fatalf("SAVEPOINT sp1: %v", err)
	}

	// Write another ~800 KiB so the spill file grows significantly on disk.
	payload2 := strings.Repeat(marker2+"|", 200)
	if _, err := conn.ExecContext(ctx, `INSERT INTO p4_spill SELECT g, $1 FROM generate_series(401, 500) AS g`, payload2); err != nil {
		t.Fatalf("post-savepoint bulk INSERT: %v", err)
	}

	_, grownRaw := findSpillFile()
	if len(grownRaw) <= len(beforeRollbackRaw) {
		t.Fatalf("spill file did not grow after savepoint writes: before=%d after=%d", len(beforeRollbackRaw), len(grownRaw))
	}
	if bytes.Contains(grownRaw, []byte(marker1)) || bytes.Contains(grownRaw, []byte(marker2)) {
		t.Fatalf("grown spill file contains plaintext marker")
	}

	// Snapshot the encrypted prefix bytes before rollback triggers Truncate.
	prefixLen := len(beforeRollbackRaw)
	prefixCipherBefore := bytes.Clone(grownRaw[:prefixLen])

	if _, err := conn.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp1`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT sp1: %v", err)
	}

	afterRollbackRaw, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("read spill file after rollback: %v", err)
	}
	if len(afterRollbackRaw) >= len(grownRaw) {
		t.Fatalf("spill file did not shrink on savepoint rollback: grown=%d after=%d", len(grownRaw), len(afterRollbackRaw))
	}
	if len(afterRollbackRaw) < prefixLen {
		prefixLen = len(afterRollbackRaw)
	}
	if prefixLen == 0 {
		t.Fatal("spill file unexpectedly truncated to 0 bytes")
	}
	if bytes.Contains(afterRollbackRaw, []byte(marker1)) || bytes.Contains(afterRollbackRaw, []byte(marker2)) {
		t.Fatalf("spill file after rollback contains plaintext marker")
	}
	if bytes.Equal(prefixCipherBefore[:prefixLen], afterRollbackRaw[:prefixLen]) {
		t.Fatal("spill file ciphertext prefix was NOT re-encrypted with a fresh IV after savepoint rollback")
	}

	// Write replacement rows after rollback and commit.
	payload3 := strings.Repeat(marker3+"|", 200)
	if _, err := conn.ExecContext(ctx, `INSERT INTO p4_spill SELECT g, $1 FROM generate_series(1001, 1050) AS g`, payload3); err != nil {
		t.Fatalf("replacement bulk INSERT: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}

	// Verify exact committed contents.
	var total, alphaCount, betaCount, gammaCount int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_spill`).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_spill WHERE id <= 400 AND secret = $1`, payload1).Scan(&alphaCount); err != nil {
		t.Fatalf("count alpha: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_spill WHERE id BETWEEN 401 AND 500`).Scan(&betaCount); err != nil {
		t.Fatalf("count beta: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM p4_spill WHERE id >= 1001 AND secret = $1`, payload3).Scan(&gammaCount); err != nil {
		t.Fatalf("count gamma: %v", err)
	}
	if total != 450 || alphaCount != 400 || betaCount != 0 || gammaCount != 50 {
		t.Fatalf("committed rows mismatch: total=%d alpha=%d beta=%d gamma=%d (want 450, 400, 0, 50)", total, alphaCount, betaCount, gammaCount)
	}

	// Spill file must be cleaned up after COMMIT.
	if _, err := os.Stat(spillPath); !os.IsNotExist(err) {
		t.Fatalf("spill file %s still exists after COMMIT: %v", spillPath, err)
	}
}
