package join_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var integer = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var boolean = model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
var text = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

type source struct {
	builder *columnar.Builder
	rows    [][]model.Value
	index   int
	closed  bool
	empty   bool
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 3, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: b, rows: rows, empty: true}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.builder.Reset()
	if s.empty {
		s.empty = false
		return s.builder.Publish(out)
	}
	if s.index == len(s.rows) {
		return io.EOF
	}
	n := 3
	if out.MaxRows > 0 {
		n = min(n, out.MaxRows)
	}
	for range min(n, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	s.empty = true
	return s.builder.Publish(out)
}
func (s *source) Close() error { s.closed = true; return s.builder.Close() }
func newQuery(t *testing.T, fs runtime.FileSystem, dir string) *runtime.Query {
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
	budget, err := runtime.NewBudget(64<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, fs, runtime.QueryConfig{WorkBytes: 32 << 20, OutputBytes: 4 << 20, SpillBytes: 32 << 20, Directory: dir, MaxFiles: 9, MaxReaders: 2})
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
func options(t *testing.T, q *runtime.Query, kind ir.JoinKind) join.Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	o := join.Options{Kind: kind, LeftLayouts: []model.TypeLayout{integer, integer}, RightLayouts: []model.TypeLayout{integer, integer}, OutputLayouts: []model.TypeLayout{integer, integer}, Output: []join.Column{{Side: join.Left, Index: 1}, {Side: join.Right, Index: 1}}, Keys: []join.Key{{Left: 0, Right: 0}}, Encoder: encoder, Query: q, BatchRows: 3, BatchBytes: 1 << 16, MaxRecordBytes: 4096, MemoryBytes: 1 << 20, HashBytes: 1 << 16, WorkBytes: 16 << 20, PartitionBytes: 4 << 20, MaxPartitions: 3, MaxDepth: 1}
	if kind == ir.SemiJoin || kind == ir.AntiJoin {
		o.Output = o.Output[:1]
		o.OutputLayouts = o.OutputLayouts[:1]
	}
	if kind == ir.CrossJoin {
		o.Keys = nil
	}
	return o
}
func open(t *testing.T, ctx context.Context, left, right storage.BatchCursor, o join.Options) storage.BatchCursor {
	t.Helper()
	c, err := join.New(ctx, left, right, o)
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
func released(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Fatalf("join retained resources: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill cleanup: files=%v error=%v", files, err)
	}
	if err := q.Context().Err(); err != nil {
		t.Fatalf("join closed borrowed query: %v", err)
	}
}
func collect(t *testing.T, c storage.BatchCursor) [][]model.Value {
	t.Helper()
	var rows [][]model.Value
	out := columnar.Batch{MaxRows: 2}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return rows
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() > 2 {
			t.Fatalf("batch rows %d exceed request", out.Rows())
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
func bag(rows [][]model.Value) []string {
	var result []string
	for _, row := range rows {
		var fields []string
		for _, v := range row {
			if v.IsNull {
				fields = append(fields, "NULL")
			} else {
				fields = append(fields, fmt.Sprint(v.IntVal))
			}
		}
		result = append(result, strings.Join(fields, ","))
	}
	slices.Sort(result)
	return result
}
func assertBag(t *testing.T, got [][]model.Value, want []string) {
	t.Helper()
	actual := bag(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("join bag = %v, want %v", actual, want)
	}
}
func pair(key, id int64) []model.Value { return []model.Value{model.Int64Val(key), model.Int64Val(id)} }

func TestJoinKindsPreserveBagsAcrossMemorySpillAndSkew(t *testing.T) {
	left := [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12), {model.NullVal(model.TypeInt64), model.Int64Val(13)}}
	right := [][]model.Value{pair(1, 20), pair(1, 21), pair(3, 22), {model.NullVal(model.TypeInt64), model.Int64Val(23)}}
	cases := []struct {
		name string
		kind ir.JoinKind
		want []string
	}{
		{"inner", ir.InnerJoin, []string{"10,20", "10,21", "11,20", "11,21"}},
		{"left", ir.LeftJoin, []string{"10,20", "10,21", "11,20", "11,21", "12,NULL", "13,NULL"}},
		{"right", ir.RightJoin, []string{"10,20", "10,21", "11,20", "11,21", "NULL,22", "NULL,23"}},
		{"full", ir.FullJoin, []string{"10,20", "10,21", "11,20", "11,21", "12,NULL", "13,NULL", "NULL,22", "NULL,23"}},
		{"semi", ir.SemiJoin, []string{"10", "11"}},
		{"anti", ir.AntiJoin, []string{"12", "13"}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"memory", "spill", "skew"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := options(t, q, tc.kind)
				if mode != "memory" {
					o.MemoryBytes = 0
				}
				if mode == "skew" {
					o.HashBytes = 1
				}
				l := newSource(t, o.LeftLayouts, left)
				r := newSource(t, o.RightLayouts, right)
				c := open(t, t.Context(), l, r, o)
				assertBag(t, collect(t, c), tc.want)
				if !l.closed || !r.closed {
					t.Fatal("join retained inputs")
				}
				released(t, q, dir)
			})
		}
	}
}

func TestCrossJoinRewindsWithoutMaterializingResult(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.CrossJoin)
	o.MemoryBytes = 0
	c := open(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 11)}), newSource(t, o.RightLayouts, [][]model.Value{pair(9, 20), pair(8, 21), pair(7, 22)}), o)
	assertBag(t, collect(t, c), []string{"10,20", "10,21", "10,22", "11,20", "11,21", "11,22"})
	released(t, q, dir)
}

func TestNullEqualKeysMatchWithoutChangingOrdinaryNulls(t *testing.T) {
	for _, nullEqual := range []bool{false, true} {
		t.Run(fmt.Sprint(nullEqual), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			o.MemoryBytes = 0
			o.Keys = []join.Key{{Left: 0, Right: 0, NullEqual: nullEqual}, {Left: 1, Right: 1}}
			rows := [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(7)}, pair(1, 8)}
			c := open(t, t.Context(), newSource(t, o.LeftLayouts, rows), newSource(t, o.RightLayouts, rows), o)
			want := []string{"8,8"}
			if nullEqual {
				want = append(want, "7,7")
			}
			assertBag(t, collect(t, c), want)
			released(t, q, dir)
		})
	}
}

func residual(t *testing.T) *expr.Program {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 64, MaxOperands: 128, MaxSlots: 8, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := b.AddLayout(t.Context(), boolean)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Nullable: true, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: layout, Slot: slot, Nullable: true}})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{slot}})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	program, err := expr.Compile(t.Context(), graph, []ir.ScalarID{scalar}, expr.Options{Limits: expr.DefaultLimits(), Inputs: []expr.Input{{Slot: slot, Column: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func TestResidualFalseAndUnknownRemainUnmatched(t *testing.T) {
	for _, hashBytes := range []uint64{0, 1, 1 << 16} {
		t.Run(fmt.Sprint(hashBytes), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.FullJoin)
			o.MemoryBytes = 0
			o.HashBytes = hashBytes
			o.Residual = residual(t)
			o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
			o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
			if hashBytes == 0 {
				o.Keys = nil
				o.HashBytes = 1
			}
			left := [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12)}
			right := [][]model.Value{{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)}, {model.Int64Val(1), model.Int64Val(21), model.BoolVal(false)}, {model.Int64Val(2), model.Int64Val(22), model.NullVal(model.TypeBoolean)}}
			c := open(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o)
			want := []string{"10,20", "11,20", "12,NULL", "NULL,21", "NULL,22"}
			if hashBytes == 0 {
				want = []string{"10,20", "11,20", "12,20", "NULL,21", "NULL,22"}
			}
			assertBag(t, collect(t, c), want)
			released(t, q, dir)
		})
	}
}

func TestEmptySidesHaveExactOuterAndExistenceSemantics(t *testing.T) {
	cases := []struct {
		name        string
		left, right [][]model.Value
		want        map[ir.JoinKind][]string
	}{
		{name: "left", right: [][]model.Value{pair(1, 20)}, want: map[ir.JoinKind][]string{ir.RightJoin: {"NULL,20"}, ir.FullJoin: {"NULL,20"}}},
		{name: "right", left: [][]model.Value{pair(1, 10)}, want: map[ir.JoinKind][]string{ir.LeftJoin: {"10,NULL"}, ir.FullJoin: {"10,NULL"}, ir.AntiJoin: {"10"}}},
		{name: "both"},
	}
	for _, kind := range []ir.JoinKind{ir.InnerJoin, ir.CrossJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin, ir.AntiJoin} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%d/%s", kind, tc.name), func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := options(t, q, kind)
				o.MemoryBytes = 0
				c := open(t, t.Context(), newSource(t, o.LeftLayouts, tc.left), newSource(t, o.RightLayouts, tc.right), o)
				assertBag(t, collect(t, c), tc.want[kind])
				released(t, q, dir)
			})
		}
	}
}

func TestNestedPayloadSurvivesSpillAndNullExtension(t *testing.T) {
	array := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	composite := model.TypeLayout{Type: model.Type{ID: 9001, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, array}}
	payload := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("nested payload"), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -1}}, ArrayVal: []model.Value{model.Int64Val(9), model.NullVal(model.TypeInt64)}}}}
	for _, memory := range []uint64{0, 1 << 20} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.FullJoin)
			o.MemoryBytes = memory
			o.RightLayouts = []model.TypeLayout{integer, composite}
			o.OutputLayouts = []model.TypeLayout{integer, composite}
			c := open(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 11)}), newSource(t, o.RightLayouts, [][]model.Value{{model.Int64Val(1), payload}}), o)
			rows := collect(t, c)
			if len(rows) != 2 {
				t.Fatalf("rows = %v", rows)
			}
			for _, row := range rows {
				if row[0].IntVal == 10 {
					if !reflect.DeepEqual(row[1], payload) {
						t.Fatalf("nested value = %#v, want %#v", row[1], payload)
					}
				} else if row[0].IntVal != 11 || !row[1].IsNull || row[1].Type != model.TypeComposite {
					t.Fatalf("null extension = %#v", row)
				}
			}
			released(t, q, dir)
		})
	}
}

func TestEarlyCloseAndCancellationReleaseReplayState(t *testing.T) {
	for _, cancelAfter := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelAfter), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.FullJoin)
			o.HashBytes = 1
			o.MemoryBytes = 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := open(t, ctx, newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 21), pair(1, 22)}), o)
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if cancelAfter {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error = %v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

type fullFS struct{ runtime.OSFileSystem }
type fullFile struct{ *os.File }

func (fullFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return fullFile{f}, nil
}
func (f fullFile) WriteAt(data []byte, offset int64) (int, error) {
	n, err := f.File.WriteAt(data[:min(3, len(data))], offset)
	return n, errors.Join(err, syscall.ENOSPC)
}

func TestResourceFailuresReleaseChildrenAndFiles(t *testing.T) {
	for _, scenario := range []string{"disk-full", "record", "partition", "output", "work", "canceled-constructor"} {
		t.Run(scenario, func(t *testing.T) { testJoinResourceFailure(t, scenario) })
	}
}

func testJoinResourceFailure(t *testing.T, scenario string) {
	t.Helper()
	dir := t.TempDir()
	var fs runtime.FileSystem = runtime.OSFileSystem{}
	if scenario == "disk-full" {
		fs = fullFS{}
	}
	q := newQuery(t, fs, dir)
	o := options(t, q, ir.InnerJoin)
	o.MemoryBytes = 0
	rows := [][]model.Value{pair(1, 10)}
	right := [][]model.Value{pair(1, 20)}
	want := error(runtime.ErrBudget)
	switch scenario {
	case "disk-full":
		want = syscall.ENOSPC
	case "record":
		o.LeftLayouts = []model.TypeLayout{integer, text}
		o.OutputLayouts = []model.TypeLayout{text, integer}
		rows = [][]model.Value{{model.Int64Val(1), model.StringVal(strings.Repeat("x", 8192))}}
	case "partition":
		o.PartitionBytes = 1
	case "output":
		o.BatchBytes = 1
	case "work":
		o.WorkBytes = 1
	case "canceled-constructor":
		want = context.Canceled
	}
	l := newSource(t, o.LeftLayouts, rows)
	r := newSource(t, o.RightLayouts, right)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if scenario == "canceled-constructor" {
		cancel()
	}
	c, err := join.New(ctx, l, r, o)
	if err == nil {
		var out columnar.Batch
		err = c.Next(ctx, &out)
		if closeErr := c.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if !l.closed || !r.closed {
		t.Fatal("failed join retained children")
	}
	released(t, q, dir)
}

type selectedSource struct{ *source }

func (s selectedSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := s.source.Next(ctx, out); err != nil {
		return err
	}
	if out.Length == 3 {
		out.Selection = []uint32{2, 0}
	}
	return nil
}

func TestInactiveLanesDoNotCreateMatches(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes = 0
	o.HashBytes = 1
	left := selectedSource{newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 99), pair(2, 11)})}
	right := selectedSource{newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 98), pair(3, 21)})}
	c := open(t, t.Context(), left, right, o)
	assertBag(t, collect(t, c), []string{"10,20", "11,NULL", "NULL,21"})
	released(t, q, dir)
}

func TestOutputByteLimitFlushesWithoutLosingPendingPair(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.MemoryBytes = 0
	o.RightLayouts = []model.TypeLayout{integer, text}
	o.OutputLayouts = []model.TypeLayout{integer, text}
	o.BatchBytes = 1600
	a := strings.Repeat("a", 600)
	b := strings.Repeat("b", 600)
	c := open(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, [][]model.Value{{model.Int64Val(1), model.StringVal(a)}, {model.Int64Val(1), model.StringVal(b)}}), o)
	rows := collect(t, c)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, fmt.Sprintf("%d:%s", row[0].IntVal, row[1].StrVal))
	}
	slices.Sort(got)
	want := []string{"10:" + a, "10:" + b, "11:" + a, "11:" + b}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("byte-limited pairs = %v, want %v", got, want)
	}
	released(t, q, dir)
}

func TestHashAcceptsNativePhysicalSelectionBeyondBatchBound(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.BatchRows = 1
	left := lookupSelectedSource{newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 98), pair(1, 99), pair(1, 10)})}
	right := lookupSelectedSource{newSource(t, o.RightLayouts, [][]model.Value{pair(1, 98), pair(1, 99), pair(1, 20)})}
	c := open(t, t.Context(), left, right, o)
	out := columnar.Batch{MaxRows: 1}
	defer out.Reset()
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 10 || out.Columns[1].Int64[0] != 20 {
		t.Fatalf("selected pair=%+v, want (10,20)", out)
	}
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("selected EOF=%v", err)
	}
	released(t, q, dir)
}

func TestHashRejectsLogicalRowsBeyondBatchBound(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.BatchRows = 1
	left := &lookupOwner{BatchCursor: lookupBatchFunc(func(_ context.Context, out *columnar.Batch) error {
		out.Length, out.MaxRows = 2, 2
		out.Columns = []columnar.Vector{
			{Type: integer.Type, Length: 2, Int64: []int64{1, 1}},
			{Type: integer.Type, Length: 2, Int64: []int64{10, 11}},
		}
		return nil
	})}
	right := &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20)})}
	c := open(t, t.Context(), left, right, o)
	out := columnar.Batch{MaxRows: 1}
	defer out.Reset()
	if err := c.Next(t.Context(), &out); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("oversized input error=%v, want ErrInvalidBatch", err)
	}
	if left.closes != 1 || right.closes != 1 {
		t.Fatalf("input closes: left=%d right=%d", left.closes, right.closes)
	}
	released(t, q, dir)
}
