package supersql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
)

func TestPhase4CatalogFenceSecretEncryption(t *testing.T) {
	ctx := t.Context()
	liveKey := testEncryptionKey(t)
	wrongKey := testEncryptionKey(t)
	backupKey := testEncryptionKey(t)
	dir := filepath.Join(t.TempDir(), "encrypted-db")

	db, err := Open(ctx, OpenOptions{
		Directory:     dir,
		Clock:         newTestClock(),
		EncryptionKey: liveKey,
	})
	if err != nil {
		t.Fatalf("Open encrypted DB: %v", err)
	}
	if _, err := db.exec(ctx, `CREATE TABLE vault (id bigint PRIMARY KEY, amount bigint)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.exec(ctx, `INSERT INTO vault VALUES (1, 100), (2, 250)`, 0); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Verify fence.secret on disk uses the 65-byte "NSFS\x01" AES-256-GCM envelope.
	rawSecret, err := os.ReadFile(filepath.Join(dir, nativeFenceSecretFile))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", nativeFenceSecretFile, err)
	}
	if len(rawSecret) != fenceSecretEnvelopeSize {
		t.Fatalf("fence.secret size = %d, want %d", len(rawSecret), fenceSecretEnvelopeSize)
	}
	if !bytes.Equal(rawSecret[:5], []byte("NSFS\x01")) {
		t.Fatalf("fence.secret header = %q, want %q", rawSecret[:5], "NSFS\x01")
	}

	// Verify decodeFenceSecret unit invariants directly.
	if decoded, corrupt, err := decodeFenceSecret(rawSecret, liveKey); err != nil || corrupt || decoded == ([nativeFenceSecretSize]byte{}) {
		t.Fatalf("decodeFenceSecret(valid): corrupt=%v err=%v", corrupt, err)
	}
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"missing key", nil},
		{"wrong key", wrongKey},
		{"short key", []byte("short-key")},
	} {
		if _, corrupt, err := decodeFenceSecret(rawSecret, tc.key); corrupt || !errors.Is(err, ErrInvalidEncryptionKey) || !errors.Is(err, neosepia.ErrInvalidEncryptionKey) {
			t.Fatalf("decodeFenceSecret(%s): corrupt=%v err=%v, want corrupt=false and ErrInvalidEncryptionKey", tc.name, corrupt, err)
		}
	}
	tampered := bytes.Clone(rawSecret)
	tampered[len(tampered)-1] ^= 0xFF
	if _, corrupt, err := decodeFenceSecret(tampered, liveKey); corrupt || !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("decodeFenceSecret(tampered ciphertext): corrupt=%v err=%v, want corrupt=false and ErrInvalidEncryptionKey", corrupt, err)
	}
	badMagic := bytes.Clone(rawSecret)
	badMagic[0] = 'X'
	if _, corrupt, err := decodeFenceSecret(badMagic, liveKey); !corrupt || err != nil {
		t.Fatalf("decodeFenceSecret(bad header): corrupt=%v err=%v, want corrupt=true err=nil", corrupt, err)
	}

	// Take both an unencrypted hot backup and an encrypted hot backup before closing.
	plainBackupDir := filepath.Join(t.TempDir(), "plain-backup")
	if err := db.CreateHotBackup(ctx, plainBackupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	encBackupDir := filepath.Join(t.TempDir(), "enc-backup")
	if err := db.CreateHotBackupWithOptions(ctx, encBackupDir, BackupOptions{EncryptionKey: backupKey}); err != nil {
		t.Fatalf("CreateHotBackupWithOptions: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening with missing, wrong, or malformed key must fail with ErrInvalidEncryptionKey.
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"missing key", nil},
		{"wrong key", wrongKey},
		{"short key", []byte("too-short")},
	} {
		if _, err := Open(ctx, OpenOptions{
			Directory:     dir,
			Clock:         newTestClock(),
			EncryptionKey: tc.key,
		}); !errors.Is(err, ErrInvalidEncryptionKey) {
			t.Fatalf("reopen(%s) err = %v, want ErrInvalidEncryptionKey", tc.name, err)
		}
	}

	// Reopening with the right key succeeds and preserves committed data.
	reopened, err := Open(ctx, OpenOptions{
		Directory:     dir,
		Clock:         newTestClock(),
		EncryptionKey: liveKey,
	})
	if err != nil {
		t.Fatalf("reopen with liveKey: %v", err)
	}
	outcome, err := reopened.exec(ctx, `SELECT id, amount FROM vault ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT after reopen: %v", err)
	}
	wantRows := [][2]int64{{1, 100}, {2, 250}}
	if got := readInt64Rows(t, outcome.Rows); len(got) != len(wantRows) || got[0] != wantRows[0] || got[1] != wantRows[1] {
		t.Fatalf("rows after reopen = %v, want %v", got, wantRows)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("reopened Close: %v", err)
	}

	// RestoreCheckpoint on unencrypted backup of encrypted DB must return ErrInvalidEncryptionKey
	// (not ErrCorruptBackup) when live key is missing or wrong.
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"missing live key", nil},
		{"wrong live key", wrongKey},
	} {
		_, err := RestoreCheckpoint(ctx, plainBackupDir, OpenOptions{
			Directory:     filepath.Join(t.TempDir(), "plain-fail-"+tc.name),
			Clock:         newTestClock(),
			EncryptionKey: tc.key,
		})
		if !errors.Is(err, ErrInvalidEncryptionKey) {
			t.Fatalf("RestoreCheckpoint(%s) err = %v, want ErrInvalidEncryptionKey", tc.name, err)
		}
		if errors.Is(err, ErrCorruptBackup) {
			t.Fatalf("RestoreCheckpoint(%s) masked key error as ErrCorruptBackup: %v", tc.name, err)
		}
	}

	// RestoreEncryptedCheckpoint rejects wrong/missing live key and succeeds with backupKey + liveKey.
	if _, err := RestoreEncryptedCheckpoint(ctx, encBackupDir, backupKey, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "enc-wrong-live"),
		Clock:         newTestClock(),
		EncryptionKey: wrongKey,
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("RestoreEncryptedCheckpoint(wrong live key) err = %v, want ErrInvalidEncryptionKey", err)
	}
	if _, err := RestoreEncryptedCheckpoint(ctx, encBackupDir, backupKey, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "enc-missing-live"),
		Clock:     newTestClock(),
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("RestoreEncryptedCheckpoint(missing live key) err = %v, want ErrInvalidEncryptionKey", err)
	}

	restored, err := RestoreEncryptedCheckpoint(ctx, encBackupDir, backupKey, OpenOptions{
		Directory:     filepath.Join(t.TempDir(), "enc-restored"),
		Clock:         newTestClock(),
		EncryptionKey: liveKey,
	})
	if err != nil {
		t.Fatalf("RestoreEncryptedCheckpoint: %v", err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Errorf("restored Close: %v", err)
		}
	}()
	outcome, err = restored.exec(ctx, `SELECT id, amount FROM vault ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT restored: %v", err)
	}
	if got := readInt64Rows(t, outcome.Rows); len(got) != len(wantRows) || got[0] != wantRows[0] || got[1] != wantRows[1] {
		t.Fatalf("restored rows = %v, want %v", got, wantRows)
	}
}

func TestPhase4CatalogSubscribeCloseNoLeak(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sub := db.Subscribe("events")
	// Publish >100 unread notifications to completely fill the 64-slot buffer
	// so the delivery goroutine blocks attempting to send notification #65.
	for i := range 120 {
		if err := db.Notify("events", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatalf("Notify(%d): %v", i, err)
		}
	}

	// Wait until the 64-slot buffer is actually full so the delivery goroutine is
	// blocked on `out.ch <- ...`.
	deadline := time.Now().Add(2 * time.Second)
	for len(sub.ch) < cap(sub.ch) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(sub.ch) != cap(sub.ch) {
		t.Fatalf("expected subscriber channel buffer to fill (%d/%d)", len(sub.ch), cap(sub.ch))
	}

	// Close Catalog WITHOUT reading sub.C() or calling sub.Close().
	if err := db.Close(); err != nil {
		t.Fatalf("Catalog.Close: %v", err)
	}

	// Delivery goroutine must exit promptly.
	select {
	case <-sub.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Catalog.Subscribe delivery goroutine leaked after Catalog.Close()")
	}

	// Draining sub.C() must terminate promptly because sub.C() is closed.
	drained := make(chan int, 1)
	go func() {
		count := 0
		for range sub.C() {
			count++
		}
		drained <- count
	}()
	select {
	case count := <-drained:
		if count == 0 {
			t.Fatal("expected buffered notifications before close, got 0")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sub.C() was not closed after Catalog.Close()")
	}

	// Subsequent Wait returns context.Canceled and Close is a safe no-op.
	if _, err := sub.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("sub.Wait after close = %v, want context.Canceled", err)
	}
	sub.Close()

	// Subscribing on an already-closed Catalog returns a pre-closed channel.
	closedSub := db.Subscribe("events")
	select {
	case _, ok := <-closedSub.C():
		if ok {
			t.Fatal("expected closed channel for Subscribe on closed Catalog")
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe on closed Catalog did not return a closed channel")
	}
}

func TestPhase4CatalogCreateUniqueIndexConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	db, _ := openTestDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE conc_items (id bigint PRIMARY KEY, code text NOT NULL, val bigint NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO conc_items SELECT g, 'code-' || g::text, g * 10 FROM generate_series(1, 200) AS g`); err != nil {
		t.Fatalf("INSERT seed rows: %v", err)
	}

	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX CONCURRENTLY idx_conc_items_code ON conc_items (code)`); err != nil {
		t.Fatalf("CREATE UNIQUE INDEX CONCURRENTLY: %v", err)
	}

	// Verify unique constraint is enforced by the newly built concurrent index.
	if _, err := db.ExecContext(ctx, `INSERT INTO conc_items VALUES (999, 'code-42', 420)`); SQLState(err) != "23505" {
		t.Fatalf("duplicate INSERT SQLSTATE = %q (err=%v), want 23505", SQLState(err), err)
	}

	// Verify lookups through the concurrent index return the right row.
	var id, val int64
	if err := db.QueryRowContext(ctx, `SELECT id, val FROM conc_items WHERE code = 'code-137'`).Scan(&id, &val); err != nil {
		t.Fatalf("SELECT by indexed code: %v", err)
	}
	if id != 137 || val != 1370 {
		t.Fatalf("got (id=%d, val=%d), want (137, 1370)", id, val)
	}
}
