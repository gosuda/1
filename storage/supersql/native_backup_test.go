package supersql

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckpointRestore backs up a multi-table database, restores the
// checkpoint into a new directory, and requires the restored database to serve
// exactly the committed snapshot — then to accept new writes under a fresh
// certified fence.
func TestCheckpointRestore(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	for _, statement := range []string{
		`CREATE TABLE accounts (id bigint PRIMARY KEY, balance bigint)`,
		`CREATE TABLE ledger (seq bigint PRIMARY KEY, memo varchar)`,
		`INSERT INTO accounts VALUES (1, 100), (2, 100)`,
		`INSERT INTO ledger VALUES (1, 'seed')`,
	} {
		if _, err := database.Exec(ctx, statement, 0); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := database.CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "backup.manifest")); err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "fence.secret")); err != nil {
		t.Fatalf("fence.secret must travel with the backup: %v", err)
	}

	// More committed work after the backup must not appear in the restore.
	if _, err := database.Exec(ctx, `INSERT INTO accounts VALUES (3, 50)`, 0); err != nil {
		t.Fatalf("post-backup INSERT: %v", err)
	}

	restored, err := RestoreCheckpoint(ctx, backupDir, OpenOptions{Directory: filepath.Join(t.TempDir(), "restored"), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("restored Close: %v", err)
		}
	}()

	outcome, err := restored.Exec(ctx, `SELECT id, balance FROM accounts ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("restored SELECT: %v", err)
	}
	rows := readInt64Rows(t, outcome.Rows)
	want := [][2]int64{{1, 100}, {2, 100}}
	if len(rows) != len(want) {
		t.Fatalf("restored rows = %v, want %v", rows, want)
	}
	for index := range want {
		if rows[index] != want[index] {
			t.Fatalf("restored rows = %v, want %v", rows, want)
		}
	}
	if _, err := restored.Exec(ctx, `INSERT INTO accounts VALUES (4, 75)`, 0); err != nil {
		t.Fatalf("restored INSERT (writable readiness): %v", err)
	}
}

// TestCheckpointRestoreRejectsCorruption proves a tampered file fails
// verification and publishes nothing.
func TestCheckpointRestoreRejectsCorruption(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := database.Exec(ctx, `CREATE TABLE t (n bigint); INSERT INTO t VALUES (1)`, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := database.CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte inside database.state: a manifest-listed file.
	statePath := filepath.Join(backupDir, "database.state")
	data, err := os.ReadFile(statePath)
	if err != nil || len(data) < 8 {
		t.Fatalf("read database.state: %v", err)
	}
	data[4] ^= 0xFF
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("corrupt database.state: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreCheckpoint(ctx, backupDir, OpenOptions{Directory: destination, Clock: newTestClock()}); err == nil {
		t.Fatal("RestoreCheckpoint accepted a corrupted backup")
	} else if !errors.Is(err, ErrCorruptBackup) {
		t.Fatalf("RestoreCheckpoint error = %v, want ErrCorruptBackup", err)
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) != 0 {
		t.Fatalf("corrupted restore published %d entries", len(entries))
	}
}

// TestCheckpointRestoreRejectsOccupiedDestination proves the destination must
// be empty.
func TestCheckpointRestoreRejectsOccupiedDestination(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := database.Exec(ctx, `CREATE TABLE t (n bigint)`, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := database.CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreCheckpoint(ctx, backupDir, OpenOptions{Directory: destination, Clock: newTestClock()}); err == nil {
		t.Fatal("RestoreCheckpoint accepted a nonempty destination")
	}
}
