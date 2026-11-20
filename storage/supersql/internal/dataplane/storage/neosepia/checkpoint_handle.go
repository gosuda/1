package neosepia

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"unsafe"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type checkpointHandle struct {
	mu                  sync.Mutex
	parent, root        *os.Root
	lock                *os.File
	directory           string
	pin                 *runPin
	manifest            storage.CheckpointManifest
	streams, maxStreams uint64
	closed              bool
}

var _ storage.CheckpointHandle = (*checkpointHandle)(nil)

func (handle *checkpointHandle) Manifest(ctx context.Context, limits storage.CheckpointManifestLimits) (storage.CheckpointManifest, error) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return storage.CheckpointManifest{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.CheckpointManifest{}, err
	}
	if limits.MaxBytes == 0 || limits.MaxFiles == 0 || uint64(len(handle.manifest.Files)) > limits.MaxFiles {
		return storage.CheckpointManifest{}, columnar.ErrBatchBudget
	}
	used := uint64(unsafe.Sizeof(storage.CheckpointManifest{}))
	for _, size := range []uint64{uint64(len(handle.manifest.Backend)), uint64(len(handle.manifest.Scale.Name)), uint64(len(handle.manifest.Scale.CanonicalLeapHistory))} {
		if used > limits.MaxBytes || size > limits.MaxBytes-used {
			return storage.CheckpointManifest{}, columnar.ErrBatchBudget
		}
		used += size
	}
	entryBytes := uint64(unsafe.Sizeof(storage.CheckpointFile{}))
	if uint64(len(handle.manifest.Files)) > (limits.MaxBytes-used)/entryBytes {
		return storage.CheckpointManifest{}, columnar.ErrBatchBudget
	}
	used += uint64(len(handle.manifest.Files)) * entryBytes
	for _, file := range handle.manifest.Files {
		if err := ctx.Err(); err != nil {
			return storage.CheckpointManifest{}, err
		}
		if uint64(len(file.Name)) > limits.MaxBytes-used {
			return storage.CheckpointManifest{}, columnar.ErrBatchBudget
		}
		used += uint64(len(file.Name))
	}
	result := handle.manifest
	result.Backend = strings.Clone(result.Backend)
	result.Scale = copyClockScale(result.Scale)
	result.Scale.Name = strings.Clone(result.Scale.Name)
	result.Files = make([]storage.CheckpointFile, len(handle.manifest.Files))
	for i, file := range handle.manifest.Files {
		file.Name = strings.Clone(file.Name)
		result.Files[i] = file
	}
	if err := ctx.Err(); err != nil {
		return storage.CheckpointManifest{}, err
	}
	return result, nil
}

func (handle *checkpointHandle) OpenFile(ctx context.Context, name string) (io.ReadCloser, error) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, found := slices.BinarySearchFunc(handle.manifest.Files, name, func(file storage.CheckpointFile, name string) int { return strings.Compare(file.Name, name) })
	if !found {
		return nil, os.ErrNotExist
	}
	if handle.streams == handle.maxStreams {
		return nil, columnar.ErrBatchBudget
	}
	expected := handle.manifest.Files[index]
	file, err := handle.root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) != expected.Size {
		return nil, errors.Join(storage.ErrCorruptStorage, file.Close())
	}
	handle.streams++
	return &checkpointStream{ctx: ctx, file: file, owner: handle, remaining: expected.Size, expected: expected.SHA256, digest: sha256.New()}, nil
}

func (handle *checkpointHandle) Close() error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return nil
	}
	if handle.streams != 0 {
		return storage.ErrSnapshotPinned
	}
	handle.closed = true
	var err error
	if handle.parent != nil && handle.directory != "" {
		var cleanupErr error
		if handle.root != nil {
			cleanupErr = removeCheckpointDirectory(handle.parent, handle.root, handle.directory)
		} else {
			cleanupErr = handle.parent.Remove(handle.directory)
		}
		err = errors.Join(err, cleanupErr)
		if cleanupErr == nil {
			err = errors.Join(err, syncCommitDirectory(handle.parent))
		}
	}
	if handle.root != nil {
		err = errors.Join(err, handle.root.Close())
	}
	if handle.lock != nil {
		err = errors.Join(err, handle.lock.Close())
	}
	if handle.parent != nil {
		err = errors.Join(err, handle.parent.Close())
	}
	if handle.pin != nil {
		err = errors.Join(err, handle.pin.Close())
	}
	handle.manifest = storage.CheckpointManifest{}
	handle.pin = nil
	handle.root = nil
	handle.parent = nil
	handle.lock = nil
	return err
}

type checkpointStream struct {
	ctx              context.Context
	file             *os.File
	owner            *checkpointHandle
	remaining        uint64
	expected         [32]byte
	digest           hash.Hash
	failure          error
	verified, closed bool
}

func (stream *checkpointStream) Read(data []byte) (int, error) {
	if stream.closed {
		return 0, storage.ErrClosed
	}
	if stream.failure != nil {
		return 0, stream.failure
	}
	if err := stream.ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if stream.remaining == 0 {
		if err := stream.verify(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	count, err := stream.file.Read(data[:min(uint64(len(data)), stream.remaining)])
	if count != 0 {
		stream.digest.Write(data[:count])
		stream.remaining -= uint64(count)
	}
	if err == io.EOF {
		err = errors.Join(storage.ErrCorruptStorage, io.ErrUnexpectedEOF)
	}
	if count == 0 && err == nil {
		err = io.ErrNoProgress
	}
	if err != nil {
		stream.failure = err
	}
	return count, err
}

func (stream *checkpointStream) verify() error {
	if stream.verified {
		return nil
	}
	if stream.failure != nil {
		return stream.failure
	}
	if err := stream.ctx.Err(); err != nil {
		return err
	}
	var extra [1]byte
	count, err := stream.file.Read(extra[:])
	if count != 0 || err != io.EOF {
		stream.failure = errors.Join(storage.ErrCorruptStorage, err)
		return stream.failure
	}
	var actual [32]byte
	stream.digest.Sum(actual[:0])
	if actual != stream.expected {
		stream.failure = storage.ErrCorruptStorage
		return stream.failure
	}
	stream.verified = true
	return nil
}

func (stream *checkpointStream) Close() error {
	if stream.closed {
		return nil
	}
	verifyErr := stream.failure
	if stream.remaining == 0 {
		verifyErr = stream.verify()
	}
	stream.closed = true
	closeErr := stream.file.Close()
	stream.owner.mu.Lock()
	stream.owner.streams--
	stream.owner.mu.Unlock()
	stream.owner = nil
	stream.digest = nil
	stream.ctx = nil
	stream.file = nil
	stream.failure = nil
	return errors.Join(verifyErr, closeErr)
}
