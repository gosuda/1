package aggregate_test

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

var int8 = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var int4 = model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
var decimal = model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}
var float8 = model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}
var boolean = model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
var text = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

func number(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }

type source struct {
	builder  *columnar.Builder
	rows     [][]model.Value
	index    int
	pulls    int
	closed   bool
	selected bool
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 32, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: b, rows: rows}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.pulls++
	if s.index == len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	limit := 32
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for range min(limit, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	if err := s.builder.Publish(out); err != nil {
		return err
	}
	if s.selected {
		out.Selection = out.Selection[:0]
		for i := range out.Length {
			if i%2 == 0 {
				out.Selection = append(out.Selection, uint32(i))
			}
		}
	}
	return nil
}
func (s *source) Close() error { s.closed = true; return s.builder.Close() }

func query(t *testing.T, disabled bool, spillBytes int64) (*runtime.Query, string) {
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
	budget, err := runtime.NewBudget(192<<20, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 16 << 20, SpillBytes: spillBytes, Directory: dir, MaxFiles: 24, MaxReaders: 4}
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
func options(t *testing.T, q *runtime.Query, input, output []model.TypeLayout, groups []int, calls []aggregate.Call) aggregate.Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return aggregate.Options{InputLayouts: input, OutputLayouts: output, GroupColumns: groups, Calls: calls, Encoder: encoder, Query: q, BatchRows: 32, BatchBytes: 1 << 20, MaxRecordBytes: 1024, GroupBytes: 1 << 20, PartitionMemoryBytes: 4096, WorkBytes: 64 << 20, MaxPartitions: 16}
}
func open(t *testing.T, input storage.BatchCursor, o aggregate.Options) storage.BatchCursor {
	t.Helper()
	c, err := aggregate.New(t.Context(), input, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func drain(t *testing.T, c storage.BatchCursor) ([][]model.Value, error) {
	t.Helper()
	var rows [][]model.Value
	out := columnar.Batch{MaxRows: 3}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		if out.Rows() > 3 {
			t.Fatalf("output batch rows=%d, cap=3", out.Rows())
		}
		for i := range out.Rows() {
			row := make([]model.Value, len(out.Columns))
			for j := range row {
				v, _, err := columnar.CopyValue(t.Context(), &out.Columns[j], out.Lane(i), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				row[j] = v
			}
			rows = append(rows, row)
		}
	}
}
func released(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Fatalf("aggregate retained resources before query close: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill files=%v, error=%v", files, err)
	}
}
func exact(t *testing.T, got model.Value, want string) {
	t.Helper()
	if got.IsNull || got.Type != model.TypeNumeric {
		t.Fatalf("numeric result=%+v, want %s", got, want)
	}
	var fn builtin.Callable
	for sig := range builtin.SignaturesNamed("numeric_cmp") {
		var err error
		fn, err = builtin.Resolve(sig.ID, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	v, err := fn.Call(t.Context(), []model.Value{got, number(want)}, builtin.CallBudget{MemoryBytes: 4 << 20})
	if err != nil || v.IntVal != 0 {
		t.Fatalf("numeric result=%s, want %s, comparison=%v", got.StrVal, want, err)
	}
}

func TestGroupedMemoryAndDeferredSpillResults(t *testing.T) {
	layouts := []model.TypeLayout{int8, int4, decimal}
	calls := []aggregate.Call{{Function: 2803}, {Function: 2108, Arguments: []int{1}}, {Function: 2101, Arguments: []int{1}}, {Function: 2114, Arguments: []int{2}}, {Function: 2103, Arguments: []int{2}}, {Function: 2132, Arguments: []int{1}}, {Function: 2116, Arguments: []int{1}}}
	outputs := []model.TypeLayout{int8, int8, int8, decimal, decimal, decimal, int4, int4}
	var input [][]model.Value
	for round := range 6 {
		for g := range 9 {
			input = append(input, []model.Value{model.Int64Val(int64(g)), model.Int64Val(int64(round + 1)), number("9007199254740993")})
		}
	}
	for _, spill := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "spill"}[spill], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			o := options(t, q, layouts, outputs, []int{0}, calls)
			if spill {
				o.GroupBytes = 64 << 10
			}
			c := open(t, newSource(t, layouts, input), o)
			rows, err := drain(t, c)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 9 {
				t.Fatalf("groups=%d, want 9", len(rows))
			}
			seen := map[int64]bool{}
			for _, row := range rows {
				id := row[0].IntVal
				if id < 0 || id > 8 || seen[id] {
					t.Fatalf("duplicate or invalid group %d", id)
				}
				seen[id] = true
				if row[1].IntVal != 6 || row[2].IntVal != 21 || row[6].IntVal != 1 || row[7].IntVal != 6 {
					t.Fatalf("group %d count/sum/min/max=%v", id, row)
				}
				exact(t, row[3], "3.5")
				exact(t, row[4], "54043195528445958")
				exact(t, row[5], "9007199254740993")
			}
			if spill && q.Usage().Spill.Peak == 0 {
				t.Fatal("forced deferred partition did not spill")
			}
			if !spill && q.Usage().Spill.Peak != 0 {
				t.Fatal("resident groups unexpectedly spilled")
			}
			released(t, q, dir)
		})
	}
}

func TestDistinctFilterAndNullGrouping(t *testing.T) {
	layouts := []model.TypeLayout{decimal, int4, decimal, boolean}
	rows := [][]model.Value{
		{number("1.0"), model.Int64Val(2), number("9007199254740993"), model.BoolVal(false)},
		{number("1"), model.Int64Val(2), number("9007199254740993.00"), model.BoolVal(true)},
		{number("1.00"), model.Int64Val(2), number("9007199254740993"), model.BoolVal(true)},
		{number("1"), model.Int64Val(4), number("9007199254740995"), model.BoolVal(true)},
		{number("1"), model.Int64Val(9), number("11"), model.NullVal(model.TypeBoolean)},
		{model.NullVal(model.TypeNumeric), model.NullVal(model.TypeInt64), model.NullVal(model.TypeNumeric), model.BoolVal(true)},
		{model.NullVal(model.TypeNumeric), model.Int64Val(3), number("7"), model.BoolVal(true)},
		{model.NullVal(model.TypeNumeric), model.Int64Val(3), number("7.0"), model.BoolVal(true)},
	}
	calls := []aggregate.Call{{Function: 2147, Arguments: []int{1}, Distinct: true, FilterColumn: new(3)}, {Function: 2108, Arguments: []int{1}, Distinct: true, FilterColumn: new(3)}, {Function: 2103, Arguments: []int{2}, Distinct: true, FilterColumn: new(3)}, {Function: 2803, FilterColumn: new(3)}}
	q, dir := query(t, false, 32<<20)
	o := options(t, q, layouts, []model.TypeLayout{decimal, int8, int8, decimal, int8}, []int{0}, calls)
	o.PartitionMemoryBytes = 1024
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("groups=%d, want 2", len(got))
	}
	for _, row := range got {
		if row[0].IsNull {
			if row[1].IntVal != 1 || row[2].IntVal != 3 || row[4].IntVal != 3 {
				t.Fatalf("NULL group=%v", row)
			}
			exact(t, row[3], "7")
		} else {
			exact(t, row[0], "1")
			if row[1].IntVal != 2 || row[2].IntVal != 6 || row[4].IntVal != 3 {
				t.Fatalf("numeric group=%v", row)
			}
			exact(t, row[3], "9007199254740994")
		}
	}
	released(t, q, dir)
}

func TestEmptyInputGlobalAndGrouped(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "global", true: "grouped"}[grouped], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			var groups []int
			outputs := []model.TypeLayout{int8, int8, decimal, int4, int4}
			if grouped {
				groups = []int{0}
				outputs = append([]model.TypeLayout{int4}, outputs...)
			}
			calls := []aggregate.Call{{Function: 2803}, {Function: 2108, Arguments: []int{0}}, {Function: 2101, Arguments: []int{0}}, {Function: 2132, Arguments: []int{0}}, {Function: 2116, Arguments: []int{0}}}
			got, err := drain(t, open(t, newSource(t, []model.TypeLayout{int4}, nil), options(t, q, []model.TypeLayout{int4}, outputs, groups, calls)))
			if err != nil {
				t.Fatal(err)
			}
			if grouped {
				if len(got) != 0 {
					t.Fatalf("empty grouped rows=%v", got)
				}
			} else {
				if len(got) != 1 || got[0][0].IsNull || got[0][0].IntVal != 0 {
					t.Fatalf("empty global=%v", got)
				}
				for _, v := range got[0][1:] {
					if !v.IsNull {
						t.Fatalf("empty aggregate=%v, want NULL", v)
					}
				}
			}
			released(t, q, dir)
		})
	}
}

func TestFloatingNaNNullAndOverflow(t *testing.T) {
	for _, test := range []struct {
		name     string
		values   []float64
		function model.FunctionID
		nan      bool
		state    string
	}{
		{"NaN sum", []float64{1, math.NaN(), 2}, 2111, true, ""},
		{"NaN average", []float64{1, math.NaN(), 2}, 2105, true, ""},
		{"NaN maximum", []float64{1, math.NaN(), 2}, 2120, true, ""},
		{"NaN minimum", []float64{math.NaN(), 1, 2}, 2136, false, ""},
		{"finite sum overflow", []float64{math.MaxFloat64, math.MaxFloat64}, 2111, false, "22003"},
		{"infinity sum", []float64{math.Inf(1), math.Inf(-1)}, 2111, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			rows := [][]model.Value{{model.NullVal(model.TypeFloat64)}}
			for _, v := range test.values {
				rows = append(rows, []model.Value{model.Float64Val(v)})
			}
			c := open(t, newSource(t, []model.TypeLayout{float8}, rows), options(t, q, []model.TypeLayout{float8}, []model.TypeLayout{float8}, nil, []aggregate.Call{{Function: test.function, Arguments: []int{0}}}))
			got, err := drain(t, c)
			if test.state != "" {
				var sql *model.Error
				if !errors.As(err, &sql) || sql.Code != test.state {
					t.Fatalf("error=%v, want SQLSTATE %s", err, test.state)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 1 || got[0][0].IsNull {
					t.Fatalf("float result=%v", got)
				}
				if test.nan {
					if !math.IsNaN(got[0][0].FloatVal) {
						t.Fatalf("got %v, want NaN", got[0][0])
					}
				} else if got[0][0].FloatVal != 1 {
					t.Fatalf("minimum=%v, want 1", got[0][0])
				}
			}
			released(t, q, dir)
		})
	}
}

func TestInt8SumAndAverageDoNotOverflowThroughFloat(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	rows := [][]model.Value{{model.Int64Val(math.MaxInt64)}, {model.Int64Val(math.MaxInt64)}}
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{decimal, decimal}, nil, []aggregate.Call{{Function: 2107, Arguments: []int{0}}, {Function: 2100, Arguments: []int{0}}})
	got, err := drain(t, open(t, newSource(t, []model.TypeLayout{int8}, rows), o))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows=%v", got)
	}
	exact(t, got[0][0], "18446744073709551614")
	exact(t, got[0][1], "9223372036854775807")
	released(t, q, dir)
}

func TestFilterPreservesSelectedLanes(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{int4, boolean}
	s := newSource(t, layouts, [][]model.Value{{model.Int64Val(3), model.BoolVal(true)}, {model.Int64Val(100), model.BoolVal(true)}, {model.Int64Val(7), model.BoolVal(false)}, {model.Int64Val(200), model.BoolVal(true)}, {model.Int64Val(5), model.BoolVal(true)}})
	s.selected = true
	o := options(t, q, layouts, []model.TypeLayout{int8, int8}, nil, []aggregate.Call{{Function: 2803}, {Function: 2108, Arguments: []int{0}, FilterColumn: new(1)}})
	got, err := drain(t, open(t, s, o))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0][0].IntVal != 3 || got[0][1].IntVal != 8 {
		t.Fatalf("selected aggregate=%v, want count 3 sum 8", got)
	}
	released(t, q, dir)
}

func TestOrderedGroupsStreamAndCloseEarly(t *testing.T) {
	q, dir := query(t, true, 32<<20)
	var rows [][]model.Value
	for group := range 200 {
		rows = append(rows, []model.Value{model.Int64Val(int64(group)), model.Int64Val(2)}, []model.Value{model.Int64Val(int64(group)), model.Int64Val(3)})
	}
	layouts := []model.TypeLayout{int8, int4}
	s := newSource(t, layouts, rows)
	o := options(t, q, layouts, []model.TypeLayout{int8, int8}, []int{0}, []aggregate.Call{{Function: 2108, Arguments: []int{1}}})
	o.OrderedGroups = true
	c := open(t, s, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 0 || out.Columns[1].Int64[0] != 5 {
		t.Fatalf("first completed group=%v", out)
	}
	if s.index == len(rows) {
		t.Fatal("ordered aggregate consumed the entire input before its first group")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.closed {
		t.Fatal("input remains open")
	}
	released(t, q, dir)
}

func TestHotKeyFitsWithoutSpilling(t *testing.T) {
	q, dir := query(t, true, 32<<20)
	var rows [][]model.Value
	for range 1000 {
		rows = append(rows, []model.Value{model.Int64Val(1), model.Int64Val(3)})
	}
	layouts := []model.TypeLayout{int8, int4}
	o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8}, []int{0}, []aggregate.Call{{Function: 2803}, {Function: 2108, Arguments: []int{1}}})
	o.GroupBytes = 32 << 10
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0][1].IntVal != 1000 || got[0][2].IntVal != 3000 {
		t.Fatalf("hot group=%v", got)
	}
	released(t, q, dir)
}

func TestUnsupportedCallsFailBeforeInputConsumption(t *testing.T) {
	for _, call := range []aggregate.Call{{Function: 4000000000, Arguments: []int{0}}, {Function: 2803, Distinct: true}} {
		q, dir := query(t, false, 32<<20)
		output := decimal
		if call.Function == 2803 {
			output = int8
		}
		s := newSource(t, []model.TypeLayout{int8}, [][]model.Value{{model.Int64Val(2)}})
		_, err := aggregate.New(t.Context(), s, options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{output}, nil, []aggregate.Call{call}))
		var unsupported *aggregate.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("constructor error=%v, want unsupported", err)
		}
		if s.pulls != 0 || !s.closed {
			t.Fatalf("constructor ownership: pulls=%d closed=%v", s.pulls, s.closed)
		}
		released(t, q, dir)
	}
}

func TestSpillFailuresReleaseAllResources(t *testing.T) {
	for _, test := range []struct {
		name     string
		disabled bool
		quota    int64
		passes   int
	}{
		{"disabled", true, 32 << 20, 16}, {"quota", false, 64, 16}, {"partition limit", false, 32 << 20, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, test.disabled, test.quota)
			var rows [][]model.Value
			for i := range 100 {
				rows = append(rows, []model.Value{model.Int64Val(int64(i))})
			}
			o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8, int8}, []int{0}, []aggregate.Call{{Function: 2803}})
			o.GroupBytes = 16 << 10
			o.MaxPartitions = test.passes
			s := newSource(t, []model.TypeLayout{int8}, rows)
			_, err := drain(t, open(t, s, o))
			if err == nil {
				t.Fatal("bounded aggregation unexpectedly succeeded")
			}
			if !s.closed {
				t.Fatal("failed aggregate retained input")
			}
			released(t, q, dir)
		})
	}
}

func TestCancellationReleasesOrderedState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	ctx, cancel := context.WithCancel(t.Context())
	s := newSource(t, []model.TypeLayout{int8}, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}})
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8, int8}, []int{0}, []aggregate.Call{{Function: 2803}})
	o.OrderedGroups = true
	c, err := aggregate.New(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next=%v", err)
	}
	released(t, q, dir)
}
