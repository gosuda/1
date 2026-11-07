package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"os"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type statePoint uint8

const (
	stateAfterFileSync statePoint = iota + 1
	stateAfterRename
	stateAfterDirectorySync
)

type stateFault func(statePoint) error

func readDatabaseState(ctx context.Context, root *os.Root, maxBytes uint64) (state databaseState, err error) {
	if err := ctx.Err(); err != nil {
		return databaseState{}, err
	}
	file, err := root.Open(databaseStateName)
	if err != nil {
		return databaseState{}, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			state = databaseState{}
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return databaseState{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < databaseStateHeader {
		return databaseState{}, storage.ErrCorruptStorage
	}
	if uint64(info.Size()) > maxBytes || info.Size() > int64(math.MaxInt) {
		return databaseState{}, storage.ErrDecodeBudget
	}
	data := make([]byte, int(info.Size()))
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return databaseState{}, err
		}
		end := offset + min(len(data)-offset, 64<<10)
		n, err := file.ReadAt(data[offset:end], int64(offset))
		if err != nil {
			return databaseState{}, err
		}
		if n != end-offset {
			return databaseState{}, io.ErrUnexpectedEOF
		}
		offset = end
	}
	return decodeDatabaseState(ctx, data, maxBytes)
}

// The caller holds the directory writer lock and has resolved any earlier
// publication attempt. Once rename is attempted, errors require reconciliation.
func writeDatabaseState(ctx context.Context, root *os.Root, state databaseState, maxBytes uint64, fault stateFault) (mayBePublished bool, err error) {
	data, err := encodeDatabaseState(ctx, state, maxBytes)
	if err != nil {
		return false, err
	}
	if err := root.Remove(databaseStatePending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	file, err := root.OpenFile(databaseStatePending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !mayBePublished {
			removeErr := root.Remove(databaseStatePending)
			if !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		end := offset + min(len(data)-offset, 64<<10)
		n, err := file.Write(data[offset:end])
		if err != nil {
			return false, err
		}
		if n != end-offset {
			return false, io.ErrShortWrite
		}
		offset = end
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if fault != nil {
		if err := fault(stateAfterFileSync); err != nil {
			return false, err
		}
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return false, closeErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	mayBePublished = true
	if err := root.Rename(databaseStatePending, databaseStateName); err != nil {
		return true, err
	}
	if fault != nil {
		if err := fault(stateAfterRename); err != nil {
			return true, err
		}
	}
	if err := syncCommitDirectory(root); err != nil {
		return true, err
	}
	if fault != nil {
		if err := fault(stateAfterDirectorySync); err != nil {
			return true, err
		}
	}
	return true, nil
}
