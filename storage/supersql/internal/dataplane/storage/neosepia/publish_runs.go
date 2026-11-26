package neosepia

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Admission checks all files and reserves atomic root installation before WAL
// submission. maxBytes bounds native indexes, pointer capacity and scratch.
func (set *runSet) admitDecision(ctx context.Context, root *os.Root, decision runDecision, files []preparedRun, limits sstLimits, maxBytes uint64) (_ *runAdmission, err error) {
	if err := decision.validate(); err != nil {
		return nil, err
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	set.mu.Lock()
	closed := set.closed
	set.mu.Unlock()
	if closed {
		return nil, storage.ErrClosed
	}
	if root == nil {
		return nil, storage.ErrClosed
	}
	if len(files) == 0 {
		return nil, storage.ErrCorruptStorage
	}
	if uint64(len(files)) > set.limits.MaxFiles {
		return nil, columnar.ErrBatchBudget
	}
	pointerBytes := uint64(unsafe.Sizeof((*sstFile)(nil)))
	if uint64(len(files)) > maxBytes/pointerBytes {
		return nil, columnar.ErrBatchBudget
	}
	used := uint64(len(files)) * pointerBytes
	scratchBytes := min(uint64(64<<10), (maxBytes-used)/2)
	if scratchBytes == 0 || scratchBytes > uint64(math.MaxInt) {
		return nil, columnar.ErrBatchBudget
	}
	used += scratchBytes
	scratch := make([]byte, int(scratchBytes))
	opened := make([]*sstFile, 0, len(files))
	defer func() {
		if opened != nil {
			for _, file := range opened {
				err = errors.Join(err, file.Close())
			}
		}
	}()
	var rows uint64
	for index, descriptor := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if descriptor.Name != preparedRunName(decision.Position, index) || descriptor.Rows == 0 || descriptor.Rows > decision.MutationCount-rows {
			return nil, storage.ErrCorruptStorage
		}
		rows += descriptor.Rows
		run, openErr := openPreparedRun(ctx, root, descriptor, limits, maxBytes-used, scratch)
		if openErr != nil {
			return nil, openErr
		}
		opened = append(opened, run)
		used += uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(run.index))
	}
	if rows != decision.MutationCount {
		return nil, storage.ErrCorruptStorage
	}
	transferred := opened
	opened = nil
	return set.reserve(ctx, decision.Position, decision.CommitTS, transferred)
}

func openPreparedRun(ctx context.Context, root *os.Root, descriptor preparedRun, limits sstLimits, maxIndexBytes uint64, scratch []byte) (_ *sstFile, err error) {
	if descriptor.Bytes == 0 || descriptor.Bytes > limits.MaxBytes || !validFragmentBounds(descriptor, limits.Block.MaxBytes) {
		return nil, storage.ErrCorruptStorage
	}
	file, err := root.Open(descriptor.Name)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			err = errors.Join(err, file.Close())
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || uint64(info.Size()) != descriptor.Bytes {
		return nil, storage.ErrCorruptStorage
	}
	limits.MaxIndexBytes = min(limits.MaxIndexBytes, maxIndexBytes)
	run, err := openSST(ctx, file, limits)
	if err != nil {
		return nil, err
	}
	owned = false
	if run.target != descriptor.Target || run.rows != descriptor.Rows {
		return nil, errors.Join(storage.ErrCorruptStorage, run.Close())
	}
	checksum, err := checksumRunFile(ctx, file, info.Size(), scratch)
	if err != nil {
		return nil, errors.Join(err, run.Close())
	}
	if checksum != descriptor.Checksum {
		return nil, errors.Join(storage.ErrCorruptStorage, run.Close())
	}
	run.descriptor = descriptor
	run.physicalPath, err = filepath.Abs(filepath.Join(root.Name(), descriptor.Name))
	if err != nil {
		return nil, errors.Join(err, run.Close())
	}
	return run, nil
}

func checksumRunFile(ctx context.Context, file *os.File, size int64, scratch []byte) ([32]byte, error) {
	if len(scratch) == 0 {
		return [32]byte{}, columnar.ErrBatchBudget
	}
	digest := sha256.New()
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		chunk := scratch[:min(int64(len(scratch)), size-offset)]
		n, err := file.ReadAt(chunk, offset)
		if n != len(chunk) {
			return [32]byte{}, errors.Join(storage.ErrCorruptStorage, err)
		}
		if err != nil && err != io.EOF {
			return [32]byte{}, err
		}
		if _, err := digest.Write(chunk); err != nil {
			return [32]byte{}, err
		}
		offset += int64(n)
	}
	var checksum [32]byte
	digest.Sum(checksum[:0])
	return checksum, nil
}
