package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type recoveryLimits struct {
	Batch    storage.BatchDecodeLimits
	MaxFiles uint64
}

type recoveredPosition struct {
	LogPosition uint64
	CommitTS    model.Timestamp
}

// The caller holds the directory writer lock. A callback borrows its record
// only until return; every sequence cursor must close before the callback ends.
// Pending files are discarded only after every published decision validates.
func recoverCommits(ctx context.Context, root *os.Root, after recoveredPosition, limits recoveryLimits, visit func(*storedCommit) error) (recoveredPosition, error) {
	if err := ctx.Err(); err != nil {
		return after, err
	}
	if visit == nil || limits.MaxFiles == 0 || limits.MaxFiles > uint64(math.MaxInt)/8 {
		return after, storage.ErrDecodeBudget
	}
	positions, pending, err := discoverCommits(ctx, root, after.LogPosition, limits.MaxFiles)
	if err != nil {
		return after, err
	}
	slices.Sort(positions)
	recovered := after
	for _, position := range positions {
		if err := ctx.Err(); err != nil {
			return after, err
		}
		if recovered.LogPosition == math.MaxUint64 || position != recovered.LogPosition+1 {
			return after, storage.ErrCorruptStorage
		}
		record, err := openCommit(ctx, root, position, limits.Batch)
		if err != nil {
			return after, err
		}
		if record.Result.CommitTS <= recovered.CommitTS {
			return after, errors.Join(storage.ErrCorruptStorage, record.Close())
		}
		visitErr := visit(record)
		next := recoveredPosition{LogPosition: position, CommitTS: record.Result.CommitTS}
		if err := errors.Join(visitErr, record.Close()); err != nil {
			return after, err
		}
		recovered = next
	}
	for _, position := range pending {
		if err := ctx.Err(); err != nil {
			return after, err
		}
		if err := root.Remove(pendingCommitName(position)); err != nil {
			return after, err
		}
	}
	if err := syncCommitDirectory(root); err != nil {
		return after, err
	}
	return recovered, nil
}

func discoverCommits(ctx context.Context, root *os.Root, after, maximum uint64) (positions, pending []uint64, err error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	var count uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return nil, nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, nil, storage.ErrCorruptStorage
			}
			position, isPending, err := commitFilePosition(entry.Name())
			if err != nil {
				return nil, nil, err
			}
			if !isPending && position <= after {
				continue
			}
			if count == maximum {
				return nil, nil, storage.ErrDecodeBudget
			}
			count++
			if isPending {
				pending = append(pending, position)
			} else {
				positions = append(positions, position)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return positions, pending, nil
		}
		if readErr != nil {
			return nil, nil, readErr
		}
	}
}

func commitFilePosition(name string) (uint64, bool, error) {
	pending := strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".pending")
	digits := name
	if pending {
		if len(name) != 29 {
			return 0, false, storage.ErrCorruptStorage
		}
		digits = name[1:21]
	} else {
		if len(name) != 24 || !strings.HasSuffix(name, ".wal") {
			return 0, false, storage.ErrCorruptStorage
		}
		digits = name[:20]
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, false, storage.ErrCorruptStorage
		}
	}
	position, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || position == 0 {
		return 0, false, storage.ErrCorruptStorage
	}
	return position, pending, nil
}
