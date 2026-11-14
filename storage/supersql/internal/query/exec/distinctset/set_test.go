package distinctset_test

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
)

var (
	intLayout     = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	floatLayout   = model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}
	numericLayout = model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}
	bytesLayout   = model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}
)

type fixture struct {
	query   *runtime.Query
	dir     string
	fs      *faultFS
	options distinctset.Options
}

func setup(t *testing.T, layouts []model.TypeLayout) fixture {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	budget, err := runtime.NewBudget(32<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	fs := &faultFS{open: make(map[string]*faultFile)}
	dir := t.TempDir()
	q, err := runtime.NewQuery(t.Context(), pool, budget, fs, runtime.QueryConfig{Directory: dir, WorkBytes: 16 << 20, SpillBytes: 16 << 20, MaxFiles: 12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{query: q, dir: dir, fs: fs, options: distinctset.Options{Query: q, Layouts: layouts, Encoder: encoder, MemoryBytes: 128, WorkBytes: 4 << 20, MaxKeyBytes: 256, MaxKeys: 4096, MaxLevels: 10}}
}

func create(t *testing.T, f fixture) *distinctset.Set {
	t.Helper()
	s, err := distinctset.New(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func insert(t *testing.T, s *distinctset.Set, row []model.Value, want bool) {
	t.Helper()
	got, err := s.Insert(t.Context(), row)
	if err != nil || got != want {
		t.Fatalf("Insert(%v) = %v, %v; want %v", row, got, err, want)
	}
}

func released(t *testing.T, f fixture) {
	t.Helper()
	usage := f.query.Usage()
	if usage.Work.Live != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("owned resources remain: %+v", usage)
	}
	files, err := os.ReadDir(f.dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill cleanup: %v, %v", files, err)
	}
}

func TestCanonicalEqualitySurvivesSpillAndMerge(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout, floatLayout, numericLayout})
	s := create(t, f)
	original := []model.Value{model.NullVal(model.TypeInt64), model.Float64Val(math.Float64frombits(0x7ff8000000000001)), {Type: model.TypeNumeric, StrVal: "1.00"}}
	equal := []model.Value{model.NullVal(model.TypeInt64), model.Float64Val(math.Float64frombits(0xfff8000000000020)), {Type: model.TypeNumeric, StrVal: "10e-1"}}
	insert(t, s, original, true)
	insert(t, s, equal, false)
	for i := int64(0); i < 80; i++ {
		insert(t, s, []model.Value{model.Int64Val(i), model.Float64Val(float64(i)), {Type: model.TypeNumeric, StrVal: "1"}}, true)
	}
	if f.query.Usage().Spill.Live == 0 || f.fs.created < 4 {
		t.Fatal("fixture did not force spill and merge")
	}
	insert(t, s, equal, false)
	for i := int64(79); i >= 0; i-- {
		insert(t, s, []model.Value{model.Int64Val(i), model.Float64Val(float64(i)), {Type: model.TypeNumeric, StrVal: "1.0000"}}, false)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, f)
}

func TestBorrowedBytesAreOwnedAfterInsert(t *testing.T) {
	f := setup(t, []model.TypeLayout{bytesLayout})
	s := create(t, f)
	value := []byte("original")
	insert(t, s, []model.Value{model.BytesVal(value)}, true)
	copy(value, "modified")
	insert(t, s, []model.Value{model.BytesVal([]byte("original"))}, false)
	insert(t, s, []model.Value{model.BytesVal(value)}, true)
	for i := byte(0); i < 50; i++ {
		insert(t, s, []model.Value{model.BytesVal([]byte{i})}, true)
	}
	insert(t, s, []model.Value{model.BytesVal([]byte("original"))}, false)
}

func TestDiskLookupUsesBoundedIndexReads(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MemoryBytes = 0
	s := create(t, f)
	for i := int64(0); i < 128; i++ {
		insert(t, s, []model.Value{model.Int64Val(i)}, true)
	}
	before := f.fs.reads
	insert(t, s, []model.Value{model.Int64Val(0)}, false)
	if reads := f.fs.reads - before; reads > 32 {
		t.Fatalf("indexed lookup used %d reads for 128 rows", reads)
	}
	if peak := f.query.Usage().PeakFiles; peak > f.options.MaxLevels+2 {
		t.Fatalf("file bound exceeded: %d", peak)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, f)
}

func TestEmptyTupleDeduplicatesOnDisk(t *testing.T) {
	f := setup(t, nil)
	f.options.MemoryBytes = 0
	s := create(t, f)
	insert(t, s, nil, true)
	insert(t, s, nil, false)
}

func TestLevelExhaustionFencesWithoutAcceptingNewKey(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MemoryBytes = 0
	f.options.MaxLevels = 2
	s := create(t, f)
	for i := int64(0); i < 3; i++ {
		insert(t, s, []model.Value{model.Int64Val(i)}, true)
	}
	got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(3)})
	if got || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("level exhaustion = %v, %v", got, err)
	}
	got, err = s.Insert(t.Context(), []model.Value{model.Int64Val(0)})
	if got || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("fenced retry = %v, %v", got, err)
	}
	released(t, f)
}

func TestKeyLimitStillRecognizesDuplicates(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MaxKeys = 2
	f.options.MemoryBytes = 0
	s := create(t, f)
	insert(t, s, []model.Value{model.Int64Val(1)}, true)
	insert(t, s, []model.Value{model.Int64Val(2)}, true)
	insert(t, s, []model.Value{model.Int64Val(1)}, false)
	if got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(3)}); got || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("key count rejection = %v, %v", got, err)
	}
	released(t, f)
}

func TestOversizedKeyFencesAndReleasesExistingSpills(t *testing.T) {
	f := setup(t, []model.TypeLayout{bytesLayout})
	f.options.MemoryBytes = 0
	f.options.MaxKeyBytes = 16
	s := create(t, f)
	insert(t, s, []model.Value{model.BytesVal([]byte("ok"))}, true)
	if got, err := s.Insert(t.Context(), []model.Value{model.BytesVal([]byte(strings.Repeat("x", 32)))}); got || !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("oversized key = %v, %v", got, err)
	}
	released(t, f)
}

func TestWorkspaceAdmissionPrecedesAllocation(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.WorkBytes = 1
	if s, err := distinctset.New(t.Context(), f.options); s != nil || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("workspace rejection = %v, %v", s, err)
	}
	released(t, f)
	f.options.WorkBytes = 4 << 20
	claim, err := f.query.Allocator(runtime.WorkMemory).Reserve(16 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := distinctset.New(t.Context(), f.options); s != nil || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("shared admission rejection = %v, %v", s, err)
	}
	claim.Release()
	released(t, f)
}

func TestMergeWriteFailureFencesAndCleansEveryRun(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "failed rollback"}[rollbackFails], func(t *testing.T) {
			f := setup(t, []model.TypeLayout{intLayout})
			f.options.MemoryBytes = 0
			s := create(t, f)
			insert(t, s, []model.Value{model.Int64Val(1)}, true)
			// File 2 is the incoming singleton; file 3 is the merge output.
			f.fs.failFile = 3
			f.fs.writeErr = syscall.ENOSPC
			if rollbackFails {
				f.fs.truncateErr = syscall.EIO
			}
			got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(2)})
			if got || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("failed merge = %v, %v", got, err)
			}
			if rollbackFails && !errors.Is(err, runtime.ErrFenced) {
				t.Fatalf("failed rollback did not fence: %v", err)
			}
			f.fs.writeErr, f.fs.truncateErr = nil, nil
			if got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(1)}); got || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("retry after failed merge = %v, %v", got, err)
			}
			released(t, f)
		})
	}
}

func TestCorruptRunNeverAdmitsDuplicate(t *testing.T) {
	for _, region := range []string{"record header", "key", "index", "footer"} {
		t.Run(region, func(t *testing.T) {
			f := setup(t, []model.TypeLayout{intLayout})
			f.options.MemoryBytes = 0
			s := create(t, f)
			row := []model.Value{model.Int64Val(42)}
			insert(t, s, row, true)
			for _, file := range f.fs.files() {
				stat, err := file.Stat()
				if err != nil {
					t.Fatal(err)
				}
				offset := map[string]int64{"record header": 0, "key": 32, "index": stat.Size() - 64, "footer": stat.Size() - 1}[region]
				var data [1]byte
				if _, err := file.File.ReadAt(data[:], offset); err != nil {
					t.Fatal(err)
				}
				data[0] ^= 0x80
				if _, err := file.File.WriteAt(data[:], offset); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.Insert(t.Context(), row)
			var sqlErr *model.Error
			if got || !errors.As(err, &sqlErr) || sqlErr.Code != "XX001" {
				t.Fatalf("corruption = %v, %v", got, err)
			}
			released(t, f)
		})
	}
}

func TestCancellationDuringMergeFencesAndCleansSpills(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MemoryBytes = 0
	s := create(t, f)
	insert(t, s, []model.Value{model.Int64Val(1)}, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.fs.failFile = 3
	f.fs.cancel = cancel
	if got, err := s.Insert(ctx, []model.Value{model.Int64Val(2)}); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled merge = %v, %v", got, err)
	}
	if got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(1)}); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("retry after cancellation = %v, %v", got, err)
	}
	released(t, f)
}

func TestCloseIsIdempotentAndPreventsAdmission(t *testing.T) {
	f := setup(t, []model.TypeLayout{intLayout})
	f.options.MemoryBytes = 0
	s := create(t, f)
	insert(t, s, []model.Value{model.Int64Val(1)}, true)
	for range 2 {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.Insert(t.Context(), []model.Value{model.Int64Val(2)}); got || !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("insert after close = %v, %v", got, err)
	}
	released(t, f)
}

type faultFS struct {
	runtime.OSFileSystem
	openMu      sync.Mutex
	open        map[string]*faultFile
	created     int
	reads       int
	failFile    int
	writeErr    error
	truncateErr error
	cancel      context.CancelFunc
}

func (fs *faultFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	fs.created++
	f := &faultFile{File: file, fs: fs, ordinal: fs.created}
	fs.openMu.Lock()
	fs.open[file.Name()] = f
	fs.openMu.Unlock()
	return f, nil
}

func (fs *faultFS) files() []*faultFile {
	fs.openMu.Lock()
	defer fs.openMu.Unlock()
	files := make([]*faultFile, 0, len(fs.open))
	for _, file := range fs.open {
		files = append(files, file)
	}
	return files
}

type faultFile struct {
	*os.File
	fs      *faultFS
	ordinal int
}

func (f *faultFile) ReadAt(data []byte, offset int64) (int, error) {
	f.fs.reads++
	return f.File.ReadAt(data, offset)
}

func (f *faultFile) WriteAt(data []byte, offset int64) (int, error) {
	if f.ordinal != f.fs.failFile {
		return f.File.WriteAt(data, offset)
	}
	if f.fs.writeErr != nil {
		n, err := f.File.WriteAt(data[:min(3, len(data))], offset)
		return n, errors.Join(err, f.fs.writeErr)
	}
	n, err := f.File.WriteAt(data, offset)
	if f.fs.cancel != nil {
		f.fs.cancel()
	}
	return n, err
}

func (f *faultFile) Truncate(size int64) error {
	if f.ordinal == f.fs.failFile && f.fs.truncateErr != nil {
		return f.fs.truncateErr
	}
	return f.File.Truncate(size)
}

func (f *faultFile) Close() error {
	f.fs.openMu.Lock()
	delete(f.fs.open, f.Name())
	f.fs.openMu.Unlock()
	return f.File.Close()
}
