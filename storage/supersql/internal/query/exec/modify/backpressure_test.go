package modify_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
)

// The failure is armed by observed transaction writes, not by a spill filename
// or a particular number of source/sort files.
type mutationSpillFS struct {
	runtime.OSFileSystem
	armed   atomic.Bool
	failed  atomic.Bool
	partial atomic.Int64
	open    atomic.Int64
}

func (fs *mutationSpillFS) CreateTemp(directory, pattern string) (runtime.SpillFile, error) {
	file, err := fs.OSFileSystem.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	fs.open.Add(1)
	return &mutationSpillFile{SpillFile: file, fs: fs}, nil
}

type mutationSpillFile struct {
	runtime.SpillFile
	fs *mutationSpillFS
}

func (file *mutationSpillFile) WriteAt(data []byte, offset int64) (int, error) {
	if !file.fs.armed.CompareAndSwap(true, false) {
		return file.SpillFile.WriteAt(data, offset)
	}
	n, err := file.SpillFile.WriteAt(data[:len(data)/2], offset)
	file.fs.partial.Store(int64(n))
	file.fs.failed.Store(true)
	return n, errors.Join(err, &os.PathError{Op: "write", Path: file.Name(), Err: syscall.ENOSPC})
}

func (file *mutationSpillFile) Close() error {
	err := file.SpillFile.Close()
	if err == nil {
		file.fs.open.Add(-1)
	}
	return err
}

type witnessedMutation struct {
	*txn.Transaction
	fs     *mutationSpillFS
	layout []storage.ColumnLayout
	rows   []model.Row
}

func (transaction *witnessedMutation) ApplyMutations(ctx context.Context, mutations []storage.Mutation) error {
	if err := transaction.Transaction.ApplyMutations(ctx, mutations); err != nil {
		return err
	}
	for _, mutation := range mutations {
		row, err := transaction.Transaction.Get(ctx, storage.ReadRequest{Target: mutation.Target, Key: mutation.Key, ColumnIDs: mutation.ColumnIDs, Layout: transaction.layout})
		if err != nil {
			return err
		}
		transaction.rows = append(transaction.rows, row)
	}
	if len(transaction.rows) == 2 {
		transaction.fs.armed.Store(true)
	}
	return nil
}

func (transaction *witnessedMutation) ApplyPreparedInsert(ctx context.Context, candidate *txn.InsertCandidate) error {
	if err := transaction.Transaction.ApplyPreparedInsert(ctx, candidate); err != nil {
		return err
	}
	row, _, err := candidate.CopyRow(ctx, 1<<20)
	if err != nil {
		return err
	}
	transaction.rows = append(transaction.rows, row)
	if len(transaction.rows) == 2 {
		transaction.fs.armed.Store(true)
	}
	return nil
}

func constrainedMutationStore(t *testing.T, fs runtime.FileSystem) *store {
	t.Helper()
	s := newStore(t)
	if err := s.query.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(2<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s.query, err = runtime.NewQuery(t.Context(), pool, budget, fs, runtime.QueryConfig{WorkBytes: 1 << 20, OutputBytes: 64 << 10, SpillBytes: 8 << 20, Directory: s.directory, MaxFiles: 8, MaxReaders: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.query.Close(); err != nil {
			t.Error(err)
		}
	})
	s.options.Query = s.query
	s.options.WorkBytes = 4 << 20
	return s
}

func constrainedMutationEnvironment(env *execution.Environment) {
	env.Limits.ParameterBytes = 4096
	env.Limits.MetadataBytes = 64 << 10
	env.Limits.BatchBytes = 64 << 10
	env.Limits.Expressions.MaxBytes = 64 << 10
	env.Limits.Expressions.MaxConstantBytes = 16 << 10
	env.Limits.Expressions.MaxStateBytes = 64 << 10
	env.Limits.Expressions.MaxOutputBytes = 32 << 10
}

func reopenedMutationSnapshot(t *testing.T, s *store) storage.SnapshotReader {
	t.Helper()
	if err := s.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.backend.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s.backend, err = neosepia.Open(t.Context(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.openManager(t)
	reader, err := s.manager.StrongSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func TestPortalBackpressureAndSpill(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	t.Run("ENOSPC restores only the failed statement", func(t *testing.T) {
		fs := &mutationSpillFS{}
		s := constrainedMutationStore(t, fs)
		s.seed(t, row(9, 90))
		before, err := s.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		witness := &witnessedMutation{Transaction: s.tx, fs: fs, layout: s.options.Layout}
		s.options.Transaction = witness
		portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES(1,10),(2,20),(3,30) RETURNING new.id,new.value`, constrainedMutationEnvironment)
		batch := columnar.Batch{MaxRows: 1}
		err = portal.Next(t.Context(), &batch)
		var sqlErr *model.Error
		if !errors.Is(err, syscall.ENOSPC) || !errors.As(err, &sqlErr) || sqlErr.Code != "53100" {
			t.Fatalf("spill error = %v, want SQLSTATE 53100 retaining ENOSPC", err)
		}
		if !fs.failed.Load() || fs.partial.Load() == 0 {
			t.Fatalf("failure did not follow a real partial file write: failed=%v bytes=%d", fs.failed.Load(), fs.partial.Load())
		}
		if len(witness.rows) != 2 {
			t.Fatalf("rows visible inside the failed statement = %d, want 2", len(witness.rows))
		}
		for i, got := range witness.rows {
			id := int64(i + 1)
			if got.Deleted || len(got.Values) != 2 || got.Values[0].IntVal != id || got.Values[1].IntVal != id*10 {
				t.Fatalf("pre-failure row %d = %+v, want (%d,%d)", id, got, id, id*10)
			}
		}
		if batch.Rows() != 0 || portal.RowsAffected() != 0 {
			t.Fatalf("failed statement published %d rows, affected=%d", batch.Rows(), portal.RowsAffected())
		}
		if err := portal.Close(); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{1, 2, 3} {
			s.absent(t, id)
		}
		s.require(t, 9, 90)
		requireClean(t, s)
		if fs.open.Load() != 0 {
			t.Fatalf("open spill descriptors after error: %d", fs.open.Load())
		}
		after, err := s.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.CommitTS != before.CommitTS || after.LogPosition != before.LogPosition {
			t.Fatalf("failed statement changed durable commit state: before=%+v after=%+v", before, after)
		}
		if s.tx.State() != txn.Active {
			t.Fatalf("failed statement ended caller transaction: %v", s.tx.State())
		}
		s.seed(t, row(8, 80))
		if _, err := s.tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		reader := reopenedMutationSnapshot(t, s)
		for _, id := range []int64{1, 2, 3, 8, 9} {
			got, err := reader.Get(t.Context(), s.request(t, id))
			if id < 8 {
				if !errors.Is(err, storage.ErrRowNotFound) {
					t.Fatalf("failed statement row %d became durable: %+v, %v", id, got, err)
				}
				continue
			}
			if err != nil || len(got.Values) != 2 || got.Values[0].IntVal != id || got.Values[1].IntVal != id*10 {
				t.Fatalf("surviving transaction row %d = %+v, %v", id, got, err)
			}
		}
	})

	for _, commit := range []bool{false, true} {
		name := "early RETURNING close followed by caller rollback"
		if commit {
			name = "early RETURNING close followed by caller commit"
		}
		t.Run(name, func(t *testing.T) {
			fs := &mutationSpillFS{}
			s := constrainedMutationStore(t, fs)
			s.seed(t, row(9, 90))
			portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES(1,10),(2,20),(3,30) RETURNING new.id,new.value`, constrainedMutationEnvironment)
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 {
				t.Fatalf("first RETURNING batch has %d rows, want 1", batch.Rows())
			}
			for column, want := range []int64{1, 10} {
				got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(0), 1024)
				if err != nil || got.IsNull || got.IntVal != want {
					t.Fatalf("first RETURNING column %d = %+v, %v; want %d", column, got, err, want)
				}
			}
			if s.query.Usage().Files == 0 {
				t.Fatal("RETURNING did not retain a spill while demand was paused")
			}
			if err := portal.Close(); err != nil {
				t.Fatal(err)
			}
			if portal.RowsAffected() != 3 {
				t.Fatalf("early close staged %d rows, want the entire 3-row statement", portal.RowsAffected())
			}
			for _, id := range []int64{1, 2, 3, 9} {
				s.require(t, id, id*10)
			}
			requireClean(t, s)
			if fs.open.Load() != 0 {
				t.Fatalf("open spill descriptors after close: %d", fs.open.Load())
			}
			if s.tx.State() != txn.Active {
				t.Fatalf("early close ended caller transaction: %v", s.tx.State())
			}
			observer, err := s.manager.StrongSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []int64{1, 2, 3, 9} {
				got, err := observer.Get(t.Context(), s.request(t, id))
				if !errors.Is(err, storage.ErrRowNotFound) {
					t.Errorf("uncommitted row %d visible to durable observer: %+v, %v", id, got, err)
				}
			}
			if err := observer.Close(); err != nil {
				t.Fatal(err)
			}
			if commit {
				if _, err := s.tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if err := s.tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			reader := reopenedMutationSnapshot(t, s)
			for _, id := range []int64{1, 2, 3, 9} {
				got, err := reader.Get(t.Context(), s.request(t, id))
				if !commit {
					if !errors.Is(err, storage.ErrRowNotFound) {
						t.Fatalf("caller rollback left durable row %d: %+v, %v", id, got, err)
					}
					continue
				}
				if err != nil || len(got.Values) != 2 || got.Values[0].IntVal != id || got.Values[1].IntVal != id*10 {
					t.Fatalf("caller commit omitted statement row %d: %+v, %v", id, got, err)
				}
			}
		})
	}
}
