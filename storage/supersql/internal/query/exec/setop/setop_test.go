package setop_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var int8 = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var decimal = model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}
var float8 = model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}
var text = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

func number(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }

type source struct {
	builder *columnar.Builder
	rows    [][]model.Value
	index   int
	pulls   int
	closed  bool
	failure error
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: b, rows: rows}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.pulls++
	if s.failure != nil {
		return s.failure
	}
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

func (s *source) Close() error { s.closed = true; return s.builder.Close() }

func query(t *testing.T, spillBytes int64) (*runtime.Query, string) {
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
	budget, err := runtime.NewBudget(128<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 64 << 20, OutputBytes: 8 << 20, SpillBytes: spillBytes, Directory: dir, MaxFiles: 4, MaxReaders: 2})
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

func options(t *testing.T, q *runtime.Query, layouts []model.TypeLayout) setop.Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return setop.Options{Mode: setop.Distinct, Layouts: layouts, Encoder: encoder, Query: q, BatchRows: 2, BatchBytes: 1 << 20, MaxRecordBytes: 1024, GroupBytes: 1 << 20, PartitionMemoryBytes: 0, MaxSpillBytes: 16 << 20, WorkBytes: 32 << 20, MaxPasses: 32}
}

func open(t *testing.T, o setop.Options, sources ...storage.BatchCursor) storage.BatchCursor {
	t.Helper()
	var c storage.BatchCursor
	var err error
	if len(sources) == 1 {
		c, err = setop.New(t.Context(), sources[0], o)
	} else {
		c, err = setop.NewBinary(t.Context(), sources[0], sources[1], o)
	}
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
			t.Fatalf("output rows=%d, want one", out.Rows())
		}
		row := make([]model.Value, len(out.Columns))
		for j := range row {
			v, _, err := columnar.CopyValue(t.Context(), &out.Columns[j], out.Lane(0), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			row[j] = v
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
		t.Fatalf("retained resources before Query.Close: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill files=%v, error=%v", files, err)
	}
}

func ints(values ...int64) [][]model.Value {
	rows := make([][]model.Value, len(values))
	for i, v := range values {
		rows[i] = []model.Value{model.Int64Val(v)}
	}
	return rows
}

func bag(rows [][]model.Value) map[string]int {
	result := map[string]int{}
	for _, row := range rows {
		key := "NULL"
		if !row[0].IsNull {
			key = fmt.Sprint(row[0].IntVal)
		}
		result[key]++
	}
	return result
}

func TestBinaryBagsResidentAndSpilled(t *testing.T) {
	left := append(ints(1, 1, 1, 2, 4), []model.Value{model.NullVal(model.TypeInt64)}, []model.Value{model.NullVal(model.TypeInt64)})
	right := append(ints(1, 1, 3, 3), []model.Value{model.NullVal(model.TypeInt64)})
	cases := []struct {
		name string
		kind ir.SetKind
		all  bool
		want map[string]int
	}{
		{"union", ir.Union, false, map[string]int{"1": 1, "2": 1, "3": 1, "4": 1, "NULL": 1}},
		{"union_all", ir.Union, true, map[string]int{"1": 5, "2": 1, "3": 2, "4": 1, "NULL": 3}},
		{"intersect", ir.Intersect, false, map[string]int{"1": 1, "NULL": 1}},
		{"intersect_all", ir.Intersect, true, map[string]int{"1": 2, "NULL": 1}},
		{"except", ir.Except, false, map[string]int{"2": 1, "4": 1}},
		{"except_all", ir.Except, true, map[string]int{"1": 1, "2": 1, "4": 1, "NULL": 1}},
	}
	for _, tc := range cases {
		for _, spill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spill=%t", tc.name, spill), func(t *testing.T) {
				q, dir := query(t, 16<<20)
				o := options(t, q, []model.TypeLayout{int8})
				o.Mode, o.Kind, o.All = setop.Binary, tc.kind, tc.all
				if spill {
					o.GroupBytes = 10 << 10
				}
				l, r := newSource(t, o.Layouts, left), newSource(t, o.Layouts, right)
				rows, err := drain(t, open(t, o, l, r))
				if err != nil {
					t.Fatal(err)
				}
				if got := bag(rows); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("bag=%v, want %v", got, tc.want)
				}
				if !l.closed || !r.closed {
					t.Fatal("inputs remain open")
				}
				if spill && !tc.all && q.Usage().Spill.Peak == 0 {
					t.Fatal("forced partition did not spill")
				}
				if !spill && q.Usage().Spill.Peak != 0 {
					t.Fatal("resident operation spilled")
				}
				released(t, q, dir)
			})
		}
	}
}

func TestEmptySides(t *testing.T) {
	cases := []struct {
		name        string
		left, right [][]model.Value
		want        map[ir.SetKind]int
	}{
		{name: "left", right: ints(7, 7), want: map[ir.SetKind]int{ir.Union: 1}},
		{name: "right", left: ints(7, 7), want: map[ir.SetKind]int{ir.Union: 1, ir.Except: 1}},
		{name: "both"},
	}
	for _, kind := range []ir.SetKind{ir.Union, ir.Intersect, ir.Except} {
		for _, multiplicity := range []struct {
			all    bool
			copies int
		}{{false, 1}, {true, 2}} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%d/all=%t/empty=%s", kind, multiplicity.all, tc.name), func(t *testing.T) {
					q, dir := query(t, 16<<20)
					o := options(t, q, []model.TypeLayout{int8})
					o.Mode, o.Kind, o.All = setop.Binary, kind, multiplicity.all
					rows, err := drain(t, open(t, o, newSource(t, o.Layouts, tc.left), newSource(t, o.Layouts, tc.right)))
					if err != nil {
						t.Fatal(err)
					}
					want := tc.want[kind] * multiplicity.copies
					if len(rows) != want {
						t.Fatalf("rows=%v, want %d copies of 7", rows, want)
					}
					for _, row := range rows {
						if row[0].IntVal != 7 {
							t.Fatalf("row=%v, want 7", row)
						}
					}
					released(t, q, dir)
				})
			}
		}
	}
}

func TestDistinctSQLNumericAndNaNEquality(t *testing.T) {
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprint(spill), func(t *testing.T) {
			q, dir := query(t, 16<<20)
			o := options(t, q, []model.TypeLayout{decimal, float8})
			if spill {
				o.GroupBytes = 10 << 10
			}
			input := [][]model.Value{
				{number("1.00"), model.Float64Val(math.NaN())},
				{number("2"), model.Float64Val(0)},
				{model.NullVal(model.TypeNumeric), model.NullVal(model.TypeFloat64)},
				{number("1e0"), model.Float64Val(math.Float64frombits(0x7ff8000000000001))},
				{number("2.000"), model.Float64Val(math.Copysign(0, -1))},
				{model.NullVal(model.TypeNumeric), model.NullVal(model.TypeFloat64)},
			}
			rows, err := drain(t, open(t, o, newSource(t, o.Layouts, input)))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("distinct rows=%v, want three SQL classes", rows)
			}
			counts := map[string]int{}
			for _, row := range rows {
				switch {
				case row[0].IsNull:
					counts["null"]++
				case math.IsNaN(row[1].FloatVal):
					counts["nan"]++
				case row[1].FloatVal == 0:
					counts["zero"]++
				default:
					t.Fatalf("unexpected row=%v", row)
				}
			}
			if !reflect.DeepEqual(counts, map[string]int{"null": 1, "nan": 1, "zero": 1}) {
				t.Fatalf("classes=%v", counts)
			}
			released(t, q, dir)
		})
	}
}

func TestDistinctOnRetainsFirstCompleteRowAcrossBatches(t *testing.T) {
	q, dir := query(t, 16<<20)
	o := options(t, q, []model.TypeLayout{decimal, int8, text})
	o.Mode = setop.DistinctOn
	o.DistinctColumns = []int{0}
	o.OrderedInput = []setop.OrderKey{{Column: 0, NullsFirst: true}, {Column: 1, Descending: true}}
	rows := [][]model.Value{
		{model.NullVal(model.TypeNumeric), model.Int64Val(9), model.StringVal("null-first")},
		{model.NullVal(model.TypeNumeric), model.Int64Val(8), model.StringVal("null-later")},
		{model.NullVal(model.TypeNumeric), model.Int64Val(7), model.StringVal("null-last")},
		{number("1.00"), model.Int64Val(10), model.StringVal("one-first")},
		{number("1"), model.Int64Val(5), model.StringVal("one-later")},
		{number("2"), model.Int64Val(1), model.StringVal("two-first")},
	}
	got, err := drain(t, open(t, o, newSource(t, o.Layouts, rows)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows=%v, want three", got)
	}
	for i, want := range []string{"null-first", "one-first", "two-first"} {
		if got[i][2].StrVal != want {
			t.Fatalf("representative %d=%v, want %s", i, got[i], want)
		}
	}
	if got[1][0].StrVal != "1.00" || got[1][1].IntVal != 10 {
		t.Fatalf("representative changed: %v", got[1])
	}
	released(t, q, dir)
}

func TestDistinctOnRejectsMissingProofBeforePull(t *testing.T) {
	q, dir := query(t, 16<<20)
	o := options(t, q, []model.TypeLayout{int8})
	o.Mode, o.DistinctColumns = setop.DistinctOn, []int{0}
	s := newSource(t, o.Layouts, ints(1))
	_, err := setop.New(t.Context(), s, o)
	if !errors.Is(err, setop.ErrUnprovenOrder) {
		t.Fatalf("error=%v", err)
	}
	if s.pulls != 0 || !s.closed {
		t.Fatalf("preflight pulls=%d closed=%t", s.pulls, s.closed)
	}
	released(t, q, dir)
}

func TestDistinctOnRejectsViolatedOrdering(t *testing.T) {
	q, dir := query(t, 16<<20)
	o := options(t, q, []model.TypeLayout{int8})
	o.Mode, o.DistinctColumns, o.OrderedInput = setop.DistinctOn, []int{0}, []setop.OrderKey{{Column: 0}}
	_, err := drain(t, open(t, o, newSource(t, o.Layouts, ints(1, 2, 1))))
	if !errors.Is(err, setop.ErrInputOrder) {
		t.Fatalf("error=%v", err)
	}
	released(t, q, dir)
}

func TestResourceLimitsCloseInputsAndSpools(t *testing.T) {
	for _, limit := range []string{"group", "work", "spill", "query-spill", "passes", "batch"} {
		t.Run(limit, func(t *testing.T) {
			quota := int64(16 << 20)
			if limit == "query-spill" {
				quota = 1
			}
			q, dir := query(t, quota)
			o := options(t, q, []model.TypeLayout{int8})
			o.GroupBytes = 10 << 10
			switch limit {
			case "group":
				o.GroupBytes = 1
			case "work":
				o.WorkBytes = 1
			case "spill":
				o.MaxSpillBytes = 1
			case "passes":
				o.MaxPasses = 1
			case "batch":
				o.BatchBytes = 1
			}
			s := newSource(t, o.Layouts, ints(1, 2, 3, 1, 2, 3))
			c, err := setop.New(t.Context(), s, o)
			if err == nil {
				_, err = drain(t, c)
			}
			if err == nil {
				t.Fatal("quota did not fail")
			}
			if !errors.Is(err, runtime.ErrBudget) && !errors.Is(err, columnar.ErrBatchBudget) {
				t.Fatalf("resource error=%v", err)
			}
			if !s.closed {
				t.Fatal("input remains open")
			}
			released(t, q, dir)
		})
	}
}

func TestCancellationAndEarlyCloseReleaseSpilledState(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			q, dir := query(t, 16<<20)
			o := options(t, q, []model.TypeLayout{int8})
			o.GroupBytes = 10 << 10
			c := open(t, o, newSource(t, o.Layouts, ints(1, 2, 3, 1, 2, 3)))
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if q.Usage().Spill.Live == 0 {
				t.Fatal("expected live deferred spool")
			}
			if action == "cancel" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := c.Next(ctx, &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestBinaryInputErrorClosesBothSides(t *testing.T) {
	q, dir := query(t, 16<<20)
	o := options(t, q, []model.TypeLayout{int8})
	o.Mode, o.Kind = setop.Binary, ir.Intersect
	left, right := newSource(t, o.Layouts, ints(1)), newSource(t, o.Layouts, ints(1))
	failure := errors.New("source read failed")
	left.failure = failure
	_, err := drain(t, open(t, o, left, right))
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if !left.closed || !right.closed {
		t.Fatal("input error left an input open")
	}
	released(t, q, dir)
}

func TestBinaryNumericKeysUseNotDistinctEquality(t *testing.T) {
	for _, kind := range []ir.SetKind{ir.Intersect, ir.Except} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			q, dir := query(t, 16<<20)
			o := options(t, q, []model.TypeLayout{decimal, float8})
			o.Mode, o.Kind, o.All, o.GroupBytes = setop.Binary, kind, true, 10<<10
			left := [][]model.Value{
				{number("3.000"), model.Float64Val(math.NaN())},
				{number("4.0"), model.Float64Val(0)},
				{number("3"), model.Float64Val(math.NaN())},
			}
			right := [][]model.Value{
				{number("3e0"), model.Float64Val(math.Float64frombits(0x7ff8000000000001))},
				{number("4"), model.Float64Val(math.Copysign(0, -1))},
			}
			got, err := drain(t, open(t, o, newSource(t, o.Layouts, left), newSource(t, o.Layouts, right)))
			if err != nil {
				t.Fatal(err)
			}
			nans, zeros := 0, 0
			for _, row := range got {
				if math.IsNaN(row[1].FloatVal) {
					nans++
				} else if row[1].FloatVal == 0 {
					zeros++
				} else {
					t.Fatalf("unexpected row=%v", row)
				}
			}
			wantZeros := 0
			if kind == ir.Intersect {
				wantZeros = 1
			}
			if nans != 1 || zeros != wantZeros {
				t.Fatalf("NaN multiplicity=%d, zero multiplicity=%d; want 1 and %d", nans, zeros, wantZeros)
			}
			released(t, q, dir)
		})
	}
}

func TestInvalidBinarySemanticsCloseBothInputsBeforePull(t *testing.T) {
	for _, invalid := range []string{"kind", "columns", "mode"} {
		t.Run(invalid, func(t *testing.T) {
			q, dir := query(t, 16<<20)
			o := options(t, q, []model.TypeLayout{int8})
			o.Mode, o.Kind = setop.Binary, ir.Union
			switch invalid {
			case "kind":
				o.Kind = 255
			case "columns":
				o.DistinctColumns = []int{0}
			case "mode":
				o.Mode = setop.Distinct
			}
			left, right := newSource(t, o.Layouts, ints(1)), newSource(t, o.Layouts, ints(1))
			_, err := setop.NewBinary(t.Context(), left, right, o)
			if !errors.Is(err, runtime.ErrInvalidConfig) {
				t.Fatalf("error=%v", err)
			}
			if left.pulls != 0 || right.pulls != 0 || !left.closed || !right.closed {
				t.Fatal("invalid plan consumed or retained an input")
			}
			released(t, q, dir)
		})
	}
}

func TestDistinctEmptyAndZeroColumnRows(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			q, dir := query(t, 16<<20)
			o := options(t, q, nil)
			var rows [][]model.Value
			if populated {
				rows = [][]model.Value{{}, {}, {}}
			}
			got, err := drain(t, open(t, o, newSource(t, nil, rows)))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if populated {
				want = 1
			}
			if len(got) != want {
				t.Fatalf("zero-column distinct rows=%d, want %d", len(got), want)
			}
			released(t, q, dir)
		})
	}
}
