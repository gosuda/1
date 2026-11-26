package supersql

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

// ErrCorruptBackup reports a checkpoint backup whose manifest or file content
// fails verification.
var ErrCorruptBackup = errors.New("corrupted backup manifest or file checksum mismatch")

// backupLimits bounds checkpoint backup and restore copies. Per-file and total
// sizes admit any database the local filesystem can hold; structural metadata
// stays bounded.
var backupLimits = maintenance.Limits{
	MaxManifestBytes: 64 << 20, MaxMetadataBytes: 16 << 20,
	MaxFiles: 65536, MaxNameBytes: 4 << 10, MaxNamesBytes: 16 << 20,
	MaxFileBytes: math.MaxInt64, MaxTotalBytes: math.MaxInt64,
	CopyBufferBytes: 4 << 20,
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
	spec, err := database.manager.CheckpointSpec(ctx)
	if err != nil {
		return err
	}
	_, err = maintenance.CreateBackup(ctx, database.backend, storage.CheckpointRequest{Snapshot: spec}, maintenance.BackupOptions{Destination: targetDir, Limits: backupLimits})
	return err
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
	// The fence secret travels as an authenticated manifest file; reading it
	// before staging lets the verifier authenticate the persisted fence, and a
	// tampered secret fails verification before anything is published.
	secret, err := readBackupFenceSecret(backupDir)
	if err != nil {
		return nil, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, err
	}
	_, err = maintenance.RestoreCheckpoint(ctx, backupDir, maintenance.RestoreOptions{
		Destination: options.Directory,
		Limits:      backupLimits,
		Verifier: neosepia.CheckpointVerifier{
			FenceVerifier: authority,
			Limits:      neosepia.Limits(options.Compaction),
			ExtraFiles:  []string{nativeFenceSecretFile},
		},
	})
	if err != nil {
		return nil, restoreError(err)
	}
	return Open(ctx, options)
}

func readBackupFenceSecret(backupDir string) ([nativeFenceSecretSize]byte, error) {
	secret, corrupt, err := readFenceSecretFile(backupDir)
	switch {
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
