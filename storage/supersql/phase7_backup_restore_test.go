package supersql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// failingOpenClock passes non-nil Clock pre-validation so maintenance.Restore*
// stages, verifies, and publishes files into options.Directory, then causes
// Open() to fail during readiness certification.
type failingOpenClock struct {
	*testClock
}

func (c failingOpenClock) Read(ctx context.Context) (clock.Reading, error) {
	return clock.Reading{}, clock.ErrClockUnavailable
}

func (c failingOpenClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	return clock.ErrClockUnavailable
}

// TestPhase7BackupRestoreHotBackupConcurrentStress verifies P1-S3:
// CreateHotBackup, CreateHotBackupWithOptions, and CreateIncrementalBackup
// survive heavy concurrent writes, coalesced memtable flushes, and background
// compaction without spurious storage.ErrSnapshotNotClosed failures, and every
// captured backup restores and verifies cleanly.
func TestPhase7BackupRestoreHotBackupConcurrentStress(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	db, err := Open(ctx, OpenOptions{
		Directory:        testDirectory(t),
		Clock:            clk,
		HistoryRetention: 0,
		Compaction: CompactionConfig{
			// Small WriteBytes threshold forces frequent coalesced memtable
			// flushes while writers commit concurrently.
			WriteBytes: 32 << 10,
		},
		Maintenance: MaintenanceConfig{
			Interval: MinMaintenanceInterval,
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if _, err := db.exec(ctx, `CREATE TABLE kv (id bigint PRIMARY KEY, payload varchar)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.exec(ctx, `INSERT INTO kv VALUES (0, 'seed')`, 0); err != nil {
		t.Fatalf("seed INSERT: %v", err)
	}
	// Advance HistoryFloor past bootstrap DDL once so repeated Collect calls
	// during the stress loop compact freely while keeping all subsequent
	// concurrent writes verified strictly by CheckpointVerifier.
	stInit, err := db.backend.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if err := db.backend.Collect(ctx, storage.GCRequest{Boundary: stInit.ClosedTS}); err != nil {
		t.Fatalf("initial Collect: %v", err)
	}

	encKey := testEncryptionKey(t)
	baseDir := filepath.Join(t.TempDir(), "inc-base")
	if err := db.CreateHotBackupWithOptions(ctx, baseDir, BackupOptions{EncryptionKey: encKey}); err != nil {
		t.Fatalf("initial base backup: %v", err)
	}

	stopWriters, cancelWriters := context.WithCancel(ctx)
	var writerWG sync.WaitGroup
	var nextID atomic.Int64
	var writeErr atomic.Pointer[error]
	payload := fmt.Sprintf("%0512d", 42)

	// Start concurrent writer goroutines that continuously commit mutations,
	// triggering coalesced memtable flushes.
	const numWriters = 4
	for w := 0; w < numWriters; w++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			for {
				if stopWriters.Err() != nil {
					return
				}
				id := nextID.Add(1)
				stmt := fmt.Sprintf(`INSERT INTO kv VALUES (%d, '%s')`, id, payload)
				if _, err := db.exec(stopWriters, stmt, 0); err != nil {
					if stopWriters.Err() != nil {
						return
					}
					errCopy := err
					writeErr.CompareAndSwap(nil, &errCopy)
					return
				}
			}
		}()
	}

	// Also run an aggressive compaction loop in parallel to advance
	// baseGeneration while hot backups are being taken.
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		ticker := time.NewTicker(3 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWriters.Done():
				return
			case <-ticker.C:
				st, err := db.backend.State(stopWriters)
				if err != nil {
					continue
				}
				_ = db.backend.Collect(stopWriters, storage.GCRequest{Boundary: st.HistoryFloor})
			}
		}
	}()

	// Wait until at least a few concurrent commits have landed.
	deadline := time.Now().Add(5 * time.Second)
	for nextID.Load() < 10 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	var lastPlainDir, lastEncDir, lastIncDir string
	const iterations = 12
	for i := 0; i < iterations; i++ {
		plainDir := filepath.Join(t.TempDir(), fmt.Sprintf("hot-plain-%d", i))
		if err := db.CreateHotBackup(ctx, plainDir); err != nil {
			cancelWriters()
			writerWG.Wait()
			t.Fatalf("CreateHotBackup iteration %d failed: %v", i, err)
		}
		lastPlainDir = plainDir

		encDir := filepath.Join(t.TempDir(), fmt.Sprintf("hot-enc-%d", i))
		if err := db.CreateHotBackupWithOptions(ctx, encDir, BackupOptions{EncryptionKey: encKey}); err != nil {
			cancelWriters()
			writerWG.Wait()
			t.Fatalf("CreateHotBackupWithOptions iteration %d failed: %v", i, err)
		}
		lastEncDir = encDir

		incDir := filepath.Join(t.TempDir(), fmt.Sprintf("hot-inc-%d", i))
		if _, err := db.CreateIncrementalBackup(ctx, baseDir, IncrementalBackupOptions{
			Destination:   incDir,
			EncryptionKey: encKey,
		}); err != nil {
			cancelWriters()
			writerWG.Wait()
			t.Fatalf("CreateIncrementalBackup iteration %d failed: %v", i, err)
		}
		lastIncDir = incDir
	}

	cancelWriters()
	writerWG.Wait()
	if errPtr := writeErr.Load(); errPtr != nil {
		t.Fatalf("background writer failed: %v", *errPtr)
	}

	// Verify restored checkpoints pass full CheckpointVerifier and serve valid rows.
	verifyRestored := func(label string, restored *Catalog) {
		t.Helper()
		defer func() {
			if err := restored.Close(); err != nil {
				t.Errorf("%s Close: %v", label, err)
			}
		}()
		outcome, err := restored.exec(ctx, `SELECT count(*) FROM kv`, 0)
		if err != nil {
			t.Fatalf("%s SELECT count(*): %v", label, err)
		}
		if len(outcome.Rows) != 1 || len(outcome.Rows[0]) != 1 {
			t.Fatalf("%s unexpected count result shape: %v", label, outcome.Rows)
		}
		if outcome.Rows[0][0].IntVal < 1 {
			t.Fatalf("%s count = %d, want >= 1", label, outcome.Rows[0][0].IntVal)
		}
	}

	rPlain, err := RestoreCheckpoint(ctx, lastPlainDir, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "restored-plain"),
		Clock:     newTestClock(),
	})
	if err != nil {
		t.Fatalf("RestoreCheckpoint after concurrent stress: %v", err)
	}
	verifyRestored("plain", rPlain)

	rEnc, err := RestoreEncryptedCheckpoint(ctx, lastEncDir, encKey, OpenOptions{
		Directory: filepath.Join(t.TempDir(), "restored-enc"),
		Clock:     newTestClock(),
	})
	if err != nil {
		t.Fatalf("RestoreEncryptedCheckpoint after concurrent stress: %v", err)
	}
	verifyRestored("encrypted", rEnc)

	rInc, err := RestoreIncremental(ctx, []string{baseDir, lastIncDir}, IncrementalRestoreOptions{
		Open: OpenOptions{
			Directory: filepath.Join(t.TempDir(), "restored-inc"),
			Clock:     newTestClock(),
		},
		EncryptionKey: encKey,
	})
	if err != nil {
		t.Fatalf("RestoreIncremental after concurrent stress: %v", err)
	}
	verifyRestored("incremental", rInc)
}

// TestPhase7BackupRestoreHistoricalReadOnlyNoRetry verifies that HistoricalReadOnly
// mode does not retry hot backups (attempts == 1) and can still re-publish its
// persisted closed frontier.
func TestPhase7BackupRestoreHistoricalReadOnlyNoRetry(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	db, err := Open(ctx, OpenOptions{Directory: dir, Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.exec(ctx, `CREATE TABLE t (id bigint PRIMARY KEY); INSERT INTO t VALUES (1)`, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := Open(ctx, OpenOptions{
		Directory:          dir,
		Clock:              failingOpenClock{testClock: newTestClock()},
		HistoricalReadOnly: true,
	})
	if err != nil {
		t.Fatalf("Open HistoricalReadOnly: %v", err)
	}
	defer func() {
		if err := ro.Close(); err != nil {
			t.Errorf("ro Close: %v", err)
		}
	}()
	if got := ro.hotBackupAttempts(); got != 1 {
		t.Fatalf("HistoricalReadOnly hotBackupAttempts() = %d, want 1", got)
	}
	backupDir := filepath.Join(t.TempDir(), "ro-backup")
	if err := ro.CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup HistoricalReadOnly: %v", err)
	}
}

// TestPhase7BackupRestorePreValidationAndOrphanCleanup verifies P1-S8 across
// RestoreCheckpoint, RestoreEncryptedCheckpoint, RestoreIncremental, and
// RestorePITRFromChain:
//   - invalid OpenOptions fail fast before touching options.Directory,
//   - post-publish Open() failures clean up options.Directory (removing the
//     directory if created by the restore, or emptying it if it pre-existed),
//   - retrying to the EXACT SAME options.Directory with valid OpenOptions
//     succeeds without ErrDestinationOccupied,
//   - genuinely occupied destinations remain rejected without deleting existing files.
func TestPhase7BackupRestorePreValidationAndOrphanCleanup(t *testing.T) {
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
	if _, err := db.exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, val bigint)`, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, `INSERT INTO items VALUES (1, 10)`, 0); err != nil {
		t.Fatal(err)
	}

	plainBackupDir := filepath.Join(t.TempDir(), "plain-backup")
	if err := db.CreateHotBackup(ctx, plainBackupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	encBackupDir := filepath.Join(t.TempDir(), "enc-backup")
	if err := db.CreateHotBackupWithOptions(ctx, encBackupDir, BackupOptions{EncryptionKey: backupKey}); err != nil {
		t.Fatalf("CreateHotBackupWithOptions: %v", err)
	}

	if _, err := db.exec(ctx, `INSERT INTO items VALUES (2, 20)`, 0); err != nil {
		t.Fatal(err)
	}
	stAfter2, err := db.backend.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targetT2 := clock.Instant(stAfter2.CommitTS)

	inc1Dir := filepath.Join(t.TempDir(), "inc1")
	if _, err := db.CreateIncrementalBackup(ctx, encBackupDir, IncrementalBackupOptions{
		Destination:   inc1Dir,
		EncryptionKey: backupKey,
	}); err != nil {
		t.Fatalf("CreateIncrementalBackup: %v", err)
	}

	if _, err := db.exec(ctx, `INSERT INTO items VALUES (3, 30)`, 0); err != nil {
		t.Fatal(err)
	}

	assertRows := func(t *testing.T, restored *Catalog, want [][2]int64) {
		t.Helper()
		outcome, err := restored.exec(ctx, `SELECT id, val FROM items ORDER BY id`, 0)
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

	assertDirClean := func(t *testing.T, dir string, preExisted bool) {
		t.Helper()
		if !preExisted {
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected non-preexisting destination %q to be removed on failure, stat err = %v", dir, err)
			}
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("expected pre-existing empty destination %q to remain readable, err = %v", dir, err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected pre-existing destination %q to be empty after failure, found %d entries", dir, len(entries))
		}
	}

	validClock := func() *testClock {
		c := newTestClock()
		c.now = targetT2 + 10_000_000
		return c
	}

	type restoreAPI struct {
		name    string
		want    [][2]int64
		restore func(opts OpenOptions) (*Catalog, error)
	}

	apis := []restoreAPI{
		{
			name: "RestoreCheckpoint",
			want: [][2]int64{{1, 10}},
			restore: func(opts OpenOptions) (*Catalog, error) {
				return RestoreCheckpoint(ctx, plainBackupDir, opts)
			},
		},
		{
			name: "RestoreEncryptedCheckpoint",
			want: [][2]int64{{1, 10}},
			restore: func(opts OpenOptions) (*Catalog, error) {
				return RestoreEncryptedCheckpoint(ctx, encBackupDir, backupKey, opts)
			},
		},
		{
			name: "RestoreIncremental",
			want: [][2]int64{{1, 10}, {2, 20}},
			restore: func(opts OpenOptions) (*Catalog, error) {
				return RestoreIncremental(ctx, []string{encBackupDir, inc1Dir}, IncrementalRestoreOptions{
					Open:          opts,
					EncryptionKey: backupKey,
				})
			},
		},
		{
			name: "RestorePITRFromChain",
			want: [][2]int64{{1, 10}, {2, 20}},
			restore: func(opts OpenOptions) (*Catalog, error) {
				restored, _, err := RestorePITRFromChain(ctx, []string{encBackupDir}, archiveDir, targetT2, PITRRestoreOptions{
					Open:          opts,
					EncryptionKey: backupKey,
					WALArchiveKey: archiveKey,
				})
				return restored, err
			},
		},
	}

	for _, api := range apis {
		t.Run(api.name, func(t *testing.T) {
			for _, preExisted := range []bool{false, true} {
				modeName := "NewDir"
				if preExisted {
					modeName = "PreExistingEmptyDir"
				}
				t.Run(modeName, func(t *testing.T) {
					var dest string
					if preExisted {
						dest = t.TempDir()
					} else {
						dest = filepath.Join(t.TempDir(), "restored")
					}

					// 1. Pre-validation: missing Clock -> ErrClockRequired, dir stays clean.
					if _, err := api.restore(OpenOptions{Directory: dest}); !errors.Is(err, ErrClockRequired) {
						t.Fatalf("missing clock error = %v, want ErrClockRequired", err)
					}
					assertDirClean(t, dest, preExisted)

					// 2. Pre-validation: negative HistoryRetention -> error, dir stays clean.
					if _, err := api.restore(OpenOptions{
						Directory:        dest,
						Clock:            validClock(),
						HistoryRetention: -time.Second,
					}); err == nil {
						t.Fatal("negative HistoryRetention unexpectedly succeeded")
					}
					assertDirClean(t, dest, preExisted)

					// 3. Pre-validation: invalid live EncryptionKey length -> ErrInvalidEncryptionKey, dir stays clean.
					if _, err := api.restore(OpenOptions{
						Directory:     dest,
						Clock:         validClock(),
						EncryptionKey: []byte("too-short"),
					}); !errors.Is(err, ErrInvalidEncryptionKey) {
						t.Fatalf("short EncryptionKey error = %v, want ErrInvalidEncryptionKey", err)
					}
					assertDirClean(t, dest, preExisted)

					// 4. Post-publish Open() failure: unsynchronized clock causes Open() to fail
					//    AFTER maintenance.Restore* publishes files into dest. Cleanup must remove
					//    the orphan files so retry succeeds without ErrDestinationOccupied!
					if _, err := api.restore(OpenOptions{
						Directory: dest,
						Clock:     failingOpenClock{testClock: validClock()},
					}); err == nil {
						t.Fatal("failingOpenClock unexpectedly succeeded")
					} else if errors.Is(err, maintenance.ErrDestinationOccupied) {
						t.Fatalf("unexpected ErrDestinationOccupied on Open failure: %v", err)
					}
					assertDirClean(t, dest, preExisted)

					// 5. Retry to the EXACT SAME dest with valid options must succeed!
					restored, err := api.restore(OpenOptions{
						Directory: dest,
						Clock:     validClock(),
					})
					if err != nil {
						t.Fatalf("retry restore to same dir %q failed: %v", dest, err)
					}
					defer func() {
						if err := restored.Close(); err != nil {
							t.Errorf("restored Close: %v", err)
						}
					}()
					assertRows(t, restored, api.want)
				})
			}

			// Verify genuinely occupied destinations are rejected and untouched.
			t.Run("OccupiedPreserved", func(t *testing.T) {
				occupied := t.TempDir()
				sentinel := filepath.Join(occupied, "keep-me.txt")
				if err := os.WriteFile(sentinel, []byte("precious"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := api.restore(OpenOptions{
					Directory: occupied,
					Clock:     validClock(),
				}); !errors.Is(err, maintenance.ErrDestinationOccupied) {
					t.Fatalf("occupied destination error = %v, want ErrDestinationOccupied", err)
				}
				data, err := os.ReadFile(sentinel)
				if err != nil || string(data) != "precious" {
					t.Fatalf("occupied destination file altered: data=%q err=%v", string(data), err)
				}
			})
		})
	}
}
