package supersql

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
)

func testEncryptionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestF5EncryptedBackupRestoreSQL(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: clk})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	for _, stmt := range []string{
		`CREATE TABLE vault (id bigint PRIMARY KEY, balance bigint)`,
		`INSERT INTO vault VALUES (1, 500), (2, 750)`,
	} {
		if _, err := db.exec(ctx, stmt, 0); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	key := testEncryptionKey(t)
	wrongKey := testEncryptionKey(t)
	backupDir := filepath.Join(t.TempDir(), "encrypted-backup")

	if err := db.CreateHotBackupWithOptions(ctx, backupDir, BackupOptions{EncryptionKey: key}); err != nil {
		t.Fatalf("CreateHotBackupWithOptions: %v", err)
	}

	// Wrong key must fail fast with ErrInvalidEncryptionKey.
	if _, err := RestoreEncryptedCheckpoint(ctx, backupDir, wrongKey, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "wrong"),
		Clock:     newTestClock(),
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("wrong key restore error = %v, want ErrInvalidEncryptionKey", err)
	}

	// Missing key must fail fast with ErrInvalidEncryptionKey.
	if _, err := RestoreEncryptedCheckpoint(ctx, backupDir, nil, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "missing"),
		Clock:     newTestClock(),
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("missing key restore error = %v, want ErrInvalidEncryptionKey", err)
	}

	// Right key restores and serves exact SQL rows.
	restored, err := RestoreEncryptedCheckpoint(ctx, backupDir, key, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "restored"),
		Clock:     newTestClock(),
	})
	if err != nil {
		t.Fatalf("RestoreEncryptedCheckpoint: %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("restored Close: %v", err)
		}
	}()

	outcome, err := restored.exec(ctx, `SELECT id, balance FROM vault ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	rows := readInt64Rows(t, outcome.Rows)
	want := [][2]int64{{1, 500}, {2, 750}}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
}

func TestF5IncrementalBackupTwoHopSQL(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: clk})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	key := testEncryptionKey(t)
	if _, err := db.exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, qty bigint)`, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, `INSERT INTO items VALUES (1, 10)`, 0); err != nil {
		t.Fatal(err)
	}

	baseDir := filepath.Join(t.TempDir(), "base")
	if err := db.CreateHotBackupWithOptions(ctx, baseDir, BackupOptions{EncryptionKey: key}); err != nil {
		t.Fatalf("base backup: %v", err)
	}

	if _, err := db.exec(ctx, `INSERT INTO items VALUES (2, 20)`, 0); err != nil {
		t.Fatal(err)
	}
	inc1Dir := filepath.Join(t.TempDir(), "inc1")
	inc1, err := db.CreateIncrementalBackup(ctx, baseDir, IncrementalBackupOptions{
		Destination:   inc1Dir,
		EncryptionKey: key,
	})
	if err != nil {
		t.Fatalf("inc1 backup: %v", err)
	}
	if inc1.InheritedFileCount == 0 {
		t.Fatalf("expected unchanged files to be inherited in inc1: %+v", inc1)
	}
	for _, inh := range inc1.InheritedFiles {
		if _, err := os.Stat(filepath.Join(inc1Dir, inh)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inherited file %q duplicated in inc1", inh)
		}
	}

	if _, err := db.exec(ctx, `INSERT INTO items VALUES (3, 30)`, 0); err != nil {
		t.Fatal(err)
	}
	inc2Dir := filepath.Join(t.TempDir(), "inc2")
	inc2, err := db.CreateIncrementalBackup(ctx, inc1Dir, IncrementalBackupOptions{
		Destination:   inc2Dir,
		EncryptionKey: key,
	})
	if err != nil {
		t.Fatalf("inc2 backup: %v", err)
	}
	if inc2.ParentBackupID != inc1.BackupID {
		t.Fatalf("inc2 parent = %q, want %q", inc2.ParentBackupID, inc1.BackupID)
	}

	restored, err := RestoreIncremental(ctx, []string{baseDir, inc1Dir, inc2Dir}, IncrementalRestoreOptions{
		Open:          OpenOptions{Directory: filepath.Join(t.TempDir(), "restored"), Clock: newTestClock()},
		EncryptionKey: key,
	})
	if err != nil {
		t.Fatalf("RestoreIncremental: %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("restored Close: %v", err)
		}
	}()

	outcome, err := restored.exec(ctx, `SELECT id, qty FROM items ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	rows := readInt64Rows(t, outcome.Rows)
	want := [][2]int64{{1, 10}, {2, 20}, {3, 30}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestF5PITREndToEndSQL(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	archiveDir := filepath.Join(t.TempDir(), "wal-archive")
	backupKey := testEncryptionKey(t)
	archiveKey := testEncryptionKey(t)

	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: clk})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := db.EnableWALArchive(ctx, archiveDir, archiveKey); err != nil {
		t.Fatalf("EnableWALArchive: %v", err)
	}

	// T1: create table and insert row A (id=1).
	if _, err := db.exec(ctx, `CREATE TABLE events (id bigint PRIMARY KEY, val bigint)`, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, `INSERT INTO events VALUES (1, 100)`, 0); err != nil {
		t.Fatal(err)
	}

	// Base backup after T1.
	baseDir := filepath.Join(t.TempDir(), "base")
	if err := db.CreateHotBackupWithOptions(ctx, baseDir, BackupOptions{EncryptionKey: backupKey}); err != nil {
		t.Fatalf("CreateHotBackupWithOptions: %v", err)
	}

	// T2: insert row B (id=2), then capture target timestamp T2.
	if _, err := db.exec(ctx, `INSERT INTO events VALUES (2, 200)`, 0); err != nil {
		t.Fatal(err)
	}
	stAfterB, err := db.backend.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targetT2 := clock.Instant(stAfterB.CommitTS)

	// T3: insert row C (id=3) — MUST NOT exist after PITR to T2!
	if _, err := db.exec(ctx, `INSERT INTO events VALUES (3, 300)`, 0); err != nil {
		t.Fatal(err)
	}

	// RestorePITR to targetT2 using a clock advanced past T2.
	restoreClock := newTestClock()
	restoreClock.now = targetT2 + 10_000_000

	restored, pitrRes, err := RestorePITR(ctx, baseDir, archiveDir, targetT2, PITRRestoreOptions{
		Open:          OpenOptions{Directory: filepath.Join(t.TempDir(), "pitr-restored"), Clock: restoreClock},
		EncryptionKey: backupKey,
		WALArchiveKey: archiveKey,
	})
	if err != nil {
		t.Fatalf("RestorePITR: %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("restored Close: %v", err)
		}
	}()

	if pitrRes.ReplayedSegments == 0 || pitrRes.RecoveredCommitTS != targetT2 {
		t.Fatalf("unexpected PITRResult: %+v (want RecoveredCommitTS=%d)", pitrRes, targetT2)
	}

	outcome, err := restored.exec(ctx, `SELECT id, val FROM events ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT restored: %v", err)
	}
	rows := readInt64Rows(t, outcome.Rows)
	want := [][2]int64{{1, 100}, {2, 200}}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("PITR restored rows = %v, want %v (row 3 from T3 must be excluded)", rows, want)
	}
}

func TestF5LiveEncryptedDBBackupIncrementalAndPITR(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	liveKey := testEncryptionKey(t)
	backupKey := testEncryptionKey(t)
	wrongKey := testEncryptionKey(t)
	archiveDir := filepath.Join(t.TempDir(), "live-wal-archive")

	db, err := Open(ctx, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "live-encrypted-db"),
		Clock:         clk,
		EncryptionKey: liveKey,
		WALArchiveDir: archiveDir,
	})
	if err != nil {
		t.Fatalf("Open live-encrypted DB: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// T1: seed schema + row 1.
	if _, err := db.exec(ctx, `CREATE TABLE secrets (id bigint PRIMARY KEY, val bigint)`, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, `INSERT INTO secrets VALUES (1, 100)`, 0); err != nil {
		t.Fatal(err)
	}

	// Take BOTH an unencrypted hot backup AND an encrypted hot backup (with a distinct backupKey!).
	plainHotDir := filepath.Join(t.TempDir(), "hot-plain")
	if err := db.CreateHotBackup(ctx, plainHotDir); err != nil {
		t.Fatalf("CreateHotBackup on live-encrypted DB: %v", err)
	}
	encHotDir := filepath.Join(t.TempDir(), "hot-enc")
	if err := db.CreateHotBackupWithOptions(ctx, encHotDir, BackupOptions{EncryptionKey: backupKey}); err != nil {
		t.Fatalf("CreateHotBackupWithOptions on live-encrypted DB: %v", err)
	}

	// T2: insert row 2, take incremental backup relative to encHotDir, record targetT2.
	if _, err := db.exec(ctx, `INSERT INTO secrets VALUES (2, 200)`, 0); err != nil {
		t.Fatal(err)
	}
	stAfter2, err := db.backend.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targetT2 := clock.Instant(stAfter2.CommitTS)

	inc1Dir := filepath.Join(t.TempDir(), "inc1")
	if _, err := db.CreateIncrementalBackup(ctx, encHotDir, IncrementalBackupOptions{
		Destination:   inc1Dir,
		EncryptionKey: backupKey,
	}); err != nil {
		t.Fatalf("CreateIncrementalBackup on live-encrypted DB: %v", err)
	}

	// T3: insert row 3 (must be excluded by PITR to targetT2).
	if _, err := db.exec(ctx, `INSERT INTO secrets VALUES (3, 300)`, 0); err != nil {
		t.Fatal(err)
	}

	assertRows := func(t *testing.T, restored *Catalog, want [][2]int64) {
		t.Helper()
		outcome, err := restored.exec(ctx, `SELECT id, val FROM secrets ORDER BY id`, 0)
		if err != nil {
			t.Fatalf("SELECT restored: %v", err)
		}
		got := readInt64Rows(t, outcome.Rows)
		if len(got) != len(want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d = %v, want %v", i, got[i], want[i])
			}
		}
	}

	// 1. RestoreCheckpoint on unencrypted hot backup of live-encrypted DB:
	//    missing/wrong live key must fail with ErrInvalidEncryptionKey; right liveKey succeeds!
	if _, err := RestoreCheckpoint(ctx, plainHotDir, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "plain-missing-key"),
		Clock:     newTestClock(),
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("RestoreCheckpoint missing live key = %v, want ErrInvalidEncryptionKey", err)
	}
	if _, err := RestoreCheckpoint(ctx, plainHotDir, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "plain-wrong-key"),
		Clock:         newTestClock(),
		EncryptionKey: wrongKey,
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("RestoreCheckpoint wrong live key = %v, want ErrInvalidEncryptionKey", err)
	}
	rHot, err := RestoreCheckpoint(ctx, plainHotDir, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "restored-plain-hot"),
		Clock:         newTestClock(),
		EncryptionKey: liveKey,
	})
	if err != nil {
		t.Fatalf("RestoreCheckpoint live-encrypted DB: %v", err)
	}
	assertRows(t, rHot, [][2]int64{{1, 100}})
	if err := rHot.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. RestoreEncryptedCheckpoint with distinct backupKey + liveKey!
	rEnc, err := RestoreEncryptedCheckpoint(ctx, encHotDir, backupKey, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "restored-enc-hot"),
		Clock:         newTestClock(),
		EncryptionKey: liveKey,
	})
	if err != nil {
		t.Fatalf("RestoreEncryptedCheckpoint live-encrypted DB: %v", err)
	}
	assertRows(t, rEnc, [][2]int64{{1, 100}})
	if err := rEnc.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. RestoreIncremental with distinct backupKey + liveKey!
	rInc, err := RestoreIncremental(ctx, []string{encHotDir, inc1Dir}, IncrementalRestoreOptions{
		Open: OpenOptions{
			Directory:     filepath.Join(t.TempDir(), "restored-inc"),
			Clock:         newTestClock(),
			EncryptionKey: liveKey,
		},
		EncryptionKey: backupKey,
	})
	if err != nil {
		t.Fatalf("RestoreIncremental live-encrypted DB: %v", err)
	}
	assertRows(t, rInc, [][2]int64{{1, 100}, {2, 200}})
	if err := rInc.Close(); err != nil {
		t.Fatal(err)
	}

	// 4a. RestorePITR from unencrypted hot backup of live-encrypted DB (archive encrypted with liveKey by default)!
	pitrClk1 := newTestClock()
	pitrClk1.now = targetT2 + 10_000_000
	rPITRPlain, _, err := RestorePITR(ctx, plainHotDir, archiveDir, targetT2, PITRRestoreOptions{
		Open: OpenOptions{
			Directory:     filepath.Join(t.TempDir(), "restored-pitr-plain"),
			Clock:         pitrClk1,
			EncryptionKey: liveKey,
		},
	})
	if err != nil {
		t.Fatalf("RestorePITR (unencrypted backup of live-encrypted DB): %v", err)
	}
	assertRows(t, rPITRPlain, [][2]int64{{1, 100}, {2, 200}})
	if err := rPITRPlain.Close(); err != nil {
		t.Fatal(err)
	}

	// 4b. RestorePITR from encrypted backup (distinct backupKey) of live-encrypted DB (liveKey)!
	pitrClk2 := newTestClock()
	pitrClk2.now = targetT2 + 10_000_000
	rPITREnc, _, err := RestorePITR(ctx, encHotDir, archiveDir, targetT2, PITRRestoreOptions{
		Open: OpenOptions{
			Directory:     filepath.Join(t.TempDir(), "restored-pitr-enc"),
			Clock:         pitrClk2,
			EncryptionKey: liveKey,
		},
		EncryptionKey: backupKey,
		WALArchiveKey: liveKey,
	})
	if err != nil {
		t.Fatalf("RestorePITR (distinct backupKey + liveKey): %v", err)
	}
	assertRows(t, rPITREnc, [][2]int64{{1, 100}, {2, 200}})
	if err := rPITREnc.Close(); err != nil {
		t.Fatal(err)
	}
}
