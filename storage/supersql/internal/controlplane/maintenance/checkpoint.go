package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// CreateBackup captures exactly request.Snapshot.Fence.ClosedTS. Destination must
// be missing or empty, with an existing parent. The returned manifest is owned.
// Cancellation is observed until publication; PublicationError identifies errors
// after rename, when the destination must be inspected rather than retried.
func CreateBackup(ctx context.Context, source Checkpointer, request storage.CheckpointRequest, options BackupOptions) (result storage.CheckpointManifest, err error) {
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if nilDependency(source) {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateSnapshot(request.Snapshot); err != nil {
		return result, err
	}
	stage, err := newStagedTree(ctx, options.Destination)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, stage.close())
		if err != nil {
			result = storage.CheckpointManifest{}
		}
	}()
	handle, err := source.Checkpoint(ctx, request)
	defer func() {
		if !nilDependency(handle) {
			err = errors.Join(err, handle.Close())
		}
	}()
	if err != nil {
		return result, err
	}
	if nilDependency(handle) {
		return result, ErrInvalid
	}
	manifest, err := handle.Manifest(ctx, storage.CheckpointManifestLimits{MaxBytes: options.Limits.MaxMetadataBytes, MaxFiles: options.Limits.MaxFiles})
	if err != nil {
		return result, err
	}
	size, err := validateManifest(ctx, manifest, options.Limits)
	if err != nil {
		return result, err
	}
	if manifest.SnapshotTS != request.Snapshot.Timestamp || manifest.Fence != request.Snapshot.Fence {
		return result, ErrInvalid
	}
	scratch := make([]byte, int(options.Limits.CopyBufferBytes))
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		stream, err := handle.OpenFile(ctx, file.Name)
		if err != nil {
			if !nilDependency(stream) {
				err = errors.Join(err, stream.Close())
			}
			return result, fmt.Errorf("open checkpoint file %q: %w", file.Name, err)
		}
		if nilDependency(stream) {
			return result, ErrInvalid
		}
		if err := copyFile(ctx, stage.root, file, stream, scratch); err != nil {
			return result, err
		}
	}
	closeErr := handle.Close()
	handle = nil
	if closeErr != nil {
		return result, closeErr
	}
	if err := createEnvelope(ctx, stage.root, manifest, size, scratch); err != nil {
		return result, err
	}
	if err := stage.publish(ctx); err != nil {
		return result, err
	}
	return manifest, nil
}

// RestoreCheckpoint borrows backupDir as an immutable artifact for this call.
// Only manifest-listed database files are copied; Verifier must authenticate the
// persisted fence and validate database/WAL/run closure without opening recovery.
// The caller opens the successful tree separately with its Clock/readiness policy.
// Destination and publication-error semantics match CreateBackup.
func RestoreCheckpoint(ctx context.Context, backupDir string, options RestoreOptions) (result storage.CheckpointManifest, err error) {
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if nilDependency(options.Verifier) {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	root, err := openBackupRoot(backupDir)
	if err != nil {
		return result, err
	}
	defer func() {
		if root != nil {
			err = errors.Join(err, root.Close())
		}
		if err != nil {
			result = storage.CheckpointManifest{}
		}
	}()
	scratch := make([]byte, int(options.Limits.CopyBufferBytes))
	manifest, err := loadEnvelope(ctx, root, options.Limits, scratch)
	if err != nil {
		return result, err
	}
	if err := inspectBackup(ctx, root, manifest); err != nil {
		return result, err
	}
	stage, err := newStagedTree(ctx, options.Destination)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.close()) }()
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		stream, err := openBackupFile(root, file)
		if err != nil {
			return result, fmt.Errorf("open backup file %q: %w", file.Name, err)
		}
		if err := copyFile(ctx, stage.root, file, stream, scratch); err != nil {
			return result, err
		}
	}
	closeErr := root.Close()
	root = nil
	if closeErr != nil {
		return result, closeErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := options.Verifier.VerifyCheckpoint(ctx, stage.root, manifest); err != nil {
		return result, fmt.Errorf("verify restored checkpoint: %w", err)
	}
	if err := stage.publish(ctx); err != nil {
		return result, err
	}
	return manifest, nil
}

func createEnvelope(ctx context.Context, root *os.Root, manifest storage.CheckpointManifest, size uint64, scratch []byte) (err error) {
	file, err := root.OpenFile(BackupManifestName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := writeEnvelope(ctx, file, manifest, size, scratch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return file.Sync()
}

func loadEnvelope(ctx context.Context, root *os.Root, limits Limits, scratch []byte) (manifest storage.CheckpointManifest, err error) {
	file, err := openRegular(root, BackupManifestName)
	if err != nil {
		return manifest, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return readEnvelope(ctx, file, limits, scratch)
}

func openBackupFile(root *os.Root, expected storage.CheckpointFile) (file *os.File, err error) {
	var child *os.Root
	defer func() {
		if child != nil {
			err = errors.Join(err, child.Close())
		}
		if err != nil && file != nil {
			err = errors.Join(err, file.Close())
			file = nil
		}
	}()
	directory, name, nested := strings.Cut(expected.Name, "/")
	if nested {
		child, err = openChildRoot(root, directory)
		if err != nil {
			return nil, err
		}
		root = child
	} else {
		name = expected.Name
	}
	file, err = openRegular(root, name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return file, err
	}
	if info.Size() < 0 || uint64(info.Size()) != expected.Size {
		return file, ErrCorrupt
	}
	return file, nil
}
