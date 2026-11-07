package window_test

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var int8 = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var int4 = model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
var float8 = model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}
var text = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}
var numeric = model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}

func iv(n int64) model.Value   { return model.Int64Val(n) }
func fv(n float64) model.Value { return model.Float64Val(n) }
func null() model.Value        { return model.NullVal(model.TypeInt64) }

func defaultFrame() ir.Frame {
	return ir.Frame{Mode: ir.RangeFrame, Start: ir.FrameBound{Kind: ir.UnboundedPreceding}, End: ir.FrameBound{Kind: ir.CurrentRow}}
}

type source struct {
	builder      *columnar.Builder
	rows         [][]model.Value
	index, pulls int
	closed       bool
	closeErr     error
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: b, rows: rows}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.pulls++
	if s.index == len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	limit := 2
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for range min(limit, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	return s.builder.Publish(out)
}

func (s *source) Close() error { s.closed = true; return errors.Join(s.closeErr, s.builder.Close()) }

func query(t *testing.T, disabled bool) (*runtime.Query, string) {
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
	budget, err := runtime.NewBudget(128<<20, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.QueryConfig{WorkBytes: 64 << 20, OutputBytes: 8 << 20, SpillBytes: 64 << 20, Directory: dir, MaxFiles: 3, MaxReaders: 3}
	if disabled {
		config.SpillMode = runtime.SpillDisabled
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	return q, dir
}

func options(t *testing.T, q *runtime.Query, layouts, results []model.TypeLayout, calls []window.Call) window.Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return window.Options{InputLayouts: layouts, OutputLayouts: results, Calls: calls, Frame: defaultFrame(), Encoder: encoder, Query: q, BatchRows: 2, BatchBytes: 1 << 16, MaxRecordBytes: 1024, PartitionMemoryBytes: 1 << 20, MaxPartitionBytes: 16 << 20, MaxPartitionRows: 100000, WorkBytes: 32 << 20}
}

func open(t *testing.T, input storage.BatchCursor, o window.Options) storage.BatchCursor {
	t.Helper()
	c, err := window.New(t.Context(), input, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func drain(t *testing.T, c storage.BatchCursor) ([][]model.Value, error) {
	t.Helper()
	var rows [][]model.Value
	out := columnar.Batch{MaxRows: 1}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		if out.Rows() != 1 {
			t.Fatalf("output rows=%d, want 1", out.Rows())
		}
		row := make([]model.Value, len(out.Columns))
		for i := range row {
			value, _, err := columnar.CopyValue(t.Context(), &out.Columns[i], out.Lane(0), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			row[i] = value
		}
		rows = append(rows, row)
	}
}

func released(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Fatalf("window retained query resources: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill files=%v, error=%v", files, err)
	}
}

func assertRows(t *testing.T, c storage.BatchCursor, want [][]model.Value) {
	t.Helper()
	got, err := drain(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestRankingPreservesMultiplicityAcrossPartitions(t *testing.T) {
	for _, mode := range []struct {
		name   string
		memory uint64
	}{{"spill", 0}, {"memory", 1 << 20}} {
		t.Run(mode.name, func(t *testing.T) {
			q, dir := query(t, false)
			layouts := []model.TypeLayout{int8, int8, int4}
			rows := [][]model.Value{{iv(1), iv(1), iv(3)}, {iv(1), iv(1), iv(99)}, {iv(1), iv(2), null()}, {iv(1), null(), iv(1)}, {iv(1), null(), iv(1)}, {iv(2), null(), iv(5)}}
			calls := []window.Call{{Function: 3100}, {Function: 3101}, {Function: 3102}, {Function: 3103}, {Function: 3104}, {Function: 3105, Arguments: []int{2}}}
			o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8, int8, float8, float8, int4}, calls)
			o.OutputColumns, o.PartitionColumns, o.OrderBy = []int{0}, []int{0}, []window.OrderKey{{Column: 1}}
			o.PartitionMemoryBytes = mode.memory
			c := open(t, newSource(t, layouts, rows), o)
			assertRows(t, c, [][]model.Value{
				{iv(1), iv(1), iv(1), iv(1), fv(0), fv(0.4), iv(1)},
				{iv(1), iv(2), iv(1), iv(1), fv(0), fv(0.4), iv(1)},
				{iv(1), iv(3), iv(3), iv(2), fv(0.5), fv(0.6), iv(2)},
				{iv(1), iv(4), iv(4), iv(3), fv(0.75), fv(1), iv(2)},
				{iv(1), iv(5), iv(4), iv(3), fv(0.75), fv(1), iv(3)},
				{iv(2), iv(1), iv(1), iv(1), fv(0), fv(1), iv(1)},
			})
			released(t, q, dir)
		})
	}
}

func TestLeadLagOffsetsNullsAndRowLocalDefaults(t *testing.T) {
	q, dir := query(t, false)
	layouts := []model.TypeLayout{int8, int4, int8}
	rows := [][]model.Value{{iv(10), iv(1), iv(91)}, {null(), iv(0), iv(92)}, {iv(30), iv(-1), iv(93)}, {iv(40), null(), iv(94)}, {iv(50), iv(math.MinInt32), iv(95)}}
	calls := []window.Call{{Function: 3108, Arguments: []int{0, 1, 2}}, {Function: 3111, Arguments: []int{0, 1, 2}}, {Function: 3106, Arguments: []int{0}}, {Function: 3109, Arguments: []int{0}}}
	o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8, int8, int8}, calls)
	o.OutputColumns = []int{0}
	o.PartitionMemoryBytes = 0
	assertRows(t, open(t, newSource(t, layouts, rows), o), [][]model.Value{
		{iv(10), iv(91), null(), null(), null()},
		{null(), null(), null(), iv(10), iv(30)},
		{iv(30), iv(40), null(), null(), iv(40)},
		{iv(40), null(), null(), iv(30), iv(50)},
		{iv(50), iv(95), iv(95), iv(40), null()},
	})
	released(t, q, dir)
}

func TestNtileInitialNullAndPartitionReset(t *testing.T) {
	q, dir := query(t, false)
	layouts := []model.TypeLayout{int8, int4}
	rows := [][]model.Value{{iv(1), null()}, {iv(1), iv(2)}, {iv(1), iv(9)}, {iv(1), iv(0)}, {iv(2), iv(4)}, {iv(2), iv(0)}}
	o := options(t, q, layouts, []model.TypeLayout{int4}, []window.Call{{Function: 3105, Arguments: []int{1}}})
	o.PartitionColumns = []int{0}
	assertRows(t, open(t, newSource(t, layouts, rows), o), [][]model.Value{{null()}, {iv(1)}, {iv(1)}, {iv(2)}, {iv(1)}, {iv(2)}})
	released(t, q, dir)
}

func TestEmptyWindowClosesInputWithoutOutput(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
	s := newSource(t, o.InputLayouts, nil)
	assertRows(t, open(t, s, o), nil)
	if !s.closed {
		t.Fatal("empty source remains open")
	}
	released(t, q, dir)
}
