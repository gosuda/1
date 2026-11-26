package rowsfrom_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowsfrom"
)

var (
	integer = model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	bigint  = model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	text    = model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}
	bytea   = model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
)

func query(t *testing.T, ctx context.Context) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(4<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(ctx, pool, budget, nil, runtime.QueryConfig{WorkBytes: 2 << 20, OutputBytes: 1 << 20, MaxReservations: 128, SpillMode: runtime.SpillDisabled})
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

type source struct {
	layouts  []model.TypeLayout
	rows     [][]model.Value
	builder  *columnar.Builder
	index    int
	started  bool
	onFirst  func(context.Context) error
	pull     func(context.Context, *columnar.Batch) error
	nexts    int
	closes   int
	closeErr error
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows ...[]model.Value) *source {
	t.Helper()
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 128, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{layouts: layouts, rows: rows, builder: builder}
	t.Cleanup(func() { _ = builder.Close() })
	return s
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.nexts++
	if !s.started {
		s.started = true
		if s.onFirst != nil {
			if err := s.onFirst(ctx); err != nil {
				return err
			}
		}
	}
	if s.pull != nil {
		return s.pull(ctx, out)
	}
	if s.index == len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	limit := 128
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for s.index < len(s.rows) && s.builder.Rows() < limit {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	return s.builder.Publish(out)
}

func (s *source) Close() error {
	s.closes++
	s.rows = nil
	if s.builder != nil {
		return errors.Join(s.closeErr, s.builder.Close())
	}
	return s.closeErr
}

func input(s *source) rowsfrom.Input {
	return rowsfrom.Input{Cursor: s, Layouts: s.layouts}
}

func options(q *runtime.Query, output ...model.TypeLayout) rowsfrom.Options {
	return rowsfrom.Options{Output: output, Query: q, BatchRows: 32, BatchBytes: 16 << 10}
}

func open(t *testing.T, ctx context.Context, inputs []rowsfrom.Input, opts rowsfrom.Options) storage.BatchCursor {
	t.Helper()
	cursor, err := rowsfrom.New(ctx, inputs, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cursor.Close() })
	return cursor
}

func rows(t *testing.T, batch *columnar.Batch) [][]model.Value {
	t.Helper()
	if err := batch.Validate(max(1, batch.Length)); err != nil {
		t.Fatal(err)
	}
	result := make([][]model.Value, batch.Rows())
	for i := range result {
		result[i] = make([]model.Value, len(batch.Columns))
		for j := range batch.Columns {
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[j], batch.Lane(i), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			result[i][j] = value
		}
	}
	return result
}

func requireRows(t *testing.T, out *columnar.Batch, want [][]model.Value) {
	t.Helper()
	if got := rows(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
}

func released(t *testing.T, q *runtime.Query) {
	t.Helper()
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("cursor retained query resources: %+v", usage)
	}
}

func closedOnce(t *testing.T, sources ...*source) {
	t.Helper()
	for i, source := range sources {
		if source.closes != 1 {
			t.Errorf("input %d closed %d times, want once", i, source.closes)
		}
	}
}

func TestUnevenInputsPadEveryColumnWithTypedNulls(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer, text}, []model.Value{model.Int64Val(-7), model.NullVal(model.TypeString)})
	empty := newSource(t, []model.TypeLayout{bytea})
	right := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(40)}, []model.Value{model.NullVal(model.TypeInt64)}, []model.Value{model.Int64Val(42)})
	opts := options(q, integer, text, bytea, integer, bigint)
	opts.Ordinality = true
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(empty), input(right)}, opts)
	out := columnar.Batch{MaxRows: 1}
	want := [][]model.Value{
		{model.Int64Val(-7), model.NullVal(model.TypeString), model.NullVal(model.TypeBytes), model.Int64Val(40), model.Int64Val(1)},
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.NullVal(model.TypeBytes), model.NullVal(model.TypeInt64), model.Int64Val(2)},
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.NullVal(model.TypeBytes), model.Int64Val(42), model.Int64Val(3)},
	}
	for _, row := range want {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		requireRows(t, &out, [][]model.Value{row})
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF || out.Rows() != 0 {
		t.Fatalf("completion=%v rows=%d", err, out.Rows())
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("repeated completion=%v", err)
	}
	closedOnce(t, left, empty, right)
	released(t, q)
}

func TestZeroColumnSourceDeterminesLongestInput(t *testing.T) {
	q := query(t, t.Context())
	zero := newSource(t, nil, nil, nil, nil)
	short := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(9)})
	opts := options(q, integer, bigint)
	opts.Ordinality = true
	cursor := open(t, t.Context(), []rowsfrom.Input{input(zero), input(short)}, opts)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{
		{model.Int64Val(9), model.Int64Val(1)},
		{model.NullVal(model.TypeInt64), model.Int64Val(2)},
		{model.NullVal(model.TypeInt64), model.Int64Val(3)},
	})
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatal(err)
	}
	closedOnce(t, zero, short)
	released(t, q)
}

func TestOnlyZeroColumnSourcesStillEmitRows(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil, nil), newSource(t, nil, nil, nil)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{}, {}})
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatal(err)
	}
	released(t, q)
}

func TestAllEmptyInputsEvaluateBeforeEOF(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, []model.TypeLayout{integer}), newSource(t, nil)
	var order []string
	left.onFirst = func(context.Context) error { order = append(order, "left"); return nil }
	right.onFirst = func(context.Context) error { order = append(order, "right"); return nil }
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer))
	if len(order) != 0 {
		t.Fatalf("construction evaluated inputs: %v", order)
	}
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != io.EOF || out.Rows() != 0 {
		t.Fatalf("empty inputs=%v rows=%d", err, out.Rows())
	}
	if !slices.Equal(order, []string{"left", "right"}) {
		t.Fatalf("first-demand order=%v", order)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestSparseLanesUsePhysicalLengthNotDemand(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer})
	physical := []int64{1, 2, 3, 4, 5, 60}
	left.pull = func(_ context.Context, out *columnar.Batch) error {
		if left.nexts > 2 {
			return io.EOF
		}
		selection := []uint32{}
		if left.nexts == 2 {
			selection = []uint32{5}
		}
		out.Columns = append(out.Columns[:0], columnar.Vector{Type: integer.Type, Length: len(physical), Int64: physical})
		out.Length, out.Selection = len(physical), selection
		return nil
	}
	right := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(7)})
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer, integer))
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.Int64Val(60), model.Int64Val(7)}})
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatal(err)
	}
	released(t, q)
}

func TestByteBackpressureRetainsPendingRowAndOrdinality(t *testing.T) {
	q := query(t, t.Context())
	values := make([][]model.Value, 40)
	for i := range values {
		values[i] = []model.Value{model.StringVal(strings.Repeat(string(rune('a'+i%26)), 128+i))}
	}
	left := newSource(t, []model.TypeLayout{text}, values...)
	right := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(-100)})
	opts := options(q, text, integer, bigint)
	opts.Ordinality, opts.BatchRows, opts.BatchBytes = true, 40, 3072
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, opts)
	var out columnar.Batch
	next, batches := 0, 0
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		batches++
		if out.Rows() == 0 || next+out.Rows() > len(values) {
			t.Fatalf("invalid progress next=%d rows=%d", next, out.Rows())
		}
		if live := q.Usage().Output.Live; live > int64(opts.BatchBytes) {
			t.Fatalf("output bytes=%d exceed %d", live, opts.BatchBytes)
		}
		for _, row := range rows(t, &out) {
			if row[0].StrVal != values[next][0].StrVal || row[2].IntVal != int64(next+1) {
				t.Fatalf("row %d=%v", next, row)
			}
			if row[1].IsNull != (next != 0) {
				t.Fatalf("row %d padding=%v", next, row[1])
			}
			next++
		}
	}
	if next != 40 || batches < 2 {
		t.Fatalf("returned %d rows in %d batches", next, batches)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestRetainedNestedPayloadSurvivesEveryInputAndOutputClose(t *testing.T) {
	q := query(t, t.Context())
	array := model.TypeLayout{Type: model.Type{ID: 1001, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{bytea}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, array}}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{
		model.StringVal("keep"),
		{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -7, Length: 2}}, ArrayVal: []model.Value{model.BytesVal([]byte{0, 255}), model.NullVal(model.TypeBytes)}},
	}}
	left := newSource(t, []model.TypeLayout{record}, []model.Value{value}, []model.Value{model.NullVal(model.TypeComposite)})
	right := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(9)})
	opts := options(q, record, integer)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	retained, err := expr.Retain(t.Context(), &out, opts.Output, expr.RetainOptions{MaxRows: 1, MaxBytes: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retained.Close() })
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.NullVal(model.TypeComposite), model.NullVal(model.TypeInt64)}})
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	closedOnce(t, left, right)
	released(t, q)
	if err := retained.Publish(&out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{value, model.Int64Val(9)}})
}

func TestSuccessfulNextContextIsNotRetained(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(1)}, []model.Value{model.Int64Val(2)})
	right := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(10)}, []model.Value{model.Int64Val(20)})
	var order []string
	left.onFirst = func(context.Context) error { order = append(order, "left"); return nil }
	right.onFirst = func(context.Context) error { order = append(order, "right"); return nil }
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer, integer))
	first, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(first, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"left", "right"}) {
		t.Fatalf("first-demand order=%v", order)
	}
	cancel()
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.Int64Val(2), model.Int64Val(20)}})
	if !slices.Equal(order, []string{"left", "right"}) {
		t.Fatalf("arguments evaluated again: %v", order)
	}
}

func TestCancellationInterruptsPullAndPreservesCleanupCauses(t *testing.T) {
	for owner, name := range []string{"next", "lifetime", "query"} {
		t.Run(name, func(t *testing.T) {
			next, cancelNext := context.WithCancelCause(t.Context())
			defer cancelNext(nil)
			life, cancelLife := context.WithCancelCause(t.Context())
			defer cancelLife(nil)
			queryContext, cancelQuery := context.WithCancelCause(t.Context())
			defer cancelQuery(nil)
			q := query(t, queryContext)
			canceled, pullErr, closeErr := errors.New("canceled owner"), errors.New("pull failed"), errors.New("close failed")
			cancel := []context.CancelCauseFunc{cancelNext, cancelLife, cancelQuery}[owner]
			left := newSource(t, []model.TypeLayout{integer})
			left.onFirst = func(ctx context.Context) error {
				cancel(canceled)
				<-ctx.Done()
				return pullErr
			}
			left.closeErr = closeErr
			right := newSource(t, nil)
			cursor := open(t, life, []rowsfrom.Input{input(left), input(right)}, options(q, integer))
			bounded, stop := context.WithTimeout(next, 5*time.Second)
			defer stop()
			var out columnar.Batch
			if err := cursor.Next(bounded, &out); !errors.Is(err, canceled) || !errors.Is(err, pullErr) || !errors.Is(err, closeErr) || out.Rows() != 0 {
				t.Fatalf("failed demand=%v rows=%d", err, out.Rows())
			}
			if right.nexts != 0 {
				t.Fatalf("evaluated later input after failure %d times", right.nexts)
			}
			closedOnce(t, left, right)
			released(t, q)
		})
	}
}

func TestEarlyCloseNeverPullsAndJoinsEveryCleanupError(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	left.closeErr, right.closeErr = errors.New("left close"), errors.New("right close")
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q))
	for range 2 {
		if err := cursor.Close(); !errors.Is(err, left.closeErr) || !errors.Is(err, right.closeErr) {
			t.Fatalf("close=%v", err)
		}
	}
	if left.nexts != 0 || right.nexts != 0 {
		t.Fatalf("early close pulled inputs %d,%d", left.nexts, right.nexts)
	}
	closedOnce(t, left, right)
	released(t, q)
	claim, err := q.TryReserve(runtime.WorkMemory, 1)
	if err != nil {
		t.Fatalf("cursor closed borrowed query: %v", err)
	}
	claim.Release()
}

func TestInputFailureDiscardsUnpublishedRows(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(1)}, []model.Value{model.Int64Val(2)})
	left.closeErr = errors.New("left cleanup")
	right := newSource(t, nil)
	pullErr := errors.New("later input failed")
	right.pull = func(_ context.Context, out *columnar.Batch) error {
		if right.nexts == 1 {
			out.Length = 1
			return nil
		}
		return pullErr
	}
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, pullErr) || !errors.Is(err, left.closeErr) || out.Rows() != 0 {
		t.Fatalf("failed input=%v rows=%d", err, out.Rows())
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestEOFCleanupFailureIsNotSuccessfulCompletion(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	left.closeErr, right.closeErr = errors.New("left EOF cleanup"), errors.New("right cleanup")
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err == io.EOF || !errors.Is(err, left.closeErr) || !errors.Is(err, right.closeErr) {
		t.Fatalf("EOF cleanup=%v", err)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestNilInputFailureClosesInputsAfterNil(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	left.closeErr, right.closeErr = errors.New("left close"), errors.New("right close")
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), {}, input(right)}, options(q))
	if cursor != nil || !errors.Is(err, runtime.ErrInvalidConfig) || !errors.Is(err, left.closeErr) || !errors.Is(err, right.closeErr) {
		t.Fatalf("nil input=%v cursor=%v", err, cursor)
	}
	if left.nexts != 0 || right.nexts != 0 {
		t.Fatal("constructor pulled an input")
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestFullLayoutMismatchFailsBeforePulling(t *testing.T) {
	q := query(t, t.Context())
	nested := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	wrongChild := text
	wrongChild.Type.Collation = 999
	wrongOutput := model.TypeLayout{Type: nested.Type, Children: []model.TypeLayout{wrongChild}}
	left, right := newSource(t, []model.TypeLayout{nested}), newSource(t, nil)
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, wrongOutput))
	if cursor != nil || !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("nested identity mismatch=%v cursor=%v", err, cursor)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestOrdinalityRequiresCanonicalBigint(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	wrong := bigint
	wrong.Type.Modifier = 0
	opts := options(q, wrong)
	opts.Ordinality = true
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), input(right)}, opts)
	if cursor != nil || !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("noncanonical ordinality=%v cursor=%v", err, cursor)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestOutputMetadataFailureClosesAdmittedInputs(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, []model.TypeLayout{integer}), newSource(t, nil)
	opts := options(q, integer)
	opts.BatchBytes = 1
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), input(right)}, opts)
	if cursor != nil || !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("builder admission=%v cursor=%v", err, cursor)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestWorkAdmissionFailureClosesWithoutPulling(t *testing.T) {
	q := query(t, t.Context())
	blocker, err := q.TryReserve(runtime.WorkMemory, q.Usage().Work.Limit)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	left, right := newSource(t, nil), newSource(t, nil)
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q))
	if cursor != nil || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("work admission=%v cursor=%v", err, cursor)
	}
	if left.nexts != 0 || right.nexts != 0 {
		t.Fatal("admission failure pulled inputs")
	}
	closedOnce(t, left, right)
	blocker.Release()
	released(t, q)
}

func TestOutputAllocationFailureReleasesInputs(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(1)})
	right := newSource(t, nil, nil)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer))
	usage := q.Usage().Output
	blocker, err := q.TryReserve(runtime.OutputMemory, usage.Limit-usage.Live)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) || out.Rows() != 0 {
		t.Fatalf("output allocation=%v rows=%d", err, out.Rows())
	}
	closedOnce(t, left, right)
	blocker.Release()
	released(t, q)
}

func TestMalformedSelectedLaneClosesAllInputs(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, nil)
	left.pull = func(_ context.Context, out *columnar.Batch) error {
		out.Length, out.Selection = 1, []uint32{1}
		return nil
	}
	right := newSource(t, nil)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrInvalidBatch) || out.Rows() != 0 {
		t.Fatalf("malformed selection=%v rows=%d", err, out.Rows())
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestMissingQueryStillClosesTransferredInputs(t *testing.T) {
	left, right := newSource(t, nil), newSource(t, nil)
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{input(left), input(right)}, options(nil))
	if cursor != nil || !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("missing query=%v cursor=%v", err, cursor)
	}
	closedOnce(t, left, right)
}

func TestCanceledConstructionClosesWithoutPulling(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	failure := errors.New("construction canceled")
	cancel(failure)
	cursor, err := rowsfrom.New(ctx, []rowsfrom.Input{input(left), input(right)}, options(q))
	if cursor != nil || !errors.Is(err, failure) {
		t.Fatalf("canceled construction=%v cursor=%v", err, cursor)
	}
	if left.nexts != 0 || right.nexts != 0 {
		t.Fatal("canceled construction pulled inputs")
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestCyclicDescriptorIsRejectedBeforeAdmission(t *testing.T) {
	q := query(t, t.Context())
	left, right := newSource(t, nil), newSource(t, nil)
	cycle := make([]model.TypeLayout, 1)
	cycle[0] = model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: cycle}
	cursor, err := rowsfrom.New(t.Context(), []rowsfrom.Input{{Cursor: left, Layouts: cycle}, input(right)}, options(q, cycle[0]))
	if cursor != nil || !errors.Is(err, model.ErrInvalidTypeLayout) {
		t.Fatalf("cyclic descriptor=%v cursor=%v", err, cursor)
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestInputBatchCannotChangeSQLTypeIdentity(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer})
	left.pull = func(_ context.Context, out *columnar.Batch) error {
		out.Columns = append(out.Columns[:0], columnar.Vector{Type: bigint.Type, Length: 1, Int64: []int64{9}})
		out.Length = 1
		return nil
	}
	right := newSource(t, nil)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrValueLayout) || out.Rows() != 0 {
		t.Fatalf("changed input type=%v rows=%d", err, out.Rows())
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestOversizedPendingRowReleasesEveryResource(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{text}, []model.Value{model.StringVal(strings.Repeat("x", 4096))})
	right := newSource(t, nil, nil)
	opts := options(q, text)
	opts.BatchBytes = 1024
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, opts)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrBatchBudget) || out.Rows() != 0 {
		t.Fatalf("oversized row=%v rows=%d", err, out.Rows())
	}
	closedOnce(t, left, right)
	released(t, q)
}

func TestInvalidDemandClearsBorrowedOutputAndClosesInputs(t *testing.T) {
	q := query(t, t.Context())
	left := newSource(t, []model.TypeLayout{integer}, []model.Value{model.Int64Val(1)}, []model.Value{model.Int64Val(2)})
	right := newSource(t, nil, nil, nil)
	cursor := open(t, t.Context(), []rowsfrom.Input{input(left), input(right)}, options(q, integer))
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	out.MaxRows = -1
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrInvalidBatch) || out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatalf("invalid demand=%v rows=%d columns=%d", err, out.Rows(), len(out.Columns))
	}
	closedOnce(t, left, right)
	released(t, q)
}
