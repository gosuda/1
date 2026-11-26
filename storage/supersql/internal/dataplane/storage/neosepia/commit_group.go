package neosepia

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

const maxGroupCommitBatch = 64

type groupCommitReq struct {
	ctx       context.Context
	batch     storage.MutationBatch
	validated validatedMutationBatch
	done      chan struct{}
	result    storage.ApplyResult
	err       error
}

type stagedGroupCommit struct {
	req          *groupCommitReq
	position     uint64
	decision     runDecision
	result       storage.ApplyResult
	transactions ordered.Tree[recoveryTxnID, transactionRecord]
}

// writeCommitGroup writes and fsyncs one or more WAL segments in commits/ and
// performs a SINGLE syncCommitDirectory for the entire group. Each published
// segment keeps the exact canonical commits/<pos>.wal format.
// Returns the number of segments whose Link succeeded (prefix [0:linked]) plus
// any error. If err != nil and linked > 0, segments [0:linked] may be durable
// on disk (so callers must resolve or treat as indeterminate).
func writeCommitGroup(ctx context.Context, root *os.Root, items []stagedGroupCommit, maxBytes uint64) (int, error) {
	return writeCommitGroupWithCipher(ctx, root, items, maxBytes, nil)
}

func writeCommitGroupWithCipher(ctx context.Context, root *os.Root, items []stagedGroupCommit, maxBytes uint64, c *storageCipher) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	if maxBytes == 0 || maxBytes > math.MaxInt64-2*commitHeaderSize {
		return 0, storage.ErrInvalidMutation
	}
	type pendingSegment struct {
		file      *os.File
		pending   string
		committed string
		item      stagedGroupCommit
	}
	prepared := make([]pendingSegment, 0, len(items))
	cleanupPrepared := func(from int) {
		for i := from; i < len(prepared); i++ {
			_ = prepared[i].file.Close()
			_ = root.Remove(prepared[i].pending)
		}
	}
	// Phase 1: Write and fsync every pending segment before linking any of them.
	// Any failure in Phase 1 leaves zero published WAL files (cleanly abortable!).
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			cleanupPrepared(0)
			return 0, err
		}
		pending := pendingCommitName(item.position)
		committed := commitName(item.position)
		file, err := root.OpenFile(pending, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			cleanupPrepared(0)
			return 0, err
		}
		if err := writePendingCommitBodyWithCipher(ctx, file, item.position, item.req.batch, item.req.validated.identity, maxBytes, c); err != nil {
			_ = file.Close()
			_ = root.Remove(pending)
			cleanupPrepared(0)
			return 0, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = root.Remove(pending)
			cleanupPrepared(0)
			return 0, err
		}
		prepared = append(prepared, pendingSegment{file: file, pending: pending, committed: committed, item: item})
	}
	// Phase 2: Publish each segment via hardlink, then ONE directory sync for the group.
	linked := 0
	for i, seg := range prepared {
		if err := root.Link(seg.pending, seg.committed); err != nil {
			cleanupPrepared(i)
			return linked, errors.Join(storage.ErrCommitIndeterminate, err)
		}
		linked++
		closeErr := seg.file.Close()
		removeErr := root.Remove(seg.pending)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			closeErr = errors.Join(closeErr, removeErr)
		}
		if closeErr != nil {
			cleanupPrepared(i + 1)
			return linked, errors.Join(storage.ErrCommitIndeterminate, closeErr)
		}
	}
	if err := syncCommitDirectory(root); err != nil {
		return linked, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return linked, nil
}

func writePendingCommitBodyWithCipher(ctx context.Context, file *os.File, position uint64, batch storage.MutationBatch, identity [32]byte, maxBytes uint64, c *storageCipher) error {
	if c == nil {
		return writePendingCommitBody(ctx, file, position, batch, identity, maxBytes)
	}
	ew, err := newEncryptStreamWriter(file, c)
	if err != nil {
		return err
	}
	if err := writeCommitFrames(ctx, ew, position, batch, identity, maxBytes, nil); err != nil {
		return err
	}
	return ew.Close()
}

func writePendingCommitBody(ctx context.Context, file *os.File, position uint64, batch storage.MutationBatch, identity [32]byte, maxBytes uint64) error {
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
	n, err := file.Write(header[:])
	if err != nil {
		return err
	}
	if n != len(header) {
		return io.ErrShortWrite
	}
	writer := commitWriter{writer: file, limit: maxBytes}
	hash := sha256.New()
	if err := storage.EncodeMutationBatch(ctx, io.MultiWriter(&writer, hash), batch); err != nil {
		return err
	}
	var actual [32]byte
	hash.Sum(actual[:0])
	if actual != identity {
		return storage.ErrChecksumMismatch
	}
	footer := header
	copy(footer[:], commitEndMagic)
	binary.LittleEndian.PutUint64(footer[8:], writer.count)
	footerHash := sha256.Sum256(footer[:96])
	copy(footer[96:], footerHash[:])
	n, err = file.Write(footer[:])
	if err != nil {
		return err
	}
	if n != len(footer) {
		return io.ErrShortWrite
	}
	return nil
}
