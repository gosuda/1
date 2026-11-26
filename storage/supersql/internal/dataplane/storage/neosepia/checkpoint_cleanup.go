package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
)

func cleanupCheckpointDirectories(ctx context.Context, root *os.Root) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	removed := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(32)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			if !checkpointDirectoryName(entry.Name()) {
				continue
			}
			if !entry.IsDir() {
				return ErrIncompatibleFormat
			}
			collected, err := cleanupCheckpointDirectory(ctx, root, entry.Name())
			if err != nil {
				return err
			}
			removed = removed || collected
		}
		if readErr == io.EOF {
			break
		}
	}
	if removed {
		return syncCommitDirectory(root)
	}
	return nil
}

func checkpointDirectoryName(name string) bool {
	if !strings.HasPrefix(name, checkpointPrefix) || len(name) != len(checkpointPrefix)+32 {
		return false
	}
	for _, value := range name[len(checkpointPrefix):] {
		digit := value >= '0' && value <= '9'
		lowerHex := value >= 'a' && value <= 'f'
		if !digit && !lowerHex {
			return false
		}
	}
	return true
}

func cleanupCheckpointDirectory(ctx context.Context, parent *os.Root, name string) (_ bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	lock, err := root.OpenFile(".lock", os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		empty, err := emptyNativeDirectory(parent, name)
		if err != nil {
			return false, err
		}
		if !empty {
			return false, ErrIncompatibleFormat
		}
		return true, parent.Remove(name)
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := lockDatabaseFile(lock); err != nil {
		if errors.Is(err, ErrDatabaseLocked) {
			return false, nil
		}
		return false, err
	}
	info, err := lock.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(len(checkpointMagic)) {
		return false, ErrIncompatibleFormat
	}
	var marker [len(checkpointMagic)]byte
	count, readErr := io.ReadFull(lock, marker[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return false, readErr
	}
	if !bytes.Equal(marker[:count], []byte(checkpointMagic)[:count]) {
		return false, ErrIncompatibleFormat
	}
	if count != len(marker) {
		directory, err := root.Open(".")
		if err != nil {
			return false, err
		}
		entries, listErr := directory.ReadDir(2)
		if listErr == io.EOF {
			listErr = nil
		}
		if err := errors.Join(listErr, directory.Close()); err != nil {
			return false, err
		}
		if len(entries) != 1 || entries[0].Name() != ".lock" {
			return false, ErrIncompatibleFormat
		}
	}
	return true, removeCheckpointDirectory(parent, root, name)
}

// removeCheckpointDirectory empties a staging directory whose entire contents
// are checkpoint-owned: known subtrees plus any composition-declared root
// extras. Entries are enumerated rather than listed so new checkpoint members
// cannot strand the directory.
func removeCheckpointDirectory(parent, root *os.Root, name string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	names := []string{}
	for {
		entries, readErr := directory.ReadDir(32)
		if readErr != nil && readErr != io.EOF {
			return errors.Join(readErr, directory.Close())
		}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if readErr == io.EOF {
			break
		}
	}
	if err := directory.Close(); err != nil {
		return err
	}
	for _, entry := range names {
		if err := root.RemoveAll(entry); err != nil {
			return err
		}
	}
	return parent.Remove(name)
}
