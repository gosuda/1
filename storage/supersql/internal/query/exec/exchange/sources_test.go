package exchange

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var transportInt = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}

type transportCursor struct {
	next          func(context.Context, *columnar.Batch) error
	cleanup       func() error
	calls, closes atomic.Int32
}

func (c *transportCursor) Next(ctx context.Context, out *columnar.Batch) error {
	c.calls.Add(1)
	return c.next(ctx, out)
}
func (c *transportCursor) Close() error {
	c.closes.Add(1)
	if c.cleanup != nil {
		return c.cleanup()
	}
	return nil
}

func transportRuntime(t *testing.T, config runtime.QueryConfig) (*runtime.Query, *runtime.Pool) {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	root, err := runtime.NewBudget(1<<26, 1000)
	if err != nil {
		t.Fatal(err)
	}
	config.SpillMode = runtime.SpillDisabled
	q, err := runtime.NewQuery(t.Context(), pool, root, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close(); _ = pool.Close() })
	return q, pool
}

func transportOptions(q *runtime.Query) Options {
	return Options{Query: q, Layouts: []model.TypeLayout{transportInt}, BatchRows: 8, BatchBytes: 4096, QueueBytes: 8192, MetadataBytes: 1 << 18}
}

func transportInts(values ...int64) *transportCursor {
	index := 0
	lanes := make([]int64, 8)
	return &transportCursor{next: func(ctx context.Context, out *columnar.Batch) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		out.Reset()
		if index == len(values) {
			return io.EOF
		}
		count := min(len(lanes), len(values)-index)
		if out.MaxRows > 0 {
			count = min(count, out.MaxRows)
		}
		copy(lanes, values[index:index+count])
		index += count
		out.Columns = append(out.Columns, columnar.Vector{Type: transportInt.Type, Length: count, Int64: lanes[:count]})
		out.Length = count
		return nil
	}}
}

func transportValue(t *testing.T, batch *columnar.Batch, row int) int64 {
	t.Helper()
	value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(row), 4096)
	if err != nil {
		t.Fatal(err)
	}
	return value.IntVal
}

func TestSourcesKeepOtherInputViewsAlive(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	left, right := transportInts(11, 12, 13), transportInts(21, 22)
	s, err := NewSources(t.Context(), []storage.BatchCursor{left, right}, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if left.calls.Load()+right.calls.Load() != 0 {
		t.Fatal("constructor pulled input")
	}
	first := columnar.Batch{MaxRows: 1}
	if err := s.Next(t.Context(), 0, &first); err != nil {
		t.Fatal(err)
	}
	other := columnar.Batch{MaxRows: 1}
	for range 2 {
		if err := s.Next(t.Context(), 1, &other); err != nil {
			t.Fatal(err)
		}
	}
	if first.Rows() != 1 || transportValue(t, &first, 0) != 11 {
		t.Fatal("another source invalidated a borrowed view")
	}
	if left.calls.Load() != 1 {
		t.Fatalf("first demand advanced left %d times", left.calls.Load())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if left.closes.Load() != 1 || right.closes.Load() != 1 {
		t.Fatal("children not closed exactly once")
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 {
		t.Fatalf("live buffers after close: %+v", usage)
	}
}

func TestSourcesSaturatedPoolUsesCallerWithoutDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, _ := transportRuntime(t, runtime.QueryConfig{})
		started, release := make(chan struct{}), make(chan struct{})
		first, err := q.Tasks().Submit(t.Context(), runtime.Task{Run: func(ctx context.Context) error {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		second, err := q.Tasks().Submit(t.Context(), runtime.Task{Run: func(context.Context) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		options := transportOptions(q)
		options.ParallelEligible, options.EstimatedRows = true, 65536
		left, right := transportInts(1, 2), transportInts(3, 4)
		s, err := NewSources(t.Context(), []storage.BatchCursor{left, right}, options)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			out := columnar.Batch{MaxRows: 1}
			if err := s.Next(t.Context(), i, &out); err != nil {
				t.Fatal(err)
			}
			if got := transportValue(t, &out, 0); got != int64(1+2*i) {
				t.Fatalf("source %d value %d", i, got)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := errors.Join(first.Wait(t.Context()), second.Wait(t.Context())); err != nil {
			t.Fatal(err)
		}
		if err := q.Close(); err != nil {
			t.Fatalf("exchange poisoned query tasks: %v", err)
		}
	})
}

func TestSourcesCloseJoinsBlockedPullWithoutCancelingQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, _ := transportRuntime(t, runtime.QueryConfig{})
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		var exited, prematureClose atomic.Bool
		blocked := &transportCursor{next: func(ctx context.Context, out *columnar.Batch) error {
			defer exited.Store(true)
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		}}
		blocked.cleanup = func() error {
			prematureClose.Store(!exited.Load())
			return nil
		}
		options := transportOptions(q)
		options.ParallelEligible, options.EstimatedRows = true, 65536
		s, err := NewSources(t.Context(), []storage.BatchCursor{blocked, transportInts(9)}, options)
		if err != nil {
			t.Fatal(err)
		}
		nextDone := make(chan error, 1)
		go func() { nextDone <- s.Next(t.Context(), 0, &columnar.Batch{MaxRows: 1}) }()
		<-started
		closeDone := make(chan error, 1)
		go func() { closeDone <- s.Close() }()
		<-canceled
		close(release)
		if err := <-nextDone; !errors.Is(err, context.Canceled) && !errors.Is(err, runtime.ErrClosed) {
			t.Fatalf("interrupted Next: %v", err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if blocked.closes.Load() != 1 || prematureClose.Load() {
			t.Fatalf("child cleanup: closes=%d before pull exit=%v", blocked.closes.Load(), prematureClose.Load())
		}
		if q.Context().Err() != nil {
			t.Fatalf("exchange canceled query: %v", context.Cause(q.Context()))
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSourcesCallerCancellationStopsAdmittedPull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, _ := transportRuntime(t, runtime.QueryConfig{})
		started := make(chan struct{})
		blocked := &transportCursor{next: func(ctx context.Context, out *columnar.Batch) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		}}
		o := transportOptions(q)
		o.ParallelEligible, o.EstimatedRows = true, 65536
		s, err := NewSources(t.Context(), []storage.BatchCursor{blocked, transportInts(3)}, o)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Next(ctx, 0, &columnar.Batch{MaxRows: 1}) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled pull: %v", err)
		}
		if blocked.closes.Load() != 1 || q.Usage().Work.Live != 0 {
			t.Fatal("canceled pull retained input or memory")
		}
		if q.Context().Err() != nil {
			t.Fatal("caller cancellation escaped exchange")
		}
	})
}

func TestSourcesRetainSourceAndCleanupFailures(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	pullErr, closeErr := errors.New("pull failed"), errors.New("close failed")
	input := &transportCursor{next: func(context.Context, *columnar.Batch) error { return pullErr }, cleanup: func() error { return closeErr }}
	s, err := NewSources(t.Context(), []storage.BatchCursor{input}, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Next(t.Context(), 0, &columnar.Batch{}); !errors.Is(err, pullErr) || !errors.Is(err, closeErr) {
		t.Fatalf("lost causes: %v", err)
	}
	for range 2 {
		if err := s.Close(); errors.Is(err, pullErr) || !errors.Is(err, closeErr) {
			t.Fatalf("Close must retain cleanup, not replay execution: %v", err)
		}
	}
	if input.closes.Load() != 1 {
		t.Fatal("retried child Close")
	}
}

func TestSourcesPoolShutdownClosesEveryInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, pool := transportRuntime(t, runtime.QueryConfig{})
		o := transportOptions(q)
		o.ParallelEligible, o.EstimatedRows = true, 65536
		left, right := transportInts(1), transportInts(2)
		s, err := NewSources(t.Context(), []storage.BatchCursor{left, right}, o)
		if err != nil {
			t.Fatal(err)
		}
		_ = pool.Close()
		if err := s.Next(t.Context(), 0, &columnar.Batch{}); err == nil || err == io.EOF {
			t.Fatalf("shutdown accepted pull: %v", err)
		}
		_ = s.Close()
		if left.closes.Load() != 1 || right.closes.Load() != 1 || left.calls.Load()+right.calls.Load() != 0 {
			t.Fatal("shutdown pulled or leaked inputs")
		}
	})
}

func TestSourcesDoNotRetainSuccessfulNextContext(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	o := transportOptions(q)
	o.ParallelEligible, o.EstimatedRows = true, 65536
	s, err := NewSources(t.Context(), []storage.BatchCursor{transportInts(1, 2), transportInts(3, 4)}, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	out := columnar.Batch{MaxRows: 1}
	if err := s.Next(ctx, 0, &out); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.Next(t.Context(), 1, &out); err != nil {
		t.Fatal(err)
	}
	if transportValue(t, &out, 0) != 3 {
		t.Fatal("canceled prior call invalidated another source")
	}
	if err := s.Next(t.Context(), 0, &out); err != nil {
		t.Fatal(err)
	}
	if transportValue(t, &out, 0) != 2 {
		t.Fatal("canceled prior call invalidated later pull")
	}
}

func TestSourcesKeepSmallOrUnprovedInputsSynchronous(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eligible bool
		estimate float64
	}{
		{"small", true, 65535}, {"unproved", false, 1000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := transportRuntime(t, runtime.QueryConfig{})
			o := transportOptions(q)
			o.ParallelEligible, o.EstimatedRows = tc.eligible, tc.estimate
			left, right := transportInts(1, 2), transportInts(3, 4)
			s, err := NewSources(t.Context(), []storage.BatchCursor{left, right}, o)
			if err != nil {
				t.Fatal(err)
			}
			out := columnar.Batch{MaxRows: 1}
			if err := s.Next(t.Context(), 0, &out); err != nil {
				t.Fatal(err)
			}
			if left.calls.Load() != 1 || right.calls.Load() != 0 {
				t.Fatal("synchronous demand scheduled another input")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourcesSingleWorkerDrainsMultipleInputs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, _ := transportRuntime(t, runtime.QueryConfig{})
		o := transportOptions(q)
		o.ParallelEligible, o.EstimatedRows = true, 65536
		s, err := NewSources(t.Context(), []storage.BatchCursor{transportInts(1, 2), transportInts(3, 4), transportInts(5, 6)}, o)
		if err != nil {
			t.Fatal(err)
		}
		for step := range 2 {
			for index := range 3 {
				out := columnar.Batch{MaxRows: 1}
				if err := s.Next(t.Context(), index, &out); err != nil {
					t.Fatal(err)
				}
				if got := transportValue(t, &out, 0); got != int64(index*2+step+1) {
					t.Fatalf("source %d step %d = %d", index, step, got)
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSourcesSplitBorrowedBatchWithoutAdvancingInput(t *testing.T) {
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	root, err := runtime.NewBudget(1<<24, 100)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, root, nil, runtime.QueryConfig{SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close(); _ = pool.Close() })
	o := transportOptions(q)
	o.ParallelEligible, o.EstimatedRows = true, 65536
	input := transportInts(10, 11, 12, 13)
	s, err := NewSources(t.Context(), []storage.BatchCursor{transportInts(1), input}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Next(t.Context(), 0, &columnar.Batch{MaxRows: 4}); err != nil {
		t.Fatal(err)
	}
	out := columnar.Batch{MaxRows: 1}
	for _, want := range []int64{10, 11, 12, 13} {
		if err := s.Next(t.Context(), 1, &out); err != nil {
			t.Fatal(err)
		}
		if out.Rows() != 1 || transportValue(t, &out, 0) != want {
			t.Fatalf("split queue row, want %d", want)
		}
		if input.calls.Load() != 1 {
			t.Fatal("split queue view advanced input")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
