package exec

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type sharedSequenceSource struct {
	layout   model.TypeLayout
	values   []int64
	next     int64
	limit    int64
	terminal error
	closeErr error
	closed   int
}

func (s *sharedSequenceSource) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.limit >= 0 && s.next == s.limit {
		if s.terminal != nil {
			return s.terminal
		}
		return io.EOF
	}
	count := out.MaxRows
	if s.limit >= 0 {
		count = min(count, int(s.limit-s.next))
	}
	s.values = s.values[:0]
	for range count {
		s.next++
		s.values = append(s.values, s.next)
	}
	out.Columns = append(out.Columns, columnar.Vector{Type: s.layout.Type, Length: count, Int64: s.values})
	out.Length = count
	return nil
}

func (s *sharedSequenceSource) Close() error {
	s.closed++
	clear(s.values)
	return s.closeErr
}

func sharedCursorEnvironment(t *testing.T, memory uint64) *Environment {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2, MaxGroups: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(16<<20, 1000)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 8 << 20, OutputBytes: 8 << 20, MaxReservations: 1000, SpillMode: runtime.SpillEnabled, SpillBytes: 8 << 20, Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
		if usage := budget.Usage(); usage.Live != 0 || usage.Reservations != 0 {
			t.Errorf("shared cursor resources remain: %+v", usage)
		}
	})
	return &Environment{Query: query, Limits: Limits{BatchRows: 4, BatchBytes: 64 << 10}, Spool: rowstore.Limits{MemoryBytes: memory, MaxBytes: 1 << 20, MaxRows: 100, MaxRecordBytes: 4096, MetadataBytes: 8192, DecodeBytes: 8192, MaxReaders: 4}}
}

func newSharedSequence(t *testing.T, memory uint64, count int64) (*sharedSequenceSource, *sharedProducer) {
	t.Helper()
	env := sharedCursorEnvironment(t, memory)
	layout := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	source := &sharedSequenceSource{layout: layout, limit: count}
	producer := &sharedProducer{input: source, env: env, layouts: []model.TypeLayout{layout}}
	t.Cleanup(func() {
		err := producer.Close()
		if source.closeErr == nil && err != nil {
			t.Errorf("producer close = %v", err)
		}
		if source.closeErr != nil && !errors.Is(err, source.closeErr) {
			t.Errorf("producer close = %v, want %v", err, source.closeErr)
		}
	})
	return source, producer
}

func newSharedTestCursor(t *testing.T, producer *sharedProducer) *sharedCursor {
	t.Helper()
	cursor := &sharedCursor{producer: producer, env: producer.env, layouts: producer.layouts}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}

func sharedCursorRows(t *testing.T, cursor *sharedCursor, maximum int, want ...int64) columnar.Batch {
	t.Helper()
	out := columnar.Batch{MaxRows: maximum}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Columns[0].Int64; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared rows = %v, want %v", got, want)
	}
	return out
}

func TestSharedCursorAlternatesAcrossGrowingPrefixes(t *testing.T) {
	for _, memory := range []uint64{192, 64 << 10} {
		t.Run(map[bool]string{true: "migration", false: "memory"}[memory == 192], func(t *testing.T) {
			source, producer := newSharedSequence(t, memory, 9)
			first := newSharedTestCursor(t, producer)
			second := newSharedTestCursor(t, producer)
			sharedCursorRows(t, first, 1, 1)
			sharedCursorRows(t, second, 3, 1, 2, 3)
			borrowed := sharedCursorRows(t, first, 1, 2)
			sharedCursorRows(t, second, 2, 4, 5)
			if borrowed.Columns[0].Int64[0] != 2 {
				t.Fatal("another consumer changed published output")
			}
			sharedCursorRows(t, first, 2, 3, 4)
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			sharedCursorRows(t, second, 4, 6, 7, 8, 9)
			var out columnar.Batch
			if err := second.Next(t.Context(), &out); err != io.EOF {
				t.Fatalf("final Next = %v, want EOF", err)
			}
			if source.next != 9 || source.closed != 1 {
				t.Fatalf("source generated %d rows and closed %d times", source.next, source.closed)
			}
			late := newSharedTestCursor(t, producer)
			sharedCursorRows(t, late, 3, 1, 2, 3)
		})
	}
}

func TestSharedCursorEarlyCloseDoesNotEvaluateBeyondDemand(t *testing.T) {
	source, producer := newSharedSequence(t, 64<<10, -1)
	cursor := newSharedTestCursor(t, producer)
	borrowed := sharedCursorRows(t, cursor, 1, 1)
	if source.next != 1 {
		t.Fatalf("one-row demand evaluated %d source rows", source.next)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if source.next != 1 || source.closed != 1 {
		t.Fatalf("early close generated %d rows and closed %d times", source.next, source.closed)
	}
	if borrowed.Columns[0].Int64[0] != 1 {
		t.Fatal("closing source or reader mutated published output")
	}
}

func TestSharedCursorSourceFailureIsShared(t *testing.T) {
	source, producer := newSharedSequence(t, 64<<10, 1)
	source.terminal = errors.New("source failure")
	source.closeErr = errors.New("source close failure")
	first := newSharedTestCursor(t, producer)
	second := newSharedTestCursor(t, producer)
	sharedCursorRows(t, first, 1, 1)
	sharedCursorRows(t, second, 1, 1)
	for _, cursor := range []*sharedCursor{first, second} {
		var out columnar.Batch
		if err := cursor.Next(t.Context(), &out); !errors.Is(err, source.terminal) {
			t.Fatalf("Next = %v, want source failure", err)
		}
		if err := cursor.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := producer.Close(); !errors.Is(err, source.closeErr) {
		t.Fatalf("close lost cleanup failure: %v", err)
	}
	if source.closed != 1 {
		t.Fatalf("source closed %d times, want once", source.closed)
	}
}

func TestSharedCursorCancellationDoesNotEvaluateMoreRows(t *testing.T) {
	source, producer := newSharedSequence(t, 64<<10, -1)
	first := newSharedTestCursor(t, producer)
	second := newSharedTestCursor(t, producer)
	sharedCursorRows(t, first, 1, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, cursor := range []*sharedCursor{first, second} {
		out := columnar.Batch{MaxRows: 1}
		if err := cursor.Next(ctx, &out); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Next = %v", err)
		}
	}
	if source.next != 1 {
		t.Fatalf("canceled demand evaluated %d source rows, want 1", source.next)
	}
}

func TestSharedCursorEOFPropagatesSourceCloseFailure(t *testing.T) {
	source, producer := newSharedSequence(t, 64<<10, 1)
	source.closeErr = errors.New("source close failure")
	cursor := newSharedTestCursor(t, producer)
	sharedCursorRows(t, cursor, 1, 1)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, source.closeErr) {
		t.Fatalf("EOF Next = %v, want close failure", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); !errors.Is(err, source.closeErr) {
		t.Fatalf("producer Close = %v, want close failure", err)
	}
	if source.closed != 1 {
		t.Fatalf("source closed %d times, want once", source.closed)
	}
}
