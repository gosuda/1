package rowstore

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func openPrefixReader(t *testing.T, s *Spool) *Reader {
	t.Helper()
	r, err := s.PrefixReader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func expectPrefixInt(t *testing.T, r *Reader, position Position, value int64) {
	t.Helper()
	got, row, err := r.NextRow(t.Context())
	if err != nil || got != position || len(row) != 1 || row[0].IntVal != value {
		t.Fatalf("row at %d = %d, %v, %v; want %d", position, got, row, err, value)
	}
}

func expectPrefixEnd(t *testing.T, r *Reader, position Position) {
	t.Helper()
	got, row, err := r.NextRow(t.Context())
	if !errors.Is(err, io.EOF) || got != position || row != nil {
		t.Fatalf("prefix end = %d, %v, %v; want EOF at %d", got, row, err, position)
	}
}

func TestPrefixReaderCapturesAppendExtent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory uint64
	}{
		{name: "memory", memory: 4096},
		{name: "spill", memory: 0},
		{name: "migration", memory: 57},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
			limits := testLimits()
			limits.MemoryBytes = tc.memory
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
			if _, err := s.Reader(t.Context()); !errors.Is(err, ErrNotSealed) {
				t.Fatalf("unsealed Reader = %v", err)
			}
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(11)}); err != nil {
				t.Fatal(err)
			}
			first := openPrefixReader(t, s)
			expectPrefixInt(t, first, 0, 11)
			expectPrefixEnd(t, first, 57)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(22)}); err != nil {
				t.Fatal(err)
			}
			expectPrefixEnd(t, first, 57)
			if _, err := first.Read(t.Context(), 57); !errors.Is(err, ErrPosition) {
				t.Fatalf("read beyond prefix = %v", err)
			}
			if err := first.Seek(t.Context(), 114); !errors.Is(err, ErrPosition) {
				t.Fatalf("seek beyond prefix = %v", err)
			}
			if err := first.Seek(t.Context(), 0); err != nil {
				t.Fatal(err)
			}
			expectPrefixInt(t, first, 0, 11)
			second := openPrefixReader(t, s)
			if err := second.Seek(t.Context(), 57); err != nil {
				t.Fatal(err)
			}
			expectPrefixInt(t, second, 57, 22)
			expectPrefixEnd(t, second, 114)
			if err := s.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			expectPrefixEnd(t, first, 57)
			if tc.name == "memory" && (s.Spilled() || q.Usage().Files != 0) {
				t.Fatalf("small prefix forced spill: %+v", q.Usage())
			}
		})
	}
}

func TestEmptyPrefixStaysEmptyAfterAppend(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, testLimits())
	empty := openPrefixReader(t, s)
	expectPrefixEnd(t, empty, 0)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(7)}); err != nil {
		t.Fatal(err)
	}
	if err := empty.Seek(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	expectPrefixEnd(t, empty, 0)
	if _, err := empty.Read(t.Context(), 0); !errors.Is(err, ErrPosition) {
		t.Fatalf("empty random read = %v", err)
	}
	expectPrefixInt(t, openPrefixReader(t, s), 0, 7)
}

func TestPrefixReaderSurvivesAppendRollback(t *testing.T) {
	for _, memory := range []uint64{0, 57} {
		name := "spill"
		if memory != 0 {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			fs := &faultFS{}
			q := testQuery(t, fs, runtime.QueryConfig{})
			limits := testLimits()
			limits.MemoryBytes = memory
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(9)}); err != nil {
				t.Fatal(err)
			}
			r := openPrefixReader(t, s)
			before := q.Usage()
			fs.writeErr = syscall.ENOSPC
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(10)}); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("append failure = %v", err)
			}
			if s.Rows() != 1 || s.Bytes() != 57 {
				t.Fatalf("failed append admitted data: rows=%d, bytes=%d", s.Rows(), s.Bytes())
			}
			if after := q.Usage(); after.Spill.Live != before.Spill.Live || after.Files != before.Files || after.Readers != before.Readers {
				t.Fatalf("rollback leaked ownership: %+v -> %+v", before, after)
			}
			expectPrefixInt(t, r, 0, 9)
			expectPrefixEnd(t, r, 57)
			afterFailure := openPrefixReader(t, s)
			expectPrefixInt(t, afterFailure, 0, 9)
			expectPrefixEnd(t, afterFailure, 57)
			if err := afterFailure.Close(); err != nil {
				t.Fatal(err)
			}
			fs.writeErr = nil
			if position, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(10)}); err != nil || position != 57 {
				t.Fatalf("retry = %d, %v", position, err)
			}
			expectPrefixEnd(t, r, 57)
			if err := r.Seek(t.Context(), 0); err != nil {
				t.Fatal(err)
			}
			expectPrefixInt(t, r, 0, 9)
			fresh := openPrefixReader(t, s)
			if err := fresh.Seek(t.Context(), 57); err != nil {
				t.Fatal(err)
			}
			expectPrefixInt(t, fresh, 57, 10)
		})
	}
}

func TestPrefixReaderCancellationPreservesCursor(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(3)}); err != nil {
		t.Fatal(err)
	}
	r := openPrefixReader(t, s)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(4)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := q.Usage()
	if _, err := s.PrefixReader(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader = %v", err)
	}
	if _, err := s.AppendRow(ctx, []model.Value{model.Int64Val(5)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append = %v", err)
	}
	if _, _, err := r.NextRow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
	if _, err := r.Read(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled random read = %v", err)
	}
	if err := r.Seek(ctx, 57); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seek = %v", err)
	}
	if after := q.Usage(); after.Work.Live != before.Work.Live || after.Work.Reservations != before.Work.Reservations || after.Readers != before.Readers || after.Spill.Live != before.Spill.Live {
		t.Fatalf("cancellation changed ownership: %+v -> %+v", before, after)
	}
	expectPrefixInt(t, r, 0, 3)
	expectPrefixEnd(t, r, 57)
}

func TestPrefixReaderMigrationAdmissionCanRetry(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{MaxReaders: 1})
	limits := testLimits()
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(6)}); err != nil {
		t.Fatal(err)
	}
	memory := openPrefixReader(t, s)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(8)}); err != nil {
		t.Fatal(err)
	}
	file := openPrefixReader(t, s)
	before := q.Usage()
	if _, _, err := memory.NextRow(t.Context()); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("migration reader admission = %v", err)
	}
	if after := q.Usage(); after.Work.Live != before.Work.Live || after.Work.Reservations != before.Work.Reservations || after.Readers != before.Readers {
		t.Fatalf("failed migration admission leaked: %+v -> %+v", before, after)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	expectPrefixInt(t, memory, 0, 6)
	expectPrefixEnd(t, memory, 57)
	if err := memory.Close(); err != nil {
		t.Fatal(err)
	}
	if q.Usage().Readers != 0 || q.Usage().Files != 1 {
		t.Fatalf("reader close ownership = %+v", q.Usage())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("retained ownership = %+v", usage)
	}
}

func TestPrefixReaderReportsCorruptPayload(t *testing.T) {
	fs := &faultFS{}
	q := testQuery(t, fs, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 0
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(17)}); err != nil {
		t.Fatal(err)
	}
	r := openPrefixReader(t, s)
	if _, err := fs.file.WriteAt([]byte{0xff}, frameHeaderBytes); err != nil {
		t.Fatal(err)
	}
	position, _, err := r.NextRow(t.Context())
	if !errors.Is(err, ErrCorrupt) || position != 0 {
		t.Fatalf("corrupt prefix = %d, %v", position, err)
	}
	if _, err := r.Read(t.Context(), 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt random read = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("corruption leaked ownership = %+v", usage)
	}
}

func TestPrefixReaderWorkspaceAdmissionReturnsClaims(t *testing.T) {
	const workBytes = 64 << 10
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: workBytes})
	s := newSpool(t, q, nil, testLimits())
	claim, err := q.TryReserve(runtime.WorkMemory, workBytes-q.Usage().Work.Live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claim.Release)
	before := q.Usage()
	if _, err := s.PrefixReader(t.Context()); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("workspace admission = %v", err)
	}
	if after := q.Usage(); after.Work.Live != before.Work.Live || after.Work.Reservations != before.Work.Reservations || after.Readers != before.Readers {
		t.Fatalf("failed admission leaked ownership: %+v -> %+v", before, after)
	}
	claim.Release()
	r := openPrefixReader(t, s)
	expectPrefixEnd(t, r, 0)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("retained ownership = %+v", usage)
	}
}

func TestPrefixReaderConcurrentMigration(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 57
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(13)}); err != nil {
		t.Fatal(err)
	}
	r := openPrefixReader(t, s)
	done := make(chan error, 1)
	go func() {
		for i := range 32 {
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(int64(i))}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 32 {
		if err := r.Seek(t.Context(), 0); err != nil {
			t.Fatal(err)
		}
		expectPrefixInt(t, r, 0, 13)
		expectPrefixEnd(t, r, 57)
	}
}
