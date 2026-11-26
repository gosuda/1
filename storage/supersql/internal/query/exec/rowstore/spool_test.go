package rowstore

import (
	"context"
	"errors"
	"io"
	"os"
	goruntime "runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func testLimits() Limits {
	return Limits{MemoryBytes: 4096, MaxBytes: 1 << 20, MaxRows: 128, MaxRecordBytes: 1024, MetadataBytes: 4096, DecodeBytes: 8192, MaxReaders: 2}
}

func testLayout(kind model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: model.TypeID(kind) + 100, Storage: kind}}
}

func testQuery(t *testing.T, fs runtime.FileSystem, config runtime.QueryConfig) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(1<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	if config.Directory == "" && config.SpillMode == runtime.SpillEnabled {
		config.Directory = t.TempDir()
	}
	query, err := runtime.NewQuery(t.Context(), pool, budget, fs, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = query.Close() })
	return query
}

func newSpool(t *testing.T, query *runtime.Query, layouts []model.TypeLayout, limits Limits) *Spool {
	t.Helper()
	s, err := New(t.Context(), query, layouts, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openReader(t *testing.T, s *Spool) *Reader {
	t.Helper()
	r, err := s.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestMemoryAndSpillPreserveRowsAndPositions(t *testing.T) {
	text := testLayout(model.TypeString)
	array := testLayout(model.TypeArray)
	array.Children = []model.TypeLayout{text}
	composite := testLayout(model.TypeComposite)
	composite.Children = []model.TypeLayout{testLayout(model.TypeInt64), array}
	layouts := []model.TypeLayout{testLayout(model.TypeBytes), composite}
	rows := [][]model.Value{
		{model.BytesVal([]byte{0, 255, 1}), {Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(7), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -2}}, ArrayVal: []model.Value{model.StringVal("variable"), model.NullVal(model.TypeString)}}}}},
		{model.NullVal(model.TypeBytes), model.NullVal(model.TypeComposite)},
		{model.BytesVal([]byte("tail")), {Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(-8), {Type: model.TypeArray}}}},
	}
	var memoryPositions []Position
	for _, forceSpill := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "spill"}[forceSpill], func(t *testing.T) {
			dir := t.TempDir()
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{Directory: dir})
			limits := testLimits()
			if forceSpill {
				limits.MemoryBytes = 0
			}
			s := newSpool(t, q, layouts, limits)
			positions := make([]Position, len(rows))
			for i, row := range rows {
				p, err := s.AppendRow(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				positions[i] = p
			}
			if s.Spilled() != forceSpill {
				t.Fatalf("spilled = %v", s.Spilled())
			}
			if forceSpill && q.Usage().Spill.Live != int64(s.Bytes()) {
				t.Fatalf("spill charge = %+v", q.Usage())
			}
			if forceSpill {
				if !slices.Equal(positions, memoryPositions) {
					t.Fatalf("positions = %v, memory = %v", positions, memoryPositions)
				}
			} else {
				memoryPositions = positions
			}
			if _, err := s.Reader(t.Context()); !errors.Is(err, ErrNotSealed) {
				t.Fatalf("unsealed reader = %v", err)
			}
			if err := s.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendRow(t.Context(), rows[0]); !errors.Is(err, ErrSealed) {
				t.Fatalf("sealed append = %v", err)
			}
			first, second := openReader(t, s), openReader(t, s)
			for i, want := range rows {
				position, got, err := first.NextRow(t.Context())
				if err != nil || position != positions[i] || !cmp.Equal(got, want, cmpopts.EquateEmpty()) {
					t.Fatalf("row %d = %v at %d, %v; want %v", i, got, position, err, want)
				}
			}
			if _, _, err := first.NextRow(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("end = %v", err)
			}
			got, err := second.Read(t.Context(), positions[2])
			if err != nil || !cmp.Equal(got, rows[2], cmpopts.EquateEmpty()) {
				t.Fatalf("random row = %v, %v", got, err)
			}
			position, got, err := second.NextRow(t.Context())
			if err != nil || position != 0 || !cmp.Equal(got, rows[0], cmpopts.EquateEmpty()) {
				t.Fatalf("independent prefix = %v, %v", got, err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
				t.Fatalf("leaked ownership: %+v", usage)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("remaining files %v, %v", entries, err)
			}
		})
	}
}

func TestMigrationKeepsPrefixPositions(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	limits := testLimits()
	// One integer row is 32 header + 15 composite envelope + 10 value bytes.
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	for i := range 3 {
		p, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(int64(i))})
		if err != nil || p != Position(i*57) {
			t.Fatalf("append = %d, %v", p, err)
		}
		if s.Spilled() != (i > 0) {
			t.Fatalf("migration at row %d: %v", i, s.Spilled())
		}
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	for i := range 3 {
		p, row, err := r.NextRow(t.Context())
		if err != nil || p != Position(i*57) || row[0].IntVal != int64(i) {
			t.Fatalf("migrated prefix %d: %d, %v, %v", i, p, row, err)
		}
	}
}

func TestExactCapsRejectWithoutAdmittingRows(t *testing.T) {
	for _, capKind := range []string{"rows", "bytes", "record"} {
		t.Run(capKind, func(t *testing.T) {
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
			limits := testLimits()
			switch capKind {
			case "rows":
				limits.MaxRows = 1
			case "bytes":
				limits.MaxBytes = 57
			case "record":
				limits.MaxRecordBytes = 57
			}
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeString)}, limits)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.StringVal("")}); err != nil {
				t.Fatal(err)
			}
			if s.Bytes() != 57 {
				t.Fatalf("one row bytes = %d", s.Bytes())
			}
			if _, err := s.AppendRow(t.Context(), []model.Value{model.StringVal("x")}); err == nil {
				t.Fatal("accepted over-limit row")
			}
			if s.Rows() != 1 || s.Bytes() != 57 {
				t.Fatalf("partially admitted: %d rows, %d bytes", s.Rows(), s.Bytes())
			}
		})
	}
}

func TestSpillDisabledPreservesMemoryPrefix(t *testing.T) {
	q := testQuery(t, nil, runtime.QueryConfig{SpillMode: runtime.SpillDisabled})
	limits := testLimits()
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(3)}); err != nil {
		t.Fatal(err)
	}
	_, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(4)})
	var state *model.Error
	if !errors.Is(err, runtime.ErrSpillDisabled) || !errors.As(err, &state) || state.Code != "53200" {
		t.Fatalf("spill disabled = %v", err)
	}
	if s.Rows() != 1 || s.Bytes() != 57 || s.Spilled() {
		t.Fatal("failed migration changed prefix")
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	_, row, err := r.NextRow(t.Context())
	if err != nil || row[0].IntVal != 3 {
		t.Fatalf("prefix = %v, %v", row, err)
	}
}

func TestReaderAdmissionAndOwnerCloseJoin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spilled bool
		prefix  bool
	}{
		{name: "memory"},
		{name: "spill", spilled: true},
		{name: "prefix_memory", prefix: true},
		{name: "prefix_spill", spilled: true, prefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
			limits := testLimits()
			limits.MaxReaders = 1
			if tc.spilled {
				limits.MemoryBytes = 0
			}
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(42)}); err != nil {
				t.Fatal(err)
			}
			var r *Reader
			if tc.prefix {
				r = openPrefixReader(t, s)
			} else {
				if err := s.Seal(t.Context()); err != nil {
					t.Fatal(err)
				}
				r = openReader(t, s)
			}
			if _, err := s.PrefixReader(t.Context()); !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("reader admission = %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- s.Close() }()
			wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for {
				_, err := s.PrefixReader(t.Context())
				if errors.Is(err, runtime.ErrClosed) {
					break
				}
				if !errors.Is(err, runtime.ErrBudget) {
					t.Fatalf("reader while closing = %v", err)
				}
				if wait.Err() != nil {
					t.Fatal("owner did not stop reader admission")
				}
				goruntime.Gosched()
			}
			if _, err := s.Reader(t.Context()); !errors.Is(err, runtime.ErrClosed) {
				t.Fatalf("reader after owner close = %v", err)
			}
			select {
			case err := <-done:
				t.Fatalf("owner did not join reader: %v", err)
			default:
			}
			_, row, err := r.NextRow(t.Context())
			if err != nil || row[0].IntVal != 42 {
				t.Fatalf("pinned row = %v, %v", row, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-wait.Done():
				t.Fatal("owner did not finish after its reader closed")
			}
			if _, _, err := r.NextRow(t.Context()); !errors.Is(err, runtime.ErrClosed) {
				t.Fatalf("read after close = %v", err)
			}
			if q.Usage().Work.Live != 0 || q.Usage().Spill.Live != 0 {
				t.Fatalf("retained ownership: %+v", q.Usage())
			}
		})
	}
}

func TestCancellationDoesNotAdmitOrAdvance(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, testLimits())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.AppendRow(ctx, []model.Value{model.Int64Val(1)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append = %v", err)
	}
	if s.Rows() != 0 {
		t.Fatal("canceled row admitted")
	}
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seal = %v", err)
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reader(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader = %v", err)
	}
	r := openReader(t, s)
	if _, _, err := r.NextRow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
	p, row, err := r.NextRow(t.Context())
	if err != nil || p != 0 || row[0].IntVal != 2 {
		t.Fatalf("cursor advanced on cancellation: %v, %v", row, err)
	}
}

func TestAppendBatchAdmitsSelectionPrefix(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	layout := testLayout(model.TypeInt64)
	limits := testLimits()
	limits.MaxRows = 2
	s := newSpool(t, q, []model.TypeLayout{layout}, limits)
	batch := columnar.Batch{Length: 3, Selection: []uint32{2, 0, 1}, Columns: []columnar.Vector{{Type: layout.Type, Length: 3, Int64: []int64{10, 20, 30}}}}
	if n, err := s.AppendBatch(t.Context(), &batch); n != 2 || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("append prefix = %d, %v", n, err)
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	for _, want := range []int64{30, 10} {
		_, row, err := r.NextRow(t.Context())
		if err != nil || row[0].IntVal != want {
			t.Fatalf("selection row = %v, %v", row, err)
		}
	}
}

type faultFS struct {
	runtime.OSFileSystem
	file        *faultFile
	writeErr    error
	truncateErr error
	removeErr   error
}

func (fs *faultFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	fs.file = &faultFile{File: file, fs: fs}
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
	fs *faultFS
}

func (f *faultFile) WriteAt(data []byte, offset int64) (int, error) {
	if f.fs.writeErr != nil {
		n, err := f.File.WriteAt(data[:min(3, len(data))], offset)
		return n, errors.Join(err, f.fs.writeErr)
	}
	return f.File.WriteAt(data, offset)
}
func (f *faultFile) Truncate(size int64) error {
	if f.fs.truncateErr != nil {
		return f.fs.truncateErr
	}
	return f.File.Truncate(size)
}

func TestFailedMigrationRetainsReadablePrefix(t *testing.T) {
	fs := &faultFS{writeErr: syscall.ENOSPC}
	q := testQuery(t, fs, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(9)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(10)}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("migration failure = %v", err)
	}
	if s.Spilled() || s.Rows() != 1 || q.Usage().Spill.Live != 0 || q.Usage().Files != 0 {
		t.Fatalf("failed migration ownership: %+v", q.Usage())
	}
	fs.writeErr = nil
	if p, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(10)}); p != 57 || err != nil {
		t.Fatalf("retry = %d, %v", p, err)
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	for _, want := range []int64{9, 10} {
		_, row, err := r.NextRow(t.Context())
		if err != nil || row[0].IntVal != want {
			t.Fatalf("prefix = %v, %v", row, err)
		}
	}
}

func TestFailedRollbackAndUnlinkRetainDiskCharge(t *testing.T) {
	fs := &faultFS{writeErr: syscall.ENOSPC, truncateErr: syscall.EIO, removeErr: syscall.EACCES}
	q := testQuery(t, fs, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 0
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	_, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(4)})
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, syscall.EIO) || !errors.Is(err, runtime.ErrFenced) {
		t.Fatalf("failed rollback causes = %v", err)
	}
	if s.Rows() != 0 || q.Usage().Spill.Live != 57 {
		t.Fatalf("lost failed extent charge: %+v", q.Usage())
	}
	if err := s.Close(); !errors.Is(err, syscall.EACCES) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("cleanup causes = %v", err)
	}
	if q.Usage().Spill.Live != 57 || q.Usage().Work.Live != 0 {
		t.Fatalf("cleanup accounting = %+v", q.Usage())
	}
	if err := q.Close(); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("query cleanup cause = %v", err)
	}
	if err := os.Remove(fs.file.Name()); err != nil {
		t.Fatal(err)
	}
}
