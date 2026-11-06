package recursive_test

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

var int8 = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}

func fixture(t *testing.T) (*runtime.Query, string, recursive.Options) {
	t.Helper()
	dir := t.TempDir()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(64<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 32 << 20, OutputBytes: 8 << 20, SpillBytes: 16 << 20, Directory: dir, MaxFiles: 32, MaxReaders: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return q, dir, recursive.Options{
		Layouts: []model.TypeLayout{int8}, All: true, Query: q, Encoder: encoder,
		BatchRows: 2, BatchBytes: 64 << 10, MaxIterations: 32,
		Spool: rowstore.Limits{MemoryBytes: 0, MaxBytes: 1 << 20, MaxRows: 1024, MaxRecordBytes: 1024, MetadataBytes: 16 << 10, DecodeBytes: 8192, MaxReaders: 4},
		Seen:  distinctset.Options{MemoryBytes: 0, WorkBytes: 1 << 20, MaxKeyBytes: 1024, MaxKeys: 1024, MaxLevels: 16},
	}
}

func released(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Fatalf("resources retained before Query.Close: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill files=%v, error=%v", files, err)
	}
}

type source struct {
	builder           *columnar.Builder
	values            []int64
	index             int
	closed            bool
	failure, closeErr error
}

func newSource(t *testing.T, values ...int64) *source {
	t.Helper()
	builder, err := columnar.NewBuilder([]model.TypeLayout{int8}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: builder, values: values}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	if s.failure != nil {
		return s.failure
	}
	if s.index == len(s.values) {
		return io.EOF
	}
	s.builder.Reset()
	if err := s.builder.Append(ctx, []model.Value{model.Int64Val(s.values[s.index])}); err != nil {
		return err
	}
	s.index++
	return s.builder.Publish(out)
}

func (s *source) Close() error {
	s.closed = true
	return errors.Join(s.builder.Close(), s.closeErr)
}

type stepFunc func(context.Context, recursive.Worktable) (storage.BatchCursor, error)

func (f stepFunc) Open(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
	return f(ctx, table)
}

type transform struct {
	ctx      context.Context
	input    storage.BatchCursor
	builder  *columnar.Builder
	mapValue func(int64) (int64, bool)
	batch    columnar.Batch
	closed   bool
}

func mappedStep(t *testing.T, f func(int64) (int64, bool)) recursive.StepFactory {
	t.Helper()
	return stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		input, err := table.Open(ctx)
		if err != nil {
			return nil, err
		}
		builder, err := columnar.NewBuilder([]model.TypeLayout{int8}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 8192})
		if err != nil {
			return nil, errors.Join(err, input.Close())
		}
		return &transform{ctx: ctx, input: input, builder: builder, mapValue: f}, nil
	})
}

func (s *transform) Next(ctx context.Context, out *columnar.Batch) error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	for {
		s.batch.MaxRows = 1
		if err := s.input.Next(ctx, &s.batch); err != nil {
			return err
		}
		value, keep := s.mapValue(s.batch.Columns[0].Int64[s.batch.Lane(0)])
		if !keep {
			continue
		}
		s.builder.Reset()
		if err := s.builder.Append(ctx, []model.Value{model.Int64Val(value)}); err != nil {
			return err
		}
		return s.builder.Publish(out)
	}
}

func (s *transform) Close() error {
	s.closed = true
	return errors.Join(s.input.Close(), s.builder.Close())
}

func open(t *testing.T, anchor storage.BatchCursor, o recursive.Options) storage.BatchCursor {
	t.Helper()
	c, err := recursive.New(t.Context(), anchor, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func drain(t *testing.T, c storage.BatchCursor) ([]int64, error) {
	t.Helper()
	var got []int64
	out := columnar.Batch{MaxRows: 1}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		if out.Rows() != 1 {
			t.Fatalf("rows=%d, want 1", out.Rows())
		}
		got = append(got, out.Columns[0].Int64[out.Lane(0)])
	}
}

func TestRecursiveSequenceAcrossSpilledDeltas(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v + 1, v < 3 || v >= 10 && v < 12 })
	c := open(t, newSource(t, 1, 10), o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 10, 2, 11, 3, 12}) {
		t.Fatalf("sequence=%v, error=%v", got, err)
	}
	if q.Usage().PeakFiles < 2 {
		t.Fatal("expected simultaneous spilled input and output deltas")
	}
	released(t, q, dir)
}

func TestUnionTerminatesCycleIncludingAnchorDuplicates(t *testing.T) {
	q, dir, o := fixture(t)
	o.All = false
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return 3 - v, true })
	c := open(t, newSource(t, 1, 1), o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("distinct cycle=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestUnionAllPreservesMultiplicity(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v + 1, v < 2 })
	c := open(t, newSource(t, 1, 1), o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 1, 2, 2}) {
		t.Fatalf("bag=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestUnionAllCycleHitsIterationLimit(t *testing.T) {
	q, dir, o := fixture(t)
	o.MaxIterations = 2
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v, true })
	c := open(t, newSource(t, 1), o)
	got, err := drain(t, c)
	var sqlError *model.Error
	if !errors.Is(err, runtime.ErrBudget) || !errors.As(err, &sqlError) || sqlError.Code != "53200" || !slices.Equal(got, []int64{1, 1, 1}) {
		t.Fatalf("bounded cycle=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestEmptyAnchorDoesNotOpenStep(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = stepFunc(func(context.Context, recursive.Worktable) (storage.BatchCursor, error) {
		t.Fatal("step opened for an empty anchor")
		return nil, nil
	})
	c := open(t, newSource(t), o)
	got, err := drain(t, c)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty anchor=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestFilteredDeltaEndsRecursion(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return 0, false })
	c := open(t, newSource(t, 1, 2), o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("filtered delta=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestEarlyCloseDoesNotOpenStep(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = stepFunc(func(context.Context, recursive.Worktable) (storage.BatchCursor, error) {
		t.Fatal("step eagerly opened before anchor output was consumed")
		return nil, nil
	})
	anchor := newSource(t, 1, 2)
	c := open(t, anchor, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil || out.Rows() != 1 {
		t.Fatalf("first output rows=%d, error=%v", out.Rows(), err)
	}
	if err := c.Close(); err != nil || !anchor.closed {
		t.Fatalf("early close error=%v, anchor closed=%v", err, anchor.closed)
	}
	released(t, q, dir)
}

func TestStepContextSurvivesCompletedNext(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v + 10, v < 10 })
	c := open(t, newSource(t, 1, 2), o)
	var got []int64
	for range 4 {
		ctx, cancel := context.WithCancel(t.Context())
		out := columnar.Batch{MaxRows: 1}
		err := c.Next(ctx, &out)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.Columns[0].Int64[out.Lane(0)])
	}
	tail, err := drain(t, c)
	if err != nil || len(tail) != 0 || !slices.Equal(got, []int64{1, 2, 11, 12}) {
		t.Fatalf("cross-call outputs=%v, tail=%v, error=%v", got, tail, err)
	}
	released(t, q, dir)
}

func TestStepOpenErrorClosesReturnedCursorAndOrphanReader(t *testing.T) {
	q, dir, o := fixture(t)
	failure := errors.New("step open failed")
	returned := newSource(t)
	var orphan storage.BatchCursor
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		var err error
		orphan, err = table.Open(ctx)
		if err != nil {
			return nil, err
		}
		return returned, failure
	})
	c := open(t, newSource(t, 1), o)
	got, err := drain(t, c)
	if !errors.Is(err, failure) || !returned.closed || !slices.Equal(got, []int64{1}) {
		t.Fatalf("outputs=%v, error=%v, returned closed=%v", got, err, returned.closed)
	}
	var out columnar.Batch
	if err := orphan.Next(t.Context(), &out); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("orphan reader not fenced: %v", err)
	}
	released(t, q, dir)
}

func TestStepReadErrorReleasesSpilledWorktable(t *testing.T) {
	q, dir, o := fixture(t)
	failure := errors.New("step read failed")
	returned := newSource(t)
	returned.failure = failure
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		if _, err := table.Open(ctx); err != nil {
			return nil, err
		}
		return returned, nil
	})
	c := open(t, newSource(t, 1), o)
	_, err := drain(t, c)
	if !errors.Is(err, failure) || !returned.closed {
		t.Fatalf("read error=%v, step closed=%v", err, returned.closed)
	}
	released(t, q, dir)
}

func TestEarlyCloseFencesAllIndependentReaders(t *testing.T) {
	q, dir, o := fixture(t)
	var readers [2]storage.BatchCursor
	var table recursive.Worktable
	o.Step = stepFunc(func(ctx context.Context, work recursive.Worktable) (storage.BatchCursor, error) {
		table = work
		for i := range readers {
			var err error
			readers[i], err = work.Open(ctx)
			if err != nil {
				return nil, err
			}
		}
		first, second := columnar.Batch{MaxRows: 1}, columnar.Batch{MaxRows: 1}
		if err := readers[0].Next(ctx, &first); err != nil {
			return nil, err
		}
		if err := readers[1].Next(ctx, &second); err != nil {
			return nil, err
		}
		if first.Columns[0].Int64[0] != 1 || second.Columns[0].Int64[0] != 1 {
			t.Fatal("worktable readers did not start independently")
		}
		return newSource(t, 2), nil
	})
	c := open(t, newSource(t, 1), o)
	out := columnar.Batch{MaxRows: 1}
	for range 2 {
		if err := c.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		if err := reader.Next(t.Context(), &out); !errors.Is(err, runtime.ErrClosed) {
			t.Fatalf("reader remained live: %v", err)
		}
	}
	if _, err := table.Open(t.Context()); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("expired worktable reopened: %v", err)
	}
	released(t, q, dir)
}

func TestCancellationInterruptsStepOpen(t *testing.T) {
	q, dir, o := fixture(t)
	started := make(chan struct{})
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		if _, err := table.Open(ctx); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	c := open(t, newSource(t, 1), o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Next(ctx, &out) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Open returned %v", err)
	}
	released(t, q, dir)
}

type blockingRead struct {
	started chan struct{}
	closed  bool
}

func (s *blockingRead) Next(ctx context.Context, out *columnar.Batch) error {
	close(s.started)
	<-ctx.Done()
	return context.Cause(ctx)
}

func (s *blockingRead) Close() error { s.closed = true; return nil }

func TestCancellationInterruptsStepRead(t *testing.T) {
	q, dir, o := fixture(t)
	step := &blockingRead{started: make(chan struct{})}
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		if _, err := table.Open(ctx); err != nil {
			return nil, err
		}
		return step, nil
	})
	c := open(t, newSource(t, 1), o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Next(ctx, &out) }()
	<-step.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) || !step.closed {
		t.Fatalf("cancelled read error=%v, step closed=%v", err, step.closed)
	}
	released(t, q, dir)
}

func TestConstructorRejectionClosesAnchor(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v, true })
	o.Spool.MetadataBytes = 1
	anchor := newSource(t, 1)
	c, err := recursive.New(t.Context(), anchor, o)
	if c != nil || !errors.Is(err, runtime.ErrBudget) || !anchor.closed || anchor.index != 0 {
		t.Fatalf("admission cursor=%v, error=%v, anchor closed=%v, consumed=%d", c, err, anchor.closed, anchor.index)
	}
	released(t, q, dir)
}

func TestSpoolLimitFailureReleasesPrefix(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v, true })
	o.Spool.MaxRows = 1
	c := open(t, newSource(t, 1, 2), o)
	got, err := drain(t, c)
	if !errors.Is(err, runtime.ErrBudget) || !slices.Equal(got, []int64{1}) {
		t.Fatalf("limited spool=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestCloseErrorIsNotIterationEOF(t *testing.T) {
	q, dir, o := fixture(t)
	failure := errors.New("anchor close failed")
	anchor := newSource(t, 1)
	anchor.closeErr = failure
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v, true })
	c := open(t, anchor, o)
	got, err := drain(t, c)
	if !errors.Is(err, failure) || !slices.Equal(got, []int64{1}) {
		t.Fatalf("close failure output=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

func TestOutputBatchCrossesDeltasWithoutExceedingDemand(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = mappedStep(t, func(v int64) (int64, bool) { return v + 1, v < 3 })
	c := open(t, newSource(t, 1), o)
	var out columnar.Batch
	if err := c.Next(t.Context(), &out); err != nil || out.Rows() != 2 || !slices.Equal(out.Columns[0].Int64, []int64{1, 2}) {
		t.Fatalf("first vector=%+v, error=%v", out, err)
	}
	out.MaxRows = 1
	if err := c.Next(t.Context(), &out); err != nil || out.Rows() != 1 || out.Columns[0].Int64[0] != 3 {
		t.Fatalf("limited vector=%+v, error=%v", out, err)
	}
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("final Next=%v, want EOF", err)
	}
	released(t, q, dir)
}

func TestWorktableBatchesHonorIndependentDemand(t *testing.T) {
	q, dir, o := fixture(t)
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		r, err := table.Open(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		var batch columnar.Batch
		if err := r.Next(ctx, &batch); err != nil || batch.Rows() != 2 || !slices.Equal(batch.Columns[0].Int64, []int64{1, 2}) {
			t.Fatalf("worktable vector=%+v, error=%v", batch, err)
		}
		batch.MaxRows = 1
		if err := r.Next(ctx, &batch); err != nil || batch.Rows() != 1 || batch.Columns[0].Int64[0] != 3 {
			t.Fatalf("worktable tail=%+v, error=%v", batch, err)
		}
		if err := r.Next(ctx, &batch); err != io.EOF {
			t.Fatalf("worktable EOF=%v", err)
		}
		return newSource(t), nil
	})
	c := open(t, newSource(t, 1, 2, 3), o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("output=%v, error=%v", got, err)
	}
	released(t, q, dir)
}

type emptyBatch struct {
	storage.BatchCursor
	empty bool
}

func (s *emptyBatch) Next(ctx context.Context, out *columnar.Batch) error {
	if !s.empty {
		s.empty = true
		out.Reset()
		return nil
	}
	return s.BatchCursor.Next(ctx, out)
}

func TestEmptyBatchesAreNotIterationEOF(t *testing.T) {
	q, dir, o := fixture(t)
	step := mappedStep(t, func(v int64) (int64, bool) { return v + 1, v < 2 })
	o.Step = stepFunc(func(ctx context.Context, table recursive.Worktable) (storage.BatchCursor, error) {
		c, err := step.Open(ctx, table)
		if err != nil {
			return c, err
		}
		return &emptyBatch{BatchCursor: c}, nil
	})
	c := open(t, &emptyBatch{BatchCursor: newSource(t, 1)}, o)
	got, err := drain(t, c)
	if err != nil || !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("empty-batch recursion=%v, error=%v", got, err)
	}
	released(t, q, dir)
}
