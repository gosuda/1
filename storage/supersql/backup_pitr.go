package supersql

import (
	"context"
	"errors"
	"fmt"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ErrInvalidEncryptionKey reports a missing, malformed, or mismatched 32-byte
// AES-256-GCM encryption key during encrypted backup, restore, or WAL archiving.
var ErrInvalidEncryptionKey = errors.New("supersql: invalid or missing encryption key")

// BackupOptions configures full hot checkpoint backups created by
// Catalog.CreateHotBackupWithOptions.
type BackupOptions struct {
	// EncryptionKey optionally enables pure-Go AES-256-GCM at-rest encryption
	// for every file written to the backup directory. When non-empty, it must
	// be exactly 32 bytes.
	EncryptionKey []byte
}

// IncrementalBackupOptions configures an incremental checkpoint backup created
// by Catalog.CreateIncrementalBackup.
type IncrementalBackupOptions struct {
	// Destination is the target directory for the incremental backup artifact.
	// It must be missing or empty with an existing parent directory.
	Destination string
	// EncryptionKey optionally enables pure-Go AES-256-GCM encryption for all
	// changed files copied into Destination. When non-empty, it must be 32 bytes.
	EncryptionKey []byte
}

// IncrementalBackupInfo summarizes one published incremental backup artifact.
type IncrementalBackupInfo struct {
	BackupID           string
	ParentBackupID     string
	BaseSnapshotTS     clock.Instant
	SnapshotTS         clock.Instant
	Encrypted          bool
	KeyFingerprint     string
	ChangedFileCount   int
	InheritedFileCount int
	ChangedFiles       []string
	InheritedFiles     []string
}

// IncrementalRestoreOptions configures RestoreIncremental.
type IncrementalRestoreOptions struct {
	// Open supplies the destination directory, clock, compaction limits, and
	// readiness policy used to open the reconstructed database.
	Open OpenOptions
	// EncryptionKey provides the 32-byte AES-256-GCM key required whenever any
	// hop in the backup chain is encrypted.
	EncryptionKey []byte
}

// PITRRestoreOptions configures Point-in-Time Recovery via RestorePITR and
// RestorePITRFromChain.
type PITRRestoreOptions struct {
	// Open supplies the destination directory, clock, compaction limits, and
	// readiness policy used to open the recovered database.
	Open OpenOptions
	// EncryptionKey provides the 32-byte AES-256-GCM key required when the base
	// backup (or chain) is encrypted.
	EncryptionKey []byte
	// WALArchiveKey optionally specifies a separate 32-byte AES-256-GCM key for
	// encrypted WAL archive segments; when empty, EncryptionKey is used.
	WALArchiveKey []byte
}

// PITRResult summarizes one completed Point-in-Time Recovery restore.
type PITRResult struct {
	BaseLogPosition      uint64
	BaseSnapshotTS       clock.Instant
	TargetTS             clock.Instant
	RecoveredLogPosition uint64
	RecoveredCommitTS    clock.Instant
	ReplayedSegments     uint64
}

// CreateHotBackupWithOptions captures one certified closed snapshot into
// targetDir, optionally encrypting every backed-up file with AES-256-GCM when
// options.EncryptionKey is provided.
func (database *Catalog) CreateHotBackupWithOptions(ctx context.Context, targetDir string, options BackupOptions) error {
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
	_, err = maintenance.CreateBackup(ctx, database.backend, storage.CheckpointRequest{Snapshot: spec}, maintenance.BackupOptions{
		Destination:   targetDir,
		Limits:        backupLimits,
		EncryptionKey: options.EncryptionKey,
	})
	return backupError(err)
}

// CreateIncrementalBackup captures one certified closed snapshot relative to
// baseBackupDir (a full backup or an earlier incremental backup) into
// options.Destination, copying only newly created or modified files and
// recording unchanged files as inherited references.
func (database *Catalog) CreateIncrementalBackup(ctx context.Context, baseBackupDir string, options IncrementalBackupOptions) (IncrementalBackupInfo, error) {
	if database == nil {
		return IncrementalBackupInfo{}, ErrDatabaseClosed
	}
	if err := database.checkOpen(); err != nil {
		return IncrementalBackupInfo{}, err
	}
	if baseBackupDir == "" || options.Destination == "" {
		return IncrementalBackupInfo{}, ErrDirectoryRequired
	}
	spec, err := database.manager.CheckpointSpec(ctx)
	if err != nil {
		return IncrementalBackupInfo{}, err
	}
	inc, err := maintenance.CreateIncrementalBackup(ctx, database.backend, storage.CheckpointRequest{Snapshot: spec}, baseBackupDir, maintenance.BackupOptions{
		Destination:   options.Destination,
		Limits:        backupLimits,
		EncryptionKey: options.EncryptionKey,
	})
	if err != nil {
		return IncrementalBackupInfo{}, backupError(err)
	}
	changedNames := make([]string, len(inc.ChangedFiles))
	for i, f := range inc.ChangedFiles {
		changedNames[i] = f.Name
	}
	inheritedNames := make([]string, len(inc.InheritedFiles))
	for i, f := range inc.InheritedFiles {
		inheritedNames[i] = f.Name
	}
	return IncrementalBackupInfo{
		BackupID:           inc.BackupID,
		ParentBackupID:     inc.ParentBackupID,
		BaseSnapshotTS:     clock.Instant(inc.BaseSnapshotTS),
		SnapshotTS:         clock.Instant(inc.SnapshotTS),
		Encrypted:          inc.Encrypted,
		KeyFingerprint:     inc.KeyFingerprint,
		ChangedFileCount:   len(inc.ChangedFiles),
		InheritedFileCount: len(inc.InheritedFiles),
		ChangedFiles:       changedNames,
		InheritedFiles:     inheritedNames,
	}, nil
}

// EnableWALArchive enables atomic WAL segment archiving on an open database.
// Every existing unretired WAL segment and every subsequent durable commit WAL
// segment is atomically copied to archiveDir (encrypted with AES-256-GCM when
// encryptionKey is non-empty) before WAL retirement can unlink it.
func (database *Catalog) EnableWALArchive(ctx context.Context, archiveDir string, encryptionKey []byte) error {
	if database == nil {
		return ErrDatabaseClosed
	}
	if err := database.checkOpen(); err != nil {
		return err
	}
	if archiveDir == "" {
		return ErrDirectoryRequired
	}
	return backupError(database.backend.EnableWALArchive(ctx, archiveDir, encryptionKey))
}

// RestoreEncryptedCheckpoint restores an encrypted (or unencrypted) full
// checkpoint backup from backupDir into options.Directory, verifies the
// encryption key fingerprint, file checksums, and certified read fence, then
// opens the restored database under Open's readiness rules.
func RestoreEncryptedCheckpoint(ctx context.Context, backupDir string, encryptionKey []byte, options OpenOptions) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Directory == "" || backupDir == "" {
		return nil, ErrDirectoryRequired
	}
	secret, err := readChainFenceSecret(ctx, []string{backupDir}, encryptionKey)
	if err != nil {
		return nil, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, err
	}
	_, err = maintenance.RestoreCheckpoint(ctx, backupDir, maintenance.RestoreOptions{
		Destination:       options.Directory,
		Limits:            backupLimits,
		EncryptionKey:     encryptionKey,
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
	return Open(ctx, options)
}

// RestoreIncremental validates a full-plus-incremental backup chain
// [baseDir, inc1Dir, ...], verifies parent lineage, decryption keys, checksums,
// and certified read fence, reconstructs the database at options.Open.Directory,
// and opens it.
func RestoreIncremental(ctx context.Context, chainDirs []string, options IncrementalRestoreOptions) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Open.Directory == "" || len(chainDirs) == 0 {
		return nil, ErrDirectoryRequired
	}
	secret, err := readChainFenceSecret(ctx, chainDirs, options.EncryptionKey)
	if err != nil {
		return nil, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, err
	}
	_, err = maintenance.RestoreIncremental(ctx, chainDirs, maintenance.RestoreOptions{
		Destination:       options.Open.Directory,
		Limits:            backupLimits,
		EncryptionKey:     options.EncryptionKey,
		LiveEncryptionKey: options.Open.EncryptionKey,
		Verifier: neosepia.CheckpointVerifier{
			FenceVerifier: authority,
			Limits:        mergeCompactionConfig(options.Open.Compaction),
			ExtraFiles:    []string{nativeFenceSecretFile},
			EncryptionKey: options.Open.EncryptionKey,
		},
	})
	if err != nil {
		return nil, backupError(err)
	}
	return Open(ctx, options.Open)
}

// RestorePITR restores baseBackupDir into options.Open.Directory, replays every
// contiguous archived WAL segment from walArchiveDir with commitTS <= targetTS,
// and opens the recovered database strictly at or before targetTS.
func RestorePITR(ctx context.Context, baseBackupDir string, walArchiveDir string, targetTS clock.Instant, options PITRRestoreOptions) (*Catalog, PITRResult, error) {
	if baseBackupDir == "" {
		return nil, PITRResult{}, ErrDirectoryRequired
	}
	return RestorePITRFromChain(ctx, []string{baseBackupDir}, walArchiveDir, targetTS, options)
}

// RestorePITRFromChain restores an incremental backup chain into
// options.Open.Directory, replays contiguous archived WAL segments from
// walArchiveDir with commitTS <= targetTS, and opens the recovered database.
func RestorePITRFromChain(ctx context.Context, chainDirs []string, walArchiveDir string, targetTS clock.Instant, options PITRRestoreOptions) (*Catalog, PITRResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, PITRResult{}, err
	}
	if options.Open.Directory == "" || len(chainDirs) == 0 || walArchiveDir == "" {
		return nil, PITRResult{}, ErrDirectoryRequired
	}
	secret, err := readChainFenceSecret(ctx, chainDirs, options.EncryptionKey)
	if err != nil {
		return nil, PITRResult{}, err
	}
	authority, err := txn.NewFenceAuthority(secret)
	if err != nil {
		return nil, PITRResult{}, err
	}
	archiveKey := options.WALArchiveKey
	if len(archiveKey) == 0 && len(options.EncryptionKey) == 0 {
		archiveKey = options.Open.EncryptionKey
	}
	pitrRes, err := maintenance.RestorePITRFromChain(ctx, chainDirs, walArchiveDir, model.Timestamp(targetTS), maintenance.RestoreOptions{
		Destination:       options.Open.Directory,
		Limits:            backupLimits,
		EncryptionKey:     options.EncryptionKey,
		LiveEncryptionKey: options.Open.EncryptionKey,
		WALArchiveKey:     archiveKey,
		Verifier: neosepia.CheckpointVerifier{
			FenceVerifier: authority,
			Limits:        mergeCompactionConfig(options.Open.Compaction),
			ExtraFiles:    []string{nativeFenceSecretFile},
			EncryptionKey: options.Open.EncryptionKey,
		},
	})
	if err != nil {
		return nil, PITRResult{}, backupError(err)
	}
	db, err := Open(ctx, options.Open)
	if err != nil {
		return nil, PITRResult{}, err
	}
	return db, PITRResult{
		BaseLogPosition:      pitrRes.BaseLogPosition,
		BaseSnapshotTS:       clock.Instant(pitrRes.BaseSnapshotTS),
		TargetTS:             clock.Instant(pitrRes.TargetTS),
		RecoveredLogPosition: pitrRes.RecoveredLogPos,
		RecoveredCommitTS:    clock.Instant(pitrRes.RecoveredCommitTS),
		ReplayedSegments:     pitrRes.ReplayedSegments,
	}, nil
}

func readChainFenceSecret(ctx context.Context, chainDirs []string, encryptionKey []byte) ([nativeFenceSecretSize]byte, error) {
	var secret [nativeFenceSecretSize]byte
	raw, err := maintenance.ReadChainFile(ctx, chainDirs, nativeFenceSecretFile, backupLimits, encryptionKey)
	if err != nil {
		return secret, backupError(err)
	}
	if len(raw) != nativeFenceSecretSize {
		return secret, fmt.Errorf("%w: %s has invalid size %d", ErrCorruptBackup, nativeFenceSecretFile, len(raw))
	}
	copy(secret[:], raw)
	return secret, nil
}

func backupError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, maintenance.ErrInvalidEncryptionKey), errors.Is(err, neosepia.ErrInvalidEncryptionKey):
		return errors.Join(ErrInvalidEncryptionKey, err)
	default:
		return restoreError(err)
	}
}
