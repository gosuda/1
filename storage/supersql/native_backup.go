package supersql

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ErrCorruptBackup reports a checkpoint backup whose manifest or file content
// fails verification.
var ErrCorruptBackup = errors.New("corrupted backup manifest or file checksum mismatch")

// maxHotBackupAttempts bounds retries when a concurrent memtable flush or
// compaction supersedes the captured checkpoint spec between CheckpointSpec and
// backend.Checkpoint.
const maxHotBackupAttempts = 16

// backupLimits bounds checkpoint backup and restore copies. Per-file and total
// sizes admit any database the local filesystem can hold; structural metadata
// stays bounded.
var backupLimits = maintenance.Limits{
	MaxManifestBytes: 64 << 20, MaxMetadataBytes: 16 << 20,
	MaxFiles: 65536, MaxNameBytes: 4 << 10, MaxNamesBytes: 16 << 20,
	MaxFileBytes: math.MaxInt64, MaxTotalBytes: math.MaxInt64,
	CopyBufferBytes: 4 << 20,
}

func (database *Catalog) hotBackupAttempts() int {
	if database.readOnly {
		return 1
	}
	return maxHotBackupAttempts
}

func isRetryableHotBackupError(err error) bool {
	return errors.Is(err, storage.ErrSnapshotNotClosed) || errors.Is(err, storage.ErrSnapshotTooOld)
}

func validateRestoreOpenOptions(options OpenOptions) error {
	if options.Directory == "" {
		return ErrDirectoryRequired
	}
	if clockUnavailable(options.Clock) {
		return ErrClockRequired
	}
	if options.HistoryRetention < 0 {
		return wrapError(&model.Error{Code: "22023", Message: "invalid history retention duration"})
	}
	if options.SlowQueryThreshold < 0 {
		return wrapError(&model.Error{Code: "22023", Message: "invalid slow query threshold"})
	}
	if _, err := normalizedMaintenance(options.Maintenance); err != nil {
		return wrapError(err)
	}
	if _, err := newTextSearchAnalyzerRegistryFromOptions(options.TextSearchAnalyzers); err != nil {
		return wrapError(err)
	}
	if len(options.EncryptionKey) != 0 && len(options.EncryptionKey) != 32 {
		return errors.Join(ErrInvalidEncryptionKey, neosepia.ErrInvalidEncryptionKey)
	}
	if len(options.WALArchiveKey) != 0 && len(options.WALArchiveKey) != 32 {
		return errors.Join(ErrInvalidEncryptionKey, neosepia.ErrInvalidEncryptionKey)
	}
	return nil
}

func destinationPreExistedDir(dir string) bool {
	info, err := os.Lstat(filepath.Clean(dir))
	return err == nil && info.IsDir()
}

func cleanupRestoredDestination(dir string, preExisted bool) error {
	cleanDir := filepath.Clean(dir)
	if !preExisted {
		return os.RemoveAll(cleanDir)
	}
	entries, err := os.ReadDir(cleanDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(filepath.Join(cleanDir, entry.Name())))
	}
	return cleanupErr
}

func openRestoredDatabase(ctx context.Context, options OpenOptions, dirPreExisted bool) (*Catalog, error) {
	db, err := Open(ctx, options)
	if err != nil {
		if cleanupErr := cleanupRestoredDestination(options.Directory, dirPreExisted); cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, err
	}
	return db, nil
}

// CreateHotBackup captures one certified closed snapshot — the backend file
// set plus the fence secret — into targetDir, which must be missing or empty.
// The backup is immutable and internally checksummed; RestoreCheckpoint opens
// it at a destination. Historical-only databases re-publish their persisted
// closed frontier.
func (database *Catalog) CreateHotBackup(ctx context.Context, targetDir string) error {
	if database == nil {
		return ErrDatabaseClosed
	}
	if err := database.checkOpen(); err != nil {
		return err
	}
	if targetDir == "" {
		return ErrDirectoryRequired
	}
	var lastErr error
	attempts := database.hotBackupAttempts()
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		spec, err := database.manager.CheckpointSpec(ctx)
		if err != nil {
			return err
		}
		_, err = maintenance.CreateBackup(ctx, database.backend, storage.CheckpointRequest{Snapshot: spec}, maintenance.BackupOptions{Destination: targetDir, Limits: backupLimits})
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableHotBackupError(err) {
			return err
		}
	}
	return lastErr
}

// RestoreCheckpoint restores the backup at backupDir into options.Directory —
// which must be missing or empty — verifies every manifest file and the
// persisted certified fence, then opens the restored database under Open's
// readiness rules: writable restoration certifies a fresh fence above the
// restored frontier, while HistoricalReadOnly serves retained snapshots without
// a synchronized clock.
func RestoreCheckpoint(ctx context.Context, backupDir string, options OpenOptions) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Directory == "" {
		return nil, ErrDirectoryRequired
	}
	if backupDir == "" {
		return nil, ErrDirectoryRequired
	}
	if err := validateRestoreOpenOptions(options); err != nil {
		return nil, err
	}
	// The fence secret travels as an authenticated manifest file; reading it
	// before staging lets the verifier authenticate the persisted fence, and a
	// tampered secret fails verification before anything is published.
	secret, err := readBackupFenceSecret(backupDir, options.EncryptionKey)
	if err != nil {
		return nil, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, err
	}
	dirPreExisted := destinationPreExistedDir(options.Directory)
	_, err = maintenance.RestoreCheckpoint(ctx, backupDir, maintenance.RestoreOptions{
		Destination:       options.Directory,
		Limits:            backupLimits,
		LiveEncryptionKey: options.EncryptionKey,
		Verifier: neosepia.CheckpointVerifier{
			FenceVerifier: authority,
			Limits:        mergeCompactionConfig(options.Compaction),
			ExtraFiles:    []string{nativeFenceSecretFile},
			EncryptionKey: options.EncryptionKey,
		},
	})
	if err != nil {
		return nil, backupError(err)
	}
	return openRestoredDatabase(ctx, options, dirPreExisted)
}

func readBackupFenceSecret(backupDir string, encryptionKey []byte) ([nativeFenceSecretSize]byte, error) {
	secret, corrupt, err := readFenceSecretFileWithKey(backupDir, encryptionKey)
	switch {
	case errors.Is(err, ErrInvalidEncryptionKey):
		return secret, err
	case corrupt:
		return secret, fmt.Errorf("%w: %s is corrupt", ErrCorruptBackup, nativeFenceSecretFile)
	case err != nil:
		return secret, fmt.Errorf("%w: %s", ErrCorruptBackup, nativeFenceSecretFile)
	default:
		return secret, nil
	}
}

// restoreError describes restoration as checkpoint restoration. Corruption,
// missing files, and occupied destinations keep their typed causes.
func restoreError(err error) error {
	switch {
	case errors.Is(err, maintenance.ErrCorrupt), errors.Is(err, neosepia.ErrIncompatibleFormat):
		return errors.Join(ErrCorruptBackup, err)
	default:
		return err
	}
}
