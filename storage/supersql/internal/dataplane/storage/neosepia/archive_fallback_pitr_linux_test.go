package neosepia_test

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha"
	localclock "github.com/gosuda/supersql-alpha/clock/local"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
)

func TestArchiveFallbackPITR(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		var liveKey, backupKey, archiveKey []byte
		if encrypted {
			name = "encrypted"
			liveKey, backupKey, archiveKey = bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{8}, 32), bytes.Repeat([]byte{9}, 32)
		}
		t.Run(name, func(t *testing.T) {
			calls := neosepia.RejectArchiveHardlinksForTest(t)
			clk := localclock.New()
			archive := filepath.Join(t.TempDir(), "archive")
			connector, err := supersql.OpenConnector(t.Context(), supersql.OpenOptions{
				Directory:     filepath.Join(t.TempDir(), "source"),
				Clock:         clk,
				EncryptionKey: liveKey,
				WALArchiveDir: archive,
				WALArchiveKey: archiveKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			db := sql.OpenDB(connector)
			t.Cleanup(func() {
				if err := errors.Join(db.Close(), connector.Close()); err != nil {
					t.Error(err)
				}
			})
			for _, statement := range []string{
				"CREATE TABLE events (id bigint PRIMARY KEY, val bigint)",
				"INSERT INTO events VALUES (1, 100)",
			} {
				if _, err := db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			backup := filepath.Join(t.TempDir(), "backup")
			if err := connector.Catalog().CreateHotBackupWithOptions(t.Context(), backup, supersql.BackupOptions{EncryptionKey: backupKey}); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			if _, err := db.ExecContext(t.Context(), "INSERT INTO events VALUES (2, 200)"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() < before+2 {
				t.Fatal("the commit did not exercise both WAL and metadata publication fallbacks")
			}
			target, err := clk.Read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO events VALUES (3, 300)"); err != nil {
				t.Fatal(err)
			}
			restored, result, err := supersql.RestorePITR(t.Context(), backup, archive, target.Latest, supersql.PITRRestoreOptions{
				Open: supersql.OpenOptions{
					Directory:     filepath.Join(t.TempDir(), "restored"),
					Clock:         clk,
					EncryptionKey: liveKey,
				},
				EncryptionKey: backupKey,
				WALArchiveKey: archiveKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			restoredConnector := supersql.NewConnectorWithCatalog(restored)
			restoredDB := sql.OpenDB(restoredConnector)
			t.Cleanup(func() {
				if err := errors.Join(restoredDB.Close(), restoredConnector.Close()); err != nil {
					t.Error(err)
				}
			})
			if result.ReplayedSegments == 0 || result.RecoveredCommitTS > target.Latest {
				t.Fatalf("PITR did not replay the fallback archive to the requested boundary: %+v", result)
			}
			rows, err := restoredDB.QueryContext(t.Context(), "SELECT id, val FROM events ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := rows.Close(); err != nil {
					t.Error(err)
				}
			}()
			var got [][2]int64
			for rows.Next() {
				var row [2]int64
				if err := rows.Scan(&row[0], &row[1]); err != nil {
					t.Fatal(err)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, [][2]int64{{1, 100}, {2, 200}}) {
				t.Fatalf("PITR rows = %v, want rows 1 and 2 without the later row 3", got)
			}
			// Orphan repair through the publication fallback: remove one
			// archived segment while its sidecar stays present, then retry.
			// The repaired file must be the original archived bytes.
			entries, err := os.ReadDir(archive)
			if err != nil {
				t.Fatal(err)
			}
			segment := ""
			for _, entry := range entries {
				name := entry.Name()
				if len(name) == len("00000000000000000000.wal") && strings.HasSuffix(name, ".wal") && name > segment {
					segment = name
				}
			}
			if segment == "" {
				t.Fatal("archive holds no WAL segment to remove")
			}
			sidecar := segment[:len(segment)-len(".wal")] + ".meta"
			originalSegment, err := os.ReadFile(filepath.Join(archive, segment))
			if err != nil {
				t.Fatal(err)
			}
			originalMeta, err := os.ReadFile(filepath.Join(archive, sidecar))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(archive, segment)); err != nil {
				t.Fatal(err)
			}
			beforeRetry := calls.Load()
			retryErr := connector.Catalog().EnableWALArchive(t.Context(), archive, archiveKey)
			if encrypted {
				// The archive key differs from the live key, so the original
				// ciphertext cannot be reconstructed: the retry fails closed
				// before publishing anything.
				if !errors.Is(retryErr, storage.ErrCorruptStorage) {
					t.Fatalf("repair with a different archive key = %v, want corrupt storage", retryErr)
				}
				if calls.Load() != beforeRetry {
					t.Fatalf("failed repair published through the fallback: %d link calls, want %d", calls.Load(), beforeRetry)
				}
				if got, err := os.ReadFile(filepath.Join(archive, sidecar)); err != nil || !bytes.Equal(got, originalMeta) {
					t.Fatalf("failed repair changed the original sidecar: %q, %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(archive, segment)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed repair published a replacement WAL: %v", err)
				}
				return
			}
			if retryErr != nil {
				t.Fatalf("retry after segment removal: %v", retryErr)
			}
			if calls.Load() < beforeRetry+1 {
				t.Fatal("the retry did not exercise the WAL publication fallback")
			}
			if got, err := os.ReadFile(filepath.Join(archive, segment)); err != nil || !bytes.Equal(got, originalSegment) {
				t.Fatalf("repaired WAL differs from the original archived bytes")
			}
			if got, err := os.ReadFile(filepath.Join(archive, sidecar)); err != nil || !bytes.Equal(got, originalMeta) {
				t.Fatalf("repair changed the original sidecar: %q, %v", got, err)
			}
			restoredAgain, resultAgain, err := supersql.RestorePITR(t.Context(), backup, archive, target.Latest, supersql.PITRRestoreOptions{
				Open: supersql.OpenOptions{
					Directory:     filepath.Join(t.TempDir(), "restored-again"),
					Clock:         clk,
					EncryptionKey: liveKey,
				},
				EncryptionKey: backupKey,
				WALArchiveKey: archiveKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			restoredAgainConnector := supersql.NewConnectorWithCatalog(restoredAgain)
			restoredAgainDB := sql.OpenDB(restoredAgainConnector)
			t.Cleanup(func() {
				if err := errors.Join(restoredAgainDB.Close(), restoredAgainConnector.Close()); err != nil {
					t.Error(err)
				}
			})
			if resultAgain.ReplayedSegments == 0 || resultAgain.RecoveredCommitTS > target.Latest {
				t.Fatalf("PITR did not replay the repaired archive to the requested boundary: %+v", resultAgain)
			}
			rowsAgain, err := restoredAgainDB.QueryContext(t.Context(), "SELECT id, val FROM events ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := rowsAgain.Close(); err != nil {
					t.Error(err)
				}
			}()
			var gotAgain [][2]int64
			for rowsAgain.Next() {
				var row [2]int64
				if err := rowsAgain.Scan(&row[0], &row[1]); err != nil {
					t.Fatal(err)
				}
				gotAgain = append(gotAgain, row)
			}
			if err := rowsAgain.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(gotAgain, [][2]int64{{1, 100}, {2, 200}}) {
				t.Fatalf("PITR rows through the repaired archive = %v, want rows 1 and 2", gotAgain)
			}
		})
	}
}
