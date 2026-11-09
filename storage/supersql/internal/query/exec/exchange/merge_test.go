package exchange_test

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

var mergeInt = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var mergeText = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

type mergeSource struct {
	builder  *columnar.Builder
	rows     [][]model.Value
	index    int
	empty    bool
	closed   int
	terminal error
	closeErr error
}

func newMergeSource(t *testing.T, layouts []model.TypeLayout, rows ...[]model.Value) *mergeSource {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 32, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &mergeSource{builder: b, rows: rows, empty: true}
	t.Cleanup(func() { _ = b.Close() })
	return s
}

func (s *mergeSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.builder.Reset()
	if s.empty {
		s.empty = false
		return s.builder.Publish(out)
	}
	if s.index == len(s.rows) {
		if s.terminal != nil {
			return s.terminal
		}
		return io.EOF
	}
	if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
		return err
	}
	s.index++
	s.empty = true
	return s.builder.Publish(out)
}

func (s *mergeSource) Close() error {
	s.closed++
	return errors.Join(s.builder.Close(), s.closeErr)
}

func mergeQuery(t *testing.T, work, output int64) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 2, QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(128<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: work, OutputBytes: output, SpillMode: runtime.SpillDisabled})
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

func mergeOptions(t *testing.T, q *runtime.Query, layouts []model.TypeLayout, keys ...externalsort.Key) exchange.MergeOptions {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return exchange.MergeOptions{Sources: exchange.Options{Query: q, Layouts: layouts, BatchRows: 16, BatchBytes: 32 << 10, QueueBytes: 256 << 10, MetadataBytes: 2 << 20}, Keys: keys, Encoder: encoder, MaxKeyBytes: 4096, WorkBytes: 8 << 20}
}

func openMerge(t *testing.T, inputs []storage.BatchCursor, opts exchange.MergeOptions) storage.BatchCursor {
	t.Helper()
	c, err := exchange.NewOrderedMerge(t.Context(), inputs, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mergeIDs(t *testing.T, c storage.BatchCursor, column, demand int) []int64 {
	t.Helper()
	var ids []int64
	out := columnar.Batch{MaxRows: demand}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return ids
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() > demand {
			t.Fatalf("received %d rows for demand %d", out.Rows(), demand)
		}
		for i := range out.Rows() {
			v, _, err := columnar.CopyValue(t.Context(), &out.Columns[column], out.Lane(i), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, v.IntVal)
		}
	}
}

func mergeReleased(t *testing.T, q *runtime.Query) {
	t.Helper()
	u := q.Usage()
	if u.Work.Live != 0 || u.Output.Live != 0 {
		t.Fatalf("retained work=%d output=%d", u.Work.Live, u.Output.Live)
	}
}

func TestOrderedMergeStableInputTies(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "pool"}[parallel], func(t *testing.T) {
			q := mergeQuery(t, 64<<20, 4<<20)
			layouts := []model.TypeLayout{mergeInt, mergeInt}
			row := func(key, id int64) []model.Value { return []model.Value{model.Int64Val(key), model.Int64Val(id)} }
			a := newMergeSource(t, layouts, row(1, 0), row(1, 1), row(3, 2))
			b := newMergeSource(t, layouts, row(1, 3), row(2, 4), row(3, 5))
			opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0})
			opts.Sources.ParallelEligible, opts.Sources.EstimatedRows = parallel, 65536
			c := openMerge(t, []storage.BatchCursor{a, b}, opts)
			if got, want := mergeIDs(t, c, 1, 1), []int64{0, 1, 3, 4, 2, 5}; !reflect.DeepEqual(got, want) {
				t.Fatalf("ids=%v want=%v", got, want)
			}
			if a.closed != 1 || b.closed != 1 {
				t.Fatalf("close counts=%d,%d", a.closed, b.closed)
			}
			mergeReleased(t, q)
		})
	}
}

func TestOrderedMergeNullDirections(t *testing.T) {
	for _, tc := range []struct {
		name              string
		descending, first bool
		a, b, want        []int64
	}{
		{"ascending-first", false, true, []int64{-9, 1, 3}, []int64{-9, 2}, []int64{0, 3, 1, 4, 2}},
		{"ascending-last", false, false, []int64{1, 3, -9}, []int64{2, -9}, []int64{0, 3, 1, 2, 4}},
		{"descending-first", true, true, []int64{-9, 3, 1}, []int64{-9, 2}, []int64{0, 3, 1, 4, 2}},
		{"descending-last", true, false, []int64{3, 1, -9}, []int64{2, -9}, []int64{0, 3, 1, 2, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := mergeQuery(t, 64<<20, 4<<20)
			layouts := []model.TypeLayout{mergeInt, mergeInt}
			rows := func(values []int64, start int64) [][]model.Value {
				var result [][]model.Value
				for i, n := range values {
					v := model.Int64Val(n)
					if n == -9 {
						v = model.NullVal(model.TypeInt64)
					}
					result = append(result, []model.Value{v, model.Int64Val(start + int64(i))})
				}
				return result
			}
			c := openMerge(t, []storage.BatchCursor{newMergeSource(t, layouts, rows(tc.a, 0)...), newMergeSource(t, layouts, rows(tc.b, 3)...)}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0, Descending: tc.descending, NullsFirst: tc.first}))
			if got := mergeIDs(t, c, 1, 3); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ids=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestOrderedMergeCompositeCollation(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	bpchar := model.TypeLayout{Type: model.Type{ID: 1042, Storage: model.TypeString, Collation: 950}}
	layouts := []model.TypeLayout{bpchar, mergeInt, mergeInt}
	row := func(s string, n, id int64) []model.Value {
		return []model.Value{model.StringVal(s), model.Int64Val(n), model.Int64Val(id)}
	}
	a := newMergeSource(t, layouts, row("A ", 3, 0), row("a", 2, 1), row("b", 1, 2))
	b := newMergeSource(t, layouts, row("A", 3, 3), row("a ", 3, 4), row("a", 2, 5))
	c := openMerge(t, []storage.BatchCursor{a, b}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0, InputCollation: 951}, externalsort.Key{Column: 1, Descending: true}))
	if got, want := mergeIDs(t, c, 2, 2), []int64{0, 3, 4, 1, 5, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v want=%v", got, want)
	}
}

func TestOrderedMergeNumericAndFloatPeers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout model.TypeLayout
		a, b   []model.Value
		want   []int64
	}{
		{"numeric", model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}},
			[]model.Value{{Type: model.TypeNumeric, StrVal: "-0"}, {Type: model.TypeNumeric, StrVal: "1.00"}, {Type: model.TypeNumeric, StrVal: "NaN"}},
			[]model.Value{{Type: model.TypeNumeric, StrVal: "0.00"}, {Type: model.TypeNumeric, StrVal: "1"}, {Type: model.TypeNumeric, StrVal: "NaN"}}, []int64{0, 3, 1, 4, 2, 5}},
		{"float", model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}},
			[]model.Value{model.Float64Val(math.Inf(-1)), model.Float64Val(math.Copysign(0, -1)), model.Float64Val(math.NaN())},
			[]model.Value{model.Float64Val(0), model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN())}, []int64{0, 1, 3, 4, 2, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := mergeQuery(t, 64<<20, 4<<20)
			layouts := []model.TypeLayout{tc.layout, mergeInt}
			rows := func(values []model.Value, start int64) [][]model.Value {
				var result [][]model.Value
				for i, v := range values {
					result = append(result, []model.Value{v, model.Int64Val(start + int64(i))})
				}
				return result
			}
			c := openMerge(t, []storage.BatchCursor{newMergeSource(t, layouts, rows(tc.a, 0)...), newMergeSource(t, layouts, rows(tc.b, 3)...)}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
			if got := mergeIDs(t, c, 1, 1); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ids=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestOrderedMergeEmptyInputs(t *testing.T) {
	for _, count := range []int{0, 3} {
		q := mergeQuery(t, 64<<20, 4<<20)
		layouts := []model.TypeLayout{mergeInt}
		var inputs []storage.BatchCursor
		for range count {
			inputs = append(inputs, newMergeSource(t, layouts))
		}
		c := openMerge(t, inputs, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
		if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != io.EOF {
			t.Fatalf("empty merge: %v", err)
		}
		mergeReleased(t, q)
	}
}

func TestOrderedMergeDemandDefersLateError(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt}
	late := errors.New("late input failure")
	cleanup := errors.New("input cleanup failure")
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(1)})
	s.terminal, s.closeErr = late, cleanup
	c := openMerge(t, []storage.BatchCursor{s}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatalf("one-row demand evaluated later input: %v", err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 1 {
		t.Fatalf("first output=%+v", out)
	}
	if err := c.Next(t.Context(), &out); !errors.Is(err, late) || !errors.Is(err, cleanup) {
		t.Fatalf("lost failure causes: %v", err)
	}
	if err := c.Close(); !errors.Is(err, cleanup) {
		t.Fatalf("lost retained cleanup: %v", err)
	}
	if s.closed != 1 {
		t.Fatalf("close count=%d", s.closed)
	}
	mergeReleased(t, q)
}

func TestOrderedMergeRejectsUnsortedInput(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt}
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(2)}, []model.Value{model.Int64Val(1)})
	c := openMerge(t, []storage.BatchCursor{s}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if err := c.Next(t.Context(), &out); !errors.Is(err, exchange.ErrUnsortedInput) {
		t.Fatalf("unsorted input: %v", err)
	}
	mergeReleased(t, q)
}

func TestOrderedMergePendingByteFullRow(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt, mergeText}
	payload := strings.Repeat("x", 2500)
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(1), model.StringVal(payload)}, []model.Value{model.Int64Val(2), model.StringVal(payload)}, []model.Value{model.Int64Val(3), model.StringVal(payload)})
	opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0})
	opts.Sources.BatchBytes = 4096
	c := openMerge(t, []storage.BatchCursor{s}, opts)
	out := columnar.Batch{MaxRows: 16}
	var ids []int64
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() != 1 {
			t.Fatalf("byte-full output rows=%d", out.Rows())
		}
		v, _, err := columnar.CopyValue(t.Context(), &out.Columns[1], 0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if v.StrVal != payload {
			t.Fatal("pending payload changed")
		}
		ids = append(ids, out.Columns[0].Int64[0])
	}
	if !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Fatalf("pending rows=%v", ids)
	}
	mergeReleased(t, q)
}

func TestOrderedMergeResourceAdmission(t *testing.T) {
	for _, sample := range []struct {
		name         string
		work, output int64
		limit        uint64
	}{
		{"operator_work", 64 << 20, 4 << 20, 1},
		{"query_work", 1024, 4 << 20, 16 << 20},
		{"query_output", 64 << 20, 1, 16 << 20},
	} {
		t.Run(sample.name, func(t *testing.T) {
			q := mergeQuery(t, sample.work, sample.output)
			layouts := []model.TypeLayout{mergeText}
			s := newMergeSource(t, layouts, []model.Value{model.StringVal("key")})
			opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0})
			opts.WorkBytes = sample.limit
			err := mergeFailure(t, s, opts)
			if !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("admission: %v, want budget rejection", err)
			}
			mergeReleased(t, q)
		})
	}
}

func TestOrderedMergeInvalidKeyBounds(t *testing.T) {
	for _, sample := range []struct {
		name   string
		column int
		bytes  uint64
		want   error
	}{
		{"key_bytes", 0, 2, columnar.ErrBatchBudget},
		{"key_column", 1, 4096, storage.ErrInvalidScanPlan},
	} {
		t.Run(sample.name, func(t *testing.T) {
			q := mergeQuery(t, 64<<20, 4<<20)
			layouts := []model.TypeLayout{mergeText}
			s := newMergeSource(t, layouts, []model.Value{model.StringVal("long-key")})
			opts := mergeOptions(t, q, layouts, externalsort.Key{Column: sample.column})
			opts.MaxKeyBytes = sample.bytes
			if err := mergeFailure(t, s, opts); !errors.Is(err, sample.want) {
				t.Fatalf("key: %v, want %v", err, sample.want)
			}
			mergeReleased(t, q)
		})
	}
}

func TestOrderedMergeTextKeyErrors(t *testing.T) {
	for _, sample := range []struct {
		name, value, code string
		collation         model.CollationID
	}{
		{"collation", "key", "42P22", 12345},
		{"encoding", "\xff", "22021", 950},
	} {
		t.Run(sample.name, func(t *testing.T) {
			q := mergeQuery(t, 64<<20, 4<<20)
			layouts := []model.TypeLayout{mergeText}
			s := newMergeSource(t, layouts, []model.Value{model.StringVal(sample.value)})
			opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0, InputCollation: sample.collation})
			err := mergeFailure(t, s, opts)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != sample.code {
				t.Fatalf("text key: %v, want SQLSTATE %s", err, sample.code)
			}
			mergeReleased(t, q)
		})
	}
}

func mergeFailure(t *testing.T, input *mergeSource, options exchange.MergeOptions) error {
	t.Helper()
	cursor, err := exchange.NewOrderedMerge(t.Context(), []storage.BatchCursor{input}, options)
	if err == nil {
		err = cursor.Next(t.Context(), &columnar.Batch{MaxRows: 1})
	}
	if cursor != nil {
		err = errors.Join(err, cursor.Close())
	}
	if input.closed != 1 {
		t.Fatalf("input closed %d times, want once", input.closed)
	}
	return err
}

func TestOrderedMergeSupportDoesNotConsumeInputs(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt}
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(1)})
	opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0})
	if err := exchange.CheckMergeSupport(t.Context(), 1, opts); err != nil {
		t.Fatal(err)
	}
	mergeReleased(t, q)
	c := openMerge(t, []storage.BatchCursor{s}, opts)
	if s.index != 0 {
		t.Fatal("constructor consumed input")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if s.index != 0 || s.closed != 1 {
		t.Fatalf("early close index=%d close count=%d", s.index, s.closed)
	}
	mergeReleased(t, q)
}

type mergeBlockedSource struct {
	started    chan struct{}
	active     atomic.Int32
	closed     atomic.Int32
	overlapped atomic.Bool
}

func (s *mergeBlockedSource) Next(ctx context.Context, _ *columnar.Batch) error {
	s.active.Add(1)
	defer s.active.Add(-1)
	s.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}
func (s *mergeBlockedSource) Close() error {
	if s.active.Load() != 0 {
		s.overlapped.Store(true)
	}
	s.closed.Add(1)
	return nil
}

func TestOrderedMergeCancellationJoinsPulls(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	opts := mergeOptions(t, q, []model.TypeLayout{mergeInt}, externalsort.Key{Column: 0})
	opts.Sources.ParallelEligible, opts.Sources.EstimatedRows = true, 65536
	a := &mergeBlockedSource{started: make(chan struct{}, 2)}
	b := &mergeBlockedSource{started: make(chan struct{}, 2)}
	c := openMerge(t, []storage.BatchCursor{a, b}, opts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Next(ctx, &columnar.Batch{MaxRows: 1}) }()
	select {
	case <-a.started:
	case <-t.Context().Done():
		t.Fatal("pull did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	for _, s := range []*mergeBlockedSource{a, b} {
		if s.active.Load() != 0 || s.closed.Load() != 1 || s.overlapped.Load() {
			t.Fatalf("unjoined source: active=%d closed=%d overlap=%v", s.active.Load(), s.closed.Load(), s.overlapped.Load())
		}
	}
	if q.Context().Err() != nil {
		t.Fatalf("exchange canceled borrowed query: %v", q.Context().Err())
	}
	mergeReleased(t, q)
}

func TestOrderedMergeEarlyCloseDoesNotEvaluateNextKey(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeText}
	s := newMergeSource(t, layouts, []model.Value{model.StringVal("first")}, []model.Value{model.StringVal(string([]byte{0xff}))})
	c := openMerge(t, []storage.BatchCursor{s}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
	if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("early close evaluated invalid later key: %v", err)
	}
	if s.index != 1 || s.closed != 1 {
		t.Fatalf("index=%d close count=%d", s.index, s.closed)
	}
	mergeReleased(t, q)
}

func TestOrderedMergeOwnsOrderingMetadata(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt}
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(1)}, []model.Value{model.Int64Val(2)})
	opts := mergeOptions(t, q, layouts, externalsort.Key{Column: 0})
	c := openMerge(t, []storage.BatchCursor{s}, opts)
	opts.Keys[0] = externalsort.Key{Column: 999, Descending: true}
	layouts[0].Type = model.Type{ID: 25, Storage: model.TypeString}
	if got, want := mergeIDs(t, c, 0, 1), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v want=%v", got, want)
	}
	mergeReleased(t, q)
}

func TestOrderedMergeConstructorCancellationClosesInputs(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	layouts := []model.TypeLayout{mergeInt}
	s := newMergeSource(t, layouts, []model.Value{model.Int64Val(1)})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := exchange.NewOrderedMerge(ctx, []storage.BatchCursor{s}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("constructor error=%v", err)
	}
	if s.index != 0 || s.closed != 1 {
		t.Fatalf("index=%d close count=%d", s.index, s.closed)
	}
	mergeReleased(t, q)
}

type mergeNativeSource struct {
	batch  columnar.Batch
	key    []int64
	data   []byte
	done   bool
	closed int
}

func (s *mergeNativeSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.done {
		s.key[1] = -99
		clear(s.data)
		return io.EOF
	}
	s.done = true
	out.Reset()
	out.Columns = append(out.Columns, s.batch.Columns...)
	out.Length = s.batch.Length
	out.Selection = s.batch.Selection
	return nil
}
func (s *mergeNativeSource) Close() error { s.closed++; clear(s.data); return nil }

func TestOrderedMergeSelectedNativeNestedPayload(t *testing.T) {
	q := mergeQuery(t, 64<<20, 4<<20)
	composite := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeComposite}, Children: []model.TypeLayout{mergeText}}
	layouts := []model.TypeLayout{mergeInt, composite}
	makeSource := func(key int64, payload string) *mergeNativeSource {
		s := &mergeNativeSource{key: []int64{-999, key}, data: []byte(payload)}
		s.batch = columnar.Batch{Length: 3, Selection: []uint32{2}, Columns: []columnar.Vector{
			{Type: mergeInt.Type, Encoding: columnar.Dictionary, Length: 3, Indices: []uint32{0, 0, 1}, Int64: s.key},
			{Type: composite.Type, Encoding: columnar.Constant, Length: 3, Children: []columnar.Vector{{Type: mergeText.Type, Encoding: columnar.Flat, Length: 1, Data: s.data, Ranges: []columnar.Range{{Length: uint32(len(s.data))}}}}},
		}}
		return s
	}
	a, b := makeSource(1, "first"), makeSource(2, "second")
	c := openMerge(t, []storage.BatchCursor{a, b}, mergeOptions(t, q, layouts, externalsort.Key{Column: 0}))
	out := columnar.Batch{MaxRows: 2}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 2 {
		t.Fatalf("rows=%d", out.Rows())
	}
	for i, want := range []string{"first", "second"} {
		v, _, err := columnar.CopyValue(t.Context(), &out.Columns[1], i, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.ArrayVal) != 1 || v.ArrayVal[0].StrVal != want {
			t.Fatalf("nested row %d=%+v want=%s", i, v, want)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if a.closed != 1 || b.closed != 1 {
		t.Fatalf("close counts=%d,%d", a.closed, b.closed)
	}
	mergeReleased(t, q)
}
