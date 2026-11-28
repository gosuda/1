package apply_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var integer = model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
var boolean = model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}

type factory func(context.Context, []model.Value) (storage.BatchCursor, error)

func (f factory) Open(ctx context.Context, row []model.Value) (storage.BatchCursor, error) {
	return f(ctx, row)
}

type source struct {
	owner              *columnar.Builder
	values             []model.Value
	position           int
	life               context.Context
	emptyFirst, closed bool
	terminal           error
}

func newSource(life context.Context, t *testing.T, q *runtime.Query, values ...model.Value) *source {
	t.Helper()
	owner, err := columnar.NewBuilder([]model.TypeLayout{integer}, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 4096, Allocator: q.Allocator(runtime.WorkMemory)})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{owner: owner, values: values, life: life}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if err := errors.Join(context.Cause(ctx), context.Cause(s.life)); err != nil {
		return err
	}
	if s.closed {
		return runtime.ErrClosed
	}
	if s.emptyFirst {
		s.emptyFirst = false
		return nil
	}
	if s.position == len(s.values) {
		if s.terminal != nil {
			return s.terminal
		}
		return io.EOF
	}
	s.owner.Reset()
	count := min(2, len(s.values)-s.position)
	if out.MaxRows > 0 {
		count = min(count, out.MaxRows)
	}
	for i := 0; i < count; i++ {
		if err := s.owner.Append(ctx, s.values[s.position:s.position+1]); err != nil {
			return err
		}
		s.position++
	}
	return s.owner.Publish(out)
}
func (s *source) Close() error { s.closed = true; s.values = nil; return s.owner.Close() }

func query(t *testing.T) *runtime.Query {
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
	budget, err := runtime.NewBudget(8<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, nil, runtime.QueryConfig{WorkBytes: 4 << 20, OutputBytes: 1 << 20, MaxReservations: 128, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	return q
}
func options(q *runtime.Query, kind ir.ApplyKind, f factory) apply.Options {
	return apply.Options{Kind: kind, LeftLayouts: []model.TypeLayout{integer}, RightLayouts: []model.TypeLayout{integer}, OutputLayouts: []model.TypeLayout{integer, integer}, Output: []apply.Column{{Side: apply.Left}, {Side: apply.Right}}, Factory: f, Query: q, BatchRows: 2, BatchBytes: 4096, MaxRowBytes: 4096, WorkBytes: 64 << 10}
}
func open(t *testing.T, left storage.BatchCursor, options apply.Options) storage.BatchCursor {
	t.Helper()
	cursor, err := apply.New(t.Context(), left, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}
func drain(t *testing.T, cursor storage.BatchCursor) [][]model.Value {
	t.Helper()
	var rows [][]model.Value
	batch := columnar.Batch{MaxRows: 1}
	for {
		err := cursor.Next(t.Context(), &batch)
		if err == io.EOF {
			return rows
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range batch.Rows() {
			values := make([]model.Value, len(batch.Columns))
			for i := range values {
				value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[i], batch.Lane(row), 4096)
				if err != nil {
					t.Fatal(err)
				}
				values[i] = value
			}
			rows = append(rows, values)
		}
	}
}
func released(t *testing.T, q *runtime.Query) {
	t.Helper()
	usage := q.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("Apply retained memory: %+v", usage)
	}
}

func TestScalarApplyDistinguishesEmptyFromSingleRow(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(0), model.Int64Val(1))
	f := factory(func(ctx context.Context, row []model.Value) (storage.BatchCursor, error) {
		var values []model.Value
		if row[0].IntVal != 0 {
			values = []model.Value{model.Int64Val(row[0].IntVal + 10)}
		}
		right := newSource(ctx, t, q, values...)
		right.emptyFirst = true
		return right, nil
	})
	cursor := open(t, left, options(q, ir.ScalarApply, f))
	want := [][]model.Value{{model.Int64Val(0), model.NullVal(model.TypeInt64)}, {model.Int64Val(1), model.Int64Val(11)}}
	if got := drain(t, cursor); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
	released(t, q)
}

func TestScalarApplyRejectsSecondRowBeforePublishing(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1))
	var right *source
	f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
		right = newSource(ctx, t, q, model.Int64Val(10), model.Int64Val(11))
		return right, nil
	})
	cursor := open(t, left, options(q, ir.ScalarApply, f))
	var batch columnar.Batch
	err := cursor.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "21000" {
		t.Fatalf("scalar cardinality error=%v", err)
	}
	if batch.Rows() != 0 || !left.closed || right == nil || !right.closed {
		t.Fatal("scalar violation published a row or retained input")
	}
	released(t, q)
}

func TestOuterLateralKeepsCaptureAliveAcrossNextCalls(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(0), model.Int64Val(2))
	f := factory(func(ctx context.Context, row []model.Value) (storage.BatchCursor, error) {
		if row[0].IntVal == 0 {
			return newSource(ctx, t, q), nil
		}
		return newSource(ctx, t, q, model.Int64Val(row[0].IntVal*10), model.Int64Val(row[0].IntVal*10+1)), nil
	})
	o := options(q, ir.LateralApply, f)
	o.Outer = true
	cursor := open(t, left, o)
	want := [][]model.Value{{model.Int64Val(0), model.NullVal(model.TypeInt64)}, {model.Int64Val(2), model.Int64Val(20)}, {model.Int64Val(2), model.Int64Val(21)}}
	if got := drain(t, cursor); !reflect.DeepEqual(got, want) {
		t.Fatalf("lateral rows=%v want=%v", got, want)
	}
	released(t, q)
}

func TestExistsApplyStopsAfterFirstRow(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1))
	f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
		right := newSource(ctx, t, q, model.Int64Val(10))
		right.terminal = errors.New("read beyond existence witness")
		return right, nil
	})
	o := options(q, ir.ExistsApply, f)
	o.OutputLayouts[1], o.Output[1] = boolean, apply.Column{Side: apply.Mark}
	cursor := open(t, left, o)
	want := [][]model.Value{{model.Int64Val(1), model.BoolVal(true)}}
	if got := drain(t, cursor); !reflect.DeepEqual(got, want) {
		t.Fatalf("exists rows=%v want=%v", got, want)
	}
	released(t, q)
}

func TestApplyFactoryFailureClosesBothInputs(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1))
	failure := errors.New("right preparation failed")
	var right *source
	f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
		right = newSource(ctx, t, q)
		return right, failure
	})
	cursor := open(t, left, options(q, ir.ScalarApply, f))
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); !errors.Is(err, failure) {
		t.Fatalf("factory error=%v", err)
	}
	if right == nil || !right.closed || !left.closed {
		t.Fatal("factory error retained cursor ownership")
	}
	released(t, q)
}

func TestApplyEarlyCloseReleasesActiveInner(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1), model.Int64Val(2))
	var right *source
	f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
		right = newSource(ctx, t, q, model.Int64Val(10), model.Int64Val(11))
		return right, nil
	})
	cursor := open(t, left, options(q, ir.LateralApply, f))
	batch := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || batch.Columns[1].Int64[0] != 10 {
		t.Fatalf("first lateral batch=%+v", batch)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if right == nil || !right.closed || !left.closed {
		t.Fatal("early Close retained input cursors")
	}
	released(t, q)
}

func TestApplyCancellationInterruptsRightFactory(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1))
	entered := make(chan struct{})
	f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	cursor := open(t, left, options(q, ir.ScalarApply, f))
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var result error
	go func() {
		defer close(finished)
		var batch columnar.Batch
		result = cursor.Next(ctx, &batch)
	}()
	defer func() { cancel(); <-finished }()
	<-entered
	cancel()
	<-finished
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("canceled Apply=%v", result)
	}
	if !left.closed {
		t.Fatal("canceled Apply retained outer cursor")
	}
	released(t, q)
}

func TestApplyInvalidOutputLayoutReleasesAdmission(t *testing.T) {
	q := query(t)
	left := newSource(t.Context(), t, q, model.Int64Val(1))
	f := factory(func(context.Context, []model.Value) (storage.BatchCursor, error) {
		t.Fatal("invalid Apply evaluated its factory")
		return nil, nil
	})
	o := options(q, ir.ScalarApply, f)
	o.OutputLayouts[1] = boolean
	cursor, err := apply.New(t.Context(), left, o)
	if cursor != nil || !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("invalid layout cursor=%v error=%v", cursor, err)
	}
	if !left.closed {
		t.Fatal("failed constructor retained outer cursor")
	}
	released(t, q)
}

func TestConsumeRowsClosesChildOnEveryTermination(t *testing.T) {
	failure := errors.New("row visitor failed")
	for _, termination := range []string{"stop", "visitor error", "source error", "factory error", "cancel"} {
		t.Run(termination, func(t *testing.T) {
			q := query(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var child *source
			f := factory(func(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
				child = newSource(ctx, t, q, model.Int64Val(7))
				if termination == "factory error" {
					return child, failure
				}
				child.terminal = failure
				return child, nil
			})
			o := apply.RowsOptions{Layouts: []model.TypeLayout{integer}, Query: q, MaxRowBytes: 4096, WorkBytes: 64 << 10}
			err := apply.ConsumeRows(ctx, f, nil, o, func(row []model.Value) (bool, error) {
				if len(row) != 1 || row[0].Type != model.TypeInt64 || row[0].IsNull || row[0].IntVal != 7 {
					t.Fatalf("row=%v want 7", row)
				}
				switch termination {
				case "visitor error":
					return false, failure
				case "cancel":
					cancel()
					return true, nil
				default:
					return termination == "source error", nil
				}
			})
			want := failure
			if termination == "stop" {
				want = nil
			} else if termination == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want %v", err, want)
			}
			if child == nil || !child.closed {
				t.Fatal("row stream retained child")
			}
			released(t, q)
		})
	}
}
