package neosepia

import (
	"context"
	"errors"
	"os"
)

var ErrDatabaseLocked = errors.New("database directory is already open for writing")
var ErrLockUnsupported = errors.New("exclusive database directory locking is unsupported on this platform")

// The lock inode is never unlinked: replacing it could let another opener lock
// a different inode while a previous process still owns the original lock.
type databaseDirectory struct {
	root, commits, runs *os.Root
	lock                *os.File
}

func openDatabaseDirectory(ctx context.Context, path string) (_ *databaseDirectory, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	directory := &databaseDirectory{root: root}
	defer func() {
		if err != nil {
			err = errors.Join(err, directory.Close())
		}
	}()
	directory.lock, err = root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockDatabaseFile(directory.lock); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := root.MkdirAll("commits", 0700); err != nil {
		return nil, err
	}
	if err := root.MkdirAll("runs", 0700); err != nil {
		return nil, err
	}
	directory.commits, err = root.OpenRoot("commits")
	if err != nil {
		return nil, err
	}
	directory.runs, err = root.OpenRoot("runs")
	if err != nil {
		return nil, err
	}
	if err := syncCommitDirectory(root); err != nil {
		return nil, err
	}
	return directory, nil
}

func (d *databaseDirectory) Close() (err error) {
	if d.commits != nil {
		err = errors.Join(err, d.commits.Close())
		d.commits = nil
	}
	if d.runs != nil {
		err = errors.Join(err, d.runs.Close())
		d.runs = nil
	}
	if d.root != nil {
		err = errors.Join(err, d.root.Close())
		d.root = nil
	}
	if d.lock != nil {
		err = errors.Join(err, d.lock.Close())
		d.lock = nil
	}
	return err
}
