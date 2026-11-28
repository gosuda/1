package neosepia

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplaceFunc performs the atomic no-replace rename. It is a variable
// so tests can simulate mounts that reject the rename flag.
var renameNoReplaceFunc = unix.Renameat2

// renameArchivedFileNoReplace publishes one complete file without replacing a
// competing publisher. Both names are single-component archive filenames made
// by writeArchivedSegment or writeArchivedMeta, relative to the opened root.
//
// A mount that rejects the rename flag itself (ENOSYS on kernels without
// renameat2, EINVAL where the filesystem does not support the flag) reports
// errors.ErrUnsupported so the caller fails the publication closed instead
// of replacing the destination.
func renameArchivedFileNoReplace(root *os.Root, pending, final string) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	fd := int(directory.Fd())
	if err := renameNoReplaceFunc(fd, pending, fd, final, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			return errors.ErrUnsupported
		}
		return err
	}
	return nil
}
