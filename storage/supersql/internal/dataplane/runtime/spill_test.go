package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func testQuery(t *testing.T, fs FileSystem, config QueryConfig) *Query {
	t.Helper()
	pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	memory, err := NewBudget(1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	if config.Directory == "" && config.SpillMode == SpillEnabled {
		config.Directory = t.TempDir()
	}
	query, err := NewQuery(t.Context(), pool, memory, fs, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = query.Close() })
	return query
}

func requireState(t *testing.T, err error, code string) {
	t.Helper()
	var state *model.Error
	if !errors.As(err, &state) || state.Code != code {
		t.Fatalf("error %v, want SQLSTATE %s", err, code)
	}
}

func TestSpillReadersKeepOwnedFileAlive(t *testing.T) {
	dir := t.TempDir()
	query := testQuery(t, OSFileSystem{}, QueryConfig{Directory: dir, SpillBytes: 6, MaxFiles: 1, MaxReaders: 2})
	spill, err := query.NewSpill(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if offset, err := spill.Append(t.Context(), []byte("abc")); err != nil || offset != 0 {
		t.Fatalf("append = %d, %v", offset, err)
	}
	first, err := spill.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := spill.Append(t.Context(), []byte("def")); err != nil {
		t.Fatal(err)
	}
	second, err := spill.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := spill.Reader(t.Context()); !errors.Is(err, ErrBudget) {
		t.Fatalf("reader limit = %v", err)
	}
	if err := spill.Truncate(t.Context(), 1); !errors.Is(err, ErrReadersActive) {
		t.Fatalf("truncate pinned extent = %v", err)
	}
	if err := spill.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Reader(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("new reader after owner close = %v", err)
	}
	if _, err := query.NewSpill(t.Context()); !errors.Is(err, ErrBudget) {
		t.Fatalf("pinned file lost admission charge: %v", err)
	}
	data := make([]byte, 6)
	if n, err := first.ReadAt(t.Context(), data, 0); n != 3 || !errors.Is(err, io.EOF) || string(data[:n]) != "abc" {
		t.Fatalf("prefix reader = %q, %d, %v", data, n, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := second.ReadAt(t.Context(), data, 0); n != 6 || err != nil || string(data) != "abcdef" {
		t.Fatalf("surviving reader = %q, %d, %v", data, n, err)
	}
	var closes sync.WaitGroup
	for range 4 {
		closes.Go(func() { _ = second.Close() })
	}
	closes.Wait()
	if err := spill.WaitClosed(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spill cleanup entries=%v, err=%v", entries, err)
	}
	if got := query.Usage(); got.Spill.Live != 0 || got.Spill.Peak != 6 || got.Files != 0 || got.Readers != 0 || got.PeakReaders != 2 {
		t.Fatalf("spill usage = %+v", got)
	}
}

type faultFS struct {
	OSFileSystem
	file      *faultFile
	removeErr error
}

func (fs *faultFS) CreateTemp(dir, pattern string) (SpillFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	fs.file = &faultFile{File: file}
	return fs.file, nil
}

func (fs *faultFS) Remove(name string) error {
	if fs.removeErr != nil {
		return fs.removeErr
	}
	return os.Remove(name)
}

type faultFile struct {
	*os.File
	writeErr    error
	truncateErr error
	closeErr    error
	afterWrite  func()
}

func (f *faultFile) WriteAt(data []byte, offset int64) (int, error) {
	if f.writeErr != nil {
		n, err := f.File.WriteAt(data[:min(2, len(data))], offset)
		return n, errors.Join(err, f.writeErr)
	}
	n, err := f.File.WriteAt(data, offset)
	if f.afterWrite != nil {
		f.afterWrite()
	}
	return n, err
}

func (f *faultFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

func (f *faultFile) Close() error { return errors.Join(f.File.Close(), f.closeErr) }

func TestPartialAppendRestoresExtentAndBudget(t *testing.T) {
	fs := &faultFS{}
	query := testQuery(t, fs, QueryConfig{SpillBytes: 8})
	spill, err := query.NewSpill(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Append(t.Context(), []byte("base")); err != nil {
		t.Fatal(err)
	}
	fs.file.writeErr = syscall.ENOSPC
	_, err = spill.Append(t.Context(), []byte("more"))
	requireState(t, err, "53100")
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("lost disk cause: %v", err)
	}
	stat, err := fs.file.Stat()
	if err != nil || stat.Size() != 4 || spill.Size() != 4 || query.Usage().Spill.Live != 4 {
		t.Fatalf("rollback stat=%v, err=%v, size=%d, usage=%+v", stat, err, spill.Size(), query.Usage())
	}
	fs.file.writeErr = nil
	if _, err := spill.Append(t.Context(), []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Append(t.Context(), []byte("!")); !errors.Is(err, ErrBudget) {
		t.Fatalf("extent quota = %v", err)
	}
	if err := spill.Truncate(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if query.Usage().Spill.Live != 2 {
		t.Fatal("successful truncate retained old charge")
	}
}

func TestFailedRestorationFencesSpillAndPreservesCauses(t *testing.T) {
	fs := &faultFS{}
	query := testQuery(t, fs, QueryConfig{SpillBytes: 8})
	spill, err := query.NewSpill(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fs.file.writeErr = syscall.ENOSPC
	fs.file.truncateErr = syscall.EIO
	_, err = spill.Append(t.Context(), []byte("data"))
	if !errors.Is(err, ErrFenced) || !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("fence causes = %v", err)
	}
	if spill.Size() != 2 || query.Usage().Spill.Live != 4 {
		t.Fatalf("failed restoration size=%d charge=%d", spill.Size(), query.Usage().Spill.Live)
	}
	if _, err := spill.Append(t.Context(), []byte("x")); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced append = %v", err)
	}
	if _, err := spill.ReadAt(t.Context(), make([]byte, 1), 0); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced read = %v", err)
	}
	if err := query.Close(); !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("query lost spill failure = %v", err)
	}
	if query.Usage().Spill.Live != 0 {
		t.Fatal("successfully removed fenced file retained charge")
	}
}

func TestSpillCleanupFailuresRetainChargeAndCauses(t *testing.T) {
	fs := &faultFS{removeErr: syscall.EACCES}
	query := testQuery(t, fs, QueryConfig{MaxFiles: 1})
	spill, err := query.NewSpill(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Append(t.Context(), []byte("held")); err != nil {
		t.Fatal(err)
	}
	fs.file.closeErr = syscall.EIO
	err = spill.Close()
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("cleanup causes = %v", err)
	}
	if _, err := query.NewSpill(t.Context()); !errors.Is(err, ErrBudget) {
		t.Fatalf("failed cleanup bypassed file admission: %v", err)
	}
	if query.Usage().Spill.Live != 4 {
		t.Fatal("unremoved extent was uncharged")
	}
	if err := query.Close(); !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("query cleanup causes = %v", err)
	}
	if err := os.Remove(fs.file.Name()); err != nil {
		t.Fatal(err)
	}
}

func TestSpillIOCancellationRollsBackBoundedWrite(t *testing.T) {
	fs := &faultFS{}
	query := testQuery(t, fs, QueryConfig{})
	spill, err := query.NewSpill(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fs.file.afterWrite = cancel
	if _, err := spill.Append(ctx, make([]byte, 2*ioChunkBytes)); !errors.Is(err, context.Canceled) {
		t.Fatalf("append cancellation = %v", err)
	}
	stat, err := fs.file.Stat()
	if err != nil || stat.Size() != 0 || query.Usage().Spill.Live != 0 {
		t.Fatalf("canceled write retained extent: %v, %v", stat, err)
	}
	if _, err := spill.ReadAt(ctx, nil, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty canceled read = %v", err)
	}
}

func TestSpillRequiresExplicitDirectoryAndPropagatesCreateFailure(t *testing.T) {
	pool, err := NewPool(PoolConfig{Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	budget, err := NewBudget(1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewQuery(t.Context(), pool, budget, OSFileSystem{}, QueryConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("implicit temp directory = %v", err)
	}
	query := testQuery(t, OSFileSystem{}, QueryConfig{Directory: filepath.Join(t.TempDir(), "absent")})
	if _, err := query.NewSpill(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory cause = %v", err)
	}
	if query.Usage().Spill.Reservations != 0 {
		t.Fatal("failed creation leaked claim")
	}
	disabled := testQuery(t, nil, QueryConfig{SpillMode: SpillDisabled})
	_, err = disabled.NewSpill(t.Context())
	requireState(t, err, "53200")
	if !errors.Is(err, ErrSpillDisabled) {
		t.Fatal(err)
	}
}
