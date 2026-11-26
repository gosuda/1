package maintenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func copyFile(ctx context.Context, root *os.Root, expected storage.CheckpointFile, source io.ReadCloser, scratch []byte) (err error) {
	defer func() { err = errors.Join(err, source.Close()) }()
	destination, err := root.OpenFile(expected.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	if err := copyVerified(ctx, destination, source, expected, scratch); err != nil {
		return fmt.Errorf("copy %q: %w", expected.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return destination.Sync()
}

func copyVerified(ctx context.Context, destination io.Writer, source io.Reader, expected storage.CheckpointFile, scratch []byte) error {
	digest := sha256.New()
	remaining := expected.Size
	for remaining != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := scratch[:int(min(remaining, uint64(len(scratch))))]
		n, readErr := source.Read(part)
		if n < 0 || n > len(part) {
			return errors.Join(ErrCorrupt, readErr)
		}
		if n != 0 {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, readErr)
			}
			written, writeErr := destination.Write(part[:n])
			if written != n {
				writeErr = errors.Join(writeErr, io.ErrShortWrite)
			}
			if writeErr != nil {
				return errors.Join(writeErr, readErr)
			}
			_, _ = digest.Write(part[:n])
			remaining -= uint64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if remaining != 0 {
				return errors.Join(ErrCorrupt, io.ErrUnexpectedEOF)
			}
		}
		if n == 0 && readErr == nil {
			return io.ErrNoProgress
		}
	}
	if err := requireEOF(ctx, source); err != nil {
		return err
	}
	var checksum [sha256.Size]byte
	digest.Sum(checksum[:0])
	if checksum != expected.SHA256 {
		return ErrCorrupt
	}
	return ctx.Err()
}

func requireEOF(ctx context.Context, source io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var extra [1]byte
	n, err := source.Read(extra[:])
	if n != 0 {
		return errors.Join(ErrCorrupt, err)
	}
	if err == io.EOF {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return io.ErrNoProgress
}

func openRegular(root *os.Root, name string) (_ *os.File, err error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("nonregular file %q: %w", name, ErrCorrupt)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
		err = ErrCorrupt
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func openBackupRoot(directory string) (_ *os.Root, err error) {
	if directory == "" {
		return nil, ErrInvalid
	}
	directory = filepath.Clean(directory)
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrCorrupt
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = ErrCorrupt
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

func openChildRoot(parent *os.Root, name string) (_ *os.Root, err error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrCorrupt
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = ErrCorrupt
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

// inspectBackup requires the root to hold exactly the envelope, the commits and
// runs directories, and the manifest-listed root files (database.state plus any
// composition-declared extras). Unlisted entries are corruption, never skipped.
func inspectBackup(ctx context.Context, root *os.Root, manifest storage.CheckpointManifest) (err error) {
	var rootFiles int
	for _, file := range manifest.Files {
		if !strings.Contains(file.Name, "/") {
			rootFiles++
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	var files, directories int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			switch {
			case name == BackupManifestName:
				if !entry.Type().IsRegular() {
					return ErrCorrupt
				}
			case entry.IsDir():
				if name != "commits" && name != "runs" {
					return fmt.Errorf("unexpected backup entry %q: %w", name, ErrCorrupt)
				}
				directories++
			case entry.Type().IsRegular():
				if _, found := slices.BinarySearchFunc(manifest.Files, name, func(file storage.CheckpointFile, name string) int {
					return strings.Compare(file.Name, name)
				}); !found || strings.Contains(name, "/") {
					return fmt.Errorf("unlisted backup entry %q: %w", name, ErrCorrupt)
				}
				files++
			default:
				return fmt.Errorf("unexpected backup entry %q: %w", name, ErrCorrupt)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if files != rootFiles || directories != 2 {
		return ErrCorrupt
	}
	for _, name := range [...]string{"commits", "runs"} {
		if err := inspectBackupFiles(ctx, root, name, manifest.Files); err != nil {
			return err
		}
	}
	return nil
}

func inspectBackupFiles(ctx context.Context, root *os.Root, name string, files []storage.CheckpointFile) (err error) {
	child, err := openChildRoot(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, child.Close()) }()
	directory, err := child.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			count++
			if count > len(files) {
				return ErrCorrupt
			}
			if !entry.Type().IsRegular() {
				return ErrCorrupt
			}
			path := name + "/" + entry.Name()
			_, found := slices.BinarySearchFunc(files, path, func(file storage.CheckpointFile, name string) int { return strings.Compare(file.Name, name) })
			if !found {
				return fmt.Errorf("unlisted backup file %q: %w", path, ErrCorrupt)
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func syncDirectory(root *os.Root, name string) (err error) {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	return directory.Sync()
}
