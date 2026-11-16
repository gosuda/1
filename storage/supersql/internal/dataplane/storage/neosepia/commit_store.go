package neosepia

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// A segment appends BEGIN, canonical length-framed mutations, then COMMIT.
// Both fixed frames bind transaction identity, count and body digest; COMMIT
// additionally records the complete body length. Published segments are immutable.
const commitHeaderSize = 128
const commitMagic = "NSTXBEG2"
const commitEndMagic = "NSTXCMT2"

type commitPoint uint8

const (
	commitFileSynced commitPoint = iota + 1
	commitPublished
	commitDirectorySynced
	commitBeginWritten
	commitBodyWrite
	commitBodyWritten
	commitMarkerWritten
	commitBeforeReaderPublication
	commitReaderPublished
)

type commitFault func(commitPoint) error

// A published name is the commit decision. Its inode is complete and synced
// before Link; directory sync is required before acknowledgement. Pending names
// are never replayed. Link, unlike Rename, cannot overwrite another decision.
func writeCommit(ctx context.Context, root *os.Root, position uint64, batch storage.MutationBatch, identity [32]byte, maxBytes uint64, fault commitFault) (result storage.ApplyResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if position == 0 || maxBytes == 0 || maxBytes > math.MaxInt64-2*commitHeaderSize {
		return result, storage.ErrInvalidMutation
	}
	pending := pendingCommitName(position)
	committed := commitName(position)
	file, err := root.OpenFile(pending, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	publicationAttempted := false
	defer func() {
		cleanupErr := file.Close()
		removeErr := root.Remove(pending)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
		if cleanupErr != nil && publicationAttempted {
			cleanupErr = errors.Join(storage.ErrCommitIndeterminate, cleanupErr)
		}
		err = errors.Join(err, cleanupErr)
	}()
	var header [commitHeaderSize]byte
	copy(header[:], commitMagic)
	binary.LittleEndian.PutUint64(header[8:], commitHeaderSize)
	binary.LittleEndian.PutUint64(header[16:], position)
	copy(header[24:], batch.TxID[:])
	binary.LittleEndian.PutUint64(header[40:], uint64(batch.CommitTS))
	binary.LittleEndian.PutUint64(header[48:], batch.Count())
	binary.LittleEndian.PutUint32(header[56:], batch.ChecksumCRC)
	binary.LittleEndian.PutUint32(header[60:], batch.EncodingVersion)
	copy(header[64:], identity[:])
	headerHash := sha256.Sum256(header[:96])
	copy(header[96:], headerHash[:])
	n, writeErr := file.Write(header[:])
	if writeErr != nil {
		return result, writeErr
	}
	if n != len(header) {
		return result, io.ErrShortWrite
	}
	if fault != nil {
		if err := fault(commitBeginWritten); err != nil {
			return result, err
		}
	}
	writer := commitWriter{writer: file, limit: maxBytes, fault: fault}
	hash := sha256.New()
	if err := storage.EncodeMutationBatch(ctx, io.MultiWriter(&writer, hash), batch); err != nil {
		return result, err
	}
	var actual [32]byte
	hash.Sum(actual[:0])
	if actual != identity {
		return result, storage.ErrChecksumMismatch
	}
	if fault != nil {
		if err := fault(commitBodyWritten); err != nil {
			return result, err
		}
	}
	footer := header
	copy(footer[:], commitEndMagic)
	binary.LittleEndian.PutUint64(footer[8:], writer.count)
	footerHash := sha256.Sum256(footer[:96])
	copy(footer[96:], footerHash[:])
	n, writeErr = file.Write(footer[:])
	if writeErr != nil {
		return result, writeErr
	}
	if n != len(footer) {
		return result, io.ErrShortWrite
	}
	if fault != nil {
		if err := fault(commitMarkerWritten); err != nil {
			return result, err
		}
	}
	if err := file.Sync(); err != nil {
		return result, err
	}
	if fault != nil {
		if err := fault(commitFileSynced); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	publicationAttempted = true
	if err := root.Link(pending, committed); err != nil {
		return result, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	if fault != nil {
		if err := fault(commitPublished); err != nil {
			return result, errors.Join(storage.ErrCommitIndeterminate, err)
		}
	}
	if err := syncCommitDirectory(root); err != nil {
		return result, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	if fault != nil {
		if err := fault(commitDirectorySynced); err != nil {
			return result, errors.Join(storage.ErrCommitIndeterminate, err)
		}
	}
	return storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}, nil
}

func syncCommitDirectory(root *os.Root) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	return directory.Sync()
}

func commitName(position uint64) string        { return fmt.Sprintf("%020d.wal", position) }
func pendingCommitName(position uint64) string { return fmt.Sprintf(".%020d.pending", position) }

type commitWriter struct {
	writer io.Writer
	count  uint64
	limit  uint64
	fault  commitFault
}

func (w *commitWriter) Write(data []byte) (int, error) {
	if uint64(len(data)) > w.limit-w.count {
		return 0, columnar.ErrBatchBudget
	}
	n, err := w.writer.Write(data)
	w.count += uint64(n)
	if w.fault != nil && err == nil && n == len(data) {
		err = w.fault(commitBodyWrite)
	}
	return n, err
}

// Batch sequences borrow file; callers close every cursor before this handle.
type storedCommit struct {
	file     *os.File
	Batch    storage.MutationBatch
	Result   storage.ApplyResult
	Identity [32]byte
}

func openCommit(ctx context.Context, root *os.Root, position uint64, limits storage.BatchDecodeLimits) (_ *storedCommit, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := root.Open(commitName(position))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 2*commitHeaderSize {
		return nil, storage.ErrCorruptStorage
	}
	var header [commitHeaderSize]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	headerHash := sha256.Sum256(header[:96])
	if string(header[:8]) != commitMagic || string(header[96:]) != string(headerHash[:]) {
		return nil, storage.ErrCorruptStorage
	}
	if binary.LittleEndian.Uint64(header[8:]) != commitHeaderSize {
		return nil, storage.ErrCorruptStorage
	}
	var footer [commitHeaderSize]byte
	if _, err := file.ReadAt(footer[:], info.Size()-commitHeaderSize); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	footerHash := sha256.Sum256(footer[:96])
	if string(footer[:8]) != commitEndMagic || string(footer[96:]) != string(footerHash[:]) {
		return nil, storage.ErrCorruptStorage
	}
	if string(footer[16:96]) != string(header[16:96]) {
		return nil, storage.ErrCorruptStorage
	}
	length := binary.LittleEndian.Uint64(footer[8:])
	if length != uint64(info.Size()-2*commitHeaderSize) || binary.LittleEndian.Uint64(header[16:]) != position {
		return nil, storage.ErrCorruptStorage
	}
	if length > limits.MaxBytes {
		return nil, storage.ErrDecodeBudget
	}
	identity := [32]byte(header[64:96])
	if err := verifyCommitBody(ctx, file, length, identity); err != nil {
		return nil, err
	}
	source := io.NewSectionReader(file, commitHeaderSize, int64(length))
	batch, err := storage.OpenEncodedMutationBatch(ctx, source, length, limits)
	if err != nil {
		return nil, err
	}
	if string(batch.TxID[:]) != string(header[24:40]) || uint64(batch.CommitTS) != binary.LittleEndian.Uint64(header[40:]) || batch.Count() != binary.LittleEndian.Uint64(header[48:]) || batch.ChecksumCRC != binary.LittleEndian.Uint32(header[56:]) || batch.EncodingVersion != binary.LittleEndian.Uint32(header[60:]) {
		return nil, storage.ErrCorruptStorage
	}
	return &storedCommit{file: file, Batch: batch, Result: storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}, Identity: identity}, nil
}

func verifyCommitBody(ctx context.Context, source io.ReaderAt, length uint64, identity [32]byte) error {
	hash := sha256.New()
	var buffer [4096]byte
	for offset := uint64(0); offset < length; {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := min(uint64(len(buffer)), length-offset)
		n, err := source.ReadAt(buffer[:size], int64(commitHeaderSize+offset))
		if err != nil {
			return errors.Join(storage.ErrCorruptStorage, err)
		}
		if uint64(n) != size {
			return errors.Join(storage.ErrCorruptStorage, io.ErrUnexpectedEOF)
		}
		hash.Write(buffer[:n])
		offset += size
	}
	var actual [32]byte
	hash.Sum(actual[:0])
	if actual != identity {
		return storage.ErrCorruptStorage
	}
	return nil
}

func (c *storedCommit) Close() error {
	if c.file == nil {
		return nil
	}
	file := c.file
	c.file = nil
	c.Batch = storage.MutationBatch{}
	return file.Close()
}
