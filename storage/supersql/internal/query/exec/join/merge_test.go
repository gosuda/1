package join_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func openMerge(t *testing.T, ctx context.Context, left, right storage.BatchCursor, o join.Options, ordering []join.MergeOrder) storage.BatchCursor {
	t.Helper()
	c, err := join.NewMerge(ctx, left, right, o, ordering)
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

func TestMergeJoinKindsReplayDuplicatesWithSelectiveResidual(t *testing.T) {
	cases := []struct {
		kind ir.JoinKind
		want []string
	}{
		{ir.InnerJoin, []string{"10,20", "11,20"}},
		{ir.LeftJoin, []string{"10,20", "11,20", "12,NULL", "13,NULL"}},
		{ir.RightJoin, []string{"10,20", "11,20", "NULL,21", "NULL,22", "NULL,24"}},
		{ir.FullJoin, []string{"10,20", "11,20", "12,NULL", "13,NULL", "NULL,21", "NULL,22", "NULL,24"}},
		{ir.SemiJoin, []string{"10", "11"}},
		{ir.AntiJoin, []string{"12", "13"}},
	}
	for _, tc := range cases {
		for _, memory := range []uint64{0, 1 << 20} {
			t.Run(fmt.Sprintf("%d/%d", tc.kind, memory), func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := options(t, q, tc.kind)
				o.MemoryBytes = memory
				o.Residual = residual(t)
				o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
				o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
				left := [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12), pair(3, 13)}
				right := [][]model.Value{
					{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)},
					{model.Int64Val(1), model.Int64Val(21), model.BoolVal(false)},
					{model.Int64Val(2), model.Int64Val(22), model.NullVal(model.TypeBoolean)},
					{model.Int64Val(4), model.Int64Val(24), model.BoolVal(true)},
				}
				c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{}})
				assertBag(t, collect(t, c), tc.want)
				released(t, q, dir)
			})
		}
	}
}

func TestMergeRightMatchesPersistWithoutReevaluatingResidual(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes, o.HashBytes, o.MaxDepth = 0, 1, 4
	o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
	o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
	results := []bool{true, false, false, false, true, false}
	calls := 0
	o.Residual = effectPredicate(t, func(_ context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
		if calls >= len(results) {
			t.Fatal("merge re-evaluated a residual to discover unmatched rows")
		}
		result := results[calls]
		calls++
		return model.BoolVal(result), nil
	})
	left := [][]model.Value{pair(1, 10), pair(1, 11)}
	right := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)},
		{model.Int64Val(1), model.Int64Val(21), model.BoolVal(true)},
		{model.Int64Val(1), model.Int64Val(22), model.BoolVal(true)},
	}
	c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{}})
	assertBag(t, collect(t, c), []string{"10,20", "11,21", "NULL,22"})
	if calls != 6 {
		t.Fatalf("residual effects = %d, want 6", calls)
	}
	released(t, q, dir)
}

func TestMergeOrderingAndNullEquality(t *testing.T) {
	for _, descending := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			for _, nullEqual := range []bool{false, true} {
				t.Run(fmt.Sprintf("desc=%t/nullsFirst=%t/nullEqual=%t", descending, nullsFirst, nullEqual), func(t *testing.T) {
					dir := t.TempDir()
					q := newQuery(t, runtime.OSFileSystem{}, dir)
					o := options(t, q, ir.FullJoin)
					o.Keys[0].NullEqual = nullEqual
					left := [][]model.Value{pair(1, 10), pair(2, 11)}
					right := [][]model.Value{pair(1, 20), pair(2, 21)}
					if descending {
						left[0], left[1] = left[1], left[0]
						right[0], right[1] = right[1], right[0]
					}
					ln := [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(12)}, {model.NullVal(model.TypeInt64), model.Int64Val(13)}}
					rn := [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(22)}}
					if nullsFirst {
						left, right = append(ln, left...), append(rn, right...)
					} else {
						left, right = append(left, ln...), append(right, rn...)
					}
					want := []string{"10,20", "11,21", "12,NULL", "13,NULL", "NULL,22"}
					if nullEqual {
						want = []string{"10,20", "11,21", "12,22", "13,22"}
					}
					c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{Descending: descending, NullsFirst: nullsFirst}})
					assertBag(t, collect(t, c), want)
					released(t, q, dir)
				})
			}
		}
	}
}

func TestMergeCompoundOrdering(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.LeftLayouts, o.RightLayouts = []model.TypeLayout{integer, integer, integer}, []model.TypeLayout{integer, integer, integer}
	o.Keys = []join.Key{{Left: 0, Right: 0}, {Left: 1, Right: 1}}
	o.Output = []join.Column{{Side: join.Left, Index: 2}, {Side: join.Right, Index: 2}}
	left := [][]model.Value{{model.Int64Val(1), model.Int64Val(3), model.Int64Val(10)}, {model.Int64Val(1), model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(2), model.Int64Val(4), model.Int64Val(12)}}
	right := [][]model.Value{{model.Int64Val(1), model.Int64Val(3), model.Int64Val(20)}, {model.Int64Val(1), model.Int64Val(2), model.Int64Val(21)}, {model.Int64Val(2), model.Int64Val(4), model.Int64Val(22)}}
	c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{}, {Descending: true}})
	assertBag(t, collect(t, c), []string{"10,20", "11,NULL", "NULL,21", "12,22"})
	released(t, q, dir)
}

func TestMergeNumericAndNaNKeysUseSQLEquality(t *testing.T) {
	cases := []struct {
		name        string
		layout      model.TypeLayout
		left, right []model.Value
	}{
		{"numeric", model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}, []model.Value{{Type: model.TypeNumeric, StrVal: "-2.00"}, {Type: model.TypeNumeric, StrVal: "1.0"}, {Type: model.TypeNumeric, StrVal: "NaN"}}, []model.Value{{Type: model.TypeNumeric, StrVal: "-2"}, {Type: model.TypeNumeric, StrVal: "1.000"}, {Type: model.TypeNumeric, StrVal: "NaN"}}},
		{"float", model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}, []model.Value{model.Float64Val(math.Inf(-1)), model.Float64Val(math.Copysign(0, -1)), model.Float64Val(math.Float64frombits(0x7ff8000000000001))}, []model.Value{model.Float64Val(math.Inf(-1)), model.Float64Val(0), model.Float64Val(math.Float64frombits(0xfff0000000000001))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			o.MemoryBytes = 0
			o.LeftLayouts, o.RightLayouts = []model.TypeLayout{tc.layout, integer}, []model.TypeLayout{tc.layout, integer}
			left := [][]model.Value{{tc.left[0], model.Int64Val(10)}, {tc.left[1], model.Int64Val(11)}, {tc.left[2], model.Int64Val(12)}}
			right := [][]model.Value{{tc.right[0], model.Int64Val(20)}, {tc.right[1], model.Int64Val(21)}, {tc.right[2], model.Int64Val(22)}}
			c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{}})
			assertBag(t, collect(t, c), []string{"10,20", "11,21", "12,22"})
			released(t, q, dir)
		})
	}
}

func TestMergeSkewSpillsWithoutMaterializingProduct(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes, o.HashBytes, o.MaxDepth = 0, 1, 8
	right := make([][]model.Value, 100)
	want := make([]string, 0, 200)
	for i := range right {
		right[i] = pair(1, int64(1000+i))
		want = append(want, fmt.Sprintf("10,%d", 1000+i), fmt.Sprintf("11,%d", 1000+i))
	}
	c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, right), o, []join.MergeOrder{{}})
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 10 || out.Columns[1].Int64[0] != 1000 {
		t.Fatalf("first skew output = %+v", out)
	}
	usage := q.Usage()
	if usage.Files == 0 || usage.Readers > 1 || usage.Work.Live > int64(o.WorkBytes) {
		t.Fatalf("skew admission = %+v, work bound %d", usage, o.WorkBytes)
	}
	rows := append([][]model.Value{{model.Int64Val(10), model.Int64Val(1000)}}, collect(t, c)...)
	assertBag(t, rows, want)
	released(t, q, dir)
}

var errMergeLater = errors.New("later merge source failure")

type mergeFailSource struct {
	*source
	after int
}

func (s mergeFailSource) Next(ctx context.Context, out *columnar.Batch) error {
	if s.index >= s.after {
		return errMergeLater
	}
	return s.source.Next(ctx, out)
}

func TestMergeFirstDemandPrecedesLaterRightError(t *testing.T) {
	for _, kind := range []ir.JoinKind{ir.InnerJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, kind)
			o.MemoryBytes = 0
			left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})
			right := newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 21)})
			c := openMerge(t, t.Context(), left, mergeFailSource{source: right, after: 1}, o, []join.MergeOrder{{}})
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatalf("first demanded result encountered later source error: %v", err)
			}
			if out.Rows() != 1 || out.Columns[0].Int64[0] != 10 {
				t.Fatalf("first demanded result = %+v", out)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !left.closed || !right.closed {
				t.Fatal("early merge close retained source")
			}
			released(t, q, dir)
		})
	}
}

func TestMergeEmptyLeftDoesNotReadUnneededRight(t *testing.T) {
	for _, kind := range []ir.JoinKind{ir.InnerJoin, ir.LeftJoin, ir.SemiJoin, ir.AntiJoin} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, kind)
			c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, nil), &forbiddenMarkSource{}, o, []join.MergeOrder{{}})
			assertBag(t, collect(t, c), nil)
			released(t, q, dir)
		})
	}
}

func TestMergeEmptyLeftPreservesRight(t *testing.T) {
	for _, kind := range []ir.JoinKind{ir.RightJoin, ir.FullJoin} {
		for _, scenario := range []struct {
			name string
			rows [][]model.Value
			want []string
		}{{"empty", nil, nil}, {"row", [][]model.Value{pair(1, 20)}, []string{"NULL,20"}}} {
			t.Run(fmt.Sprintf("%d/%s", kind, scenario.name), func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := options(t, q, kind)
				c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, nil), newSource(t, o.RightLayouts, scenario.rows), o, []join.MergeOrder{{}})
				assertBag(t, collect(t, c), scenario.want)
				released(t, q, dir)
			})
		}
	}
}

func TestMergeEmptyRight(t *testing.T) {
	for _, scenario := range []struct {
		kind ir.JoinKind
		want []string
	}{{ir.InnerJoin, nil}, {ir.LeftJoin, []string{"10,NULL"}}, {ir.RightJoin, nil}, {ir.FullJoin, []string{"10,NULL"}}, {ir.SemiJoin, nil}, {ir.AntiJoin, []string{"10"}}} {
		t.Run(fmt.Sprint(scenario.kind), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, scenario.kind)
			c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)}), newSource(t, o.RightLayouts, nil), o, []join.MergeOrder{{}})
			assertBag(t, collect(t, c), scenario.want)
			released(t, q, dir)
		})
	}
}

func TestMergeOwnsMetadataAndNestedReplayRows(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes = 0
	array := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	o.RightLayouts, o.OutputLayouts = []model.TypeLayout{integer, array}, []model.TypeLayout{integer, array}
	payload := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -1}}, ArrayVal: []model.Value{model.Int64Val(9), model.NullVal(model.TypeInt64)}}
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12)})
	right := newSource(t, o.RightLayouts, [][]model.Value{{model.Int64Val(1), payload}})
	ordering := []join.MergeOrder{{}}
	c := openMerge(t, t.Context(), left, right, o, ordering)
	o.Keys[0].Left = 1
	o.Output[0].Index = 0
	o.LeftLayouts[0] = boolean
	o.RightLayouts[1].Children[0] = text
	ordering[0].Descending = true
	rows := collect(t, c)
	if len(rows) != 3 {
		t.Fatalf("nested replay rows = %v", rows)
	}
	for i, id := range []int64{10, 11} {
		if rows[i][0].IntVal != id || !reflect.DeepEqual(rows[i][1], payload) {
			t.Fatalf("replayed row %d = %#v", i, rows[i])
		}
	}
	if rows[2][0].IntVal != 12 || !rows[2][1].IsNull || rows[2][1].Type != model.TypeArray {
		t.Fatalf("typed outer row = %#v", rows[2])
	}
	released(t, q, dir)
}

func TestMergeRejectsUnsortedInputAfterDemandedPrefix(t *testing.T) {
	for _, side := range []join.Side{join.Left, join.Right} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.FullJoin)
			leftRows, rightRows := [][]model.Value{pair(2, 10), pair(3, 11)}, [][]model.Value{pair(2, 20), pair(3, 21)}
			if side == join.Left {
				leftRows[1] = pair(1, 11)
			} else {
				rightRows[1] = pair(1, 21)
			}
			left, right := newSource(t, o.LeftLayouts, leftRows), newSource(t, o.RightLayouts, rightRows)
			c, err := join.NewMerge(t.Context(), left, right, o, []join.MergeOrder{{}})
			if err != nil {
				t.Fatal(err)
			}
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			err = c.Next(t.Context(), &out)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "XX000" {
				t.Fatalf("unsorted %d error = %v", side, err)
			}
			if !left.closed || !right.closed {
				t.Fatal("unsorted input retained children")
			}
			released(t, q, dir)
		})
	}
}

func TestMergeCancellationClosesActiveGroup(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes = 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	left, right := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 21)})
	c := openMerge(t, ctx, left, right, o, []join.MergeOrder{{}})
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled merge = %v", err)
	}
	if !left.closed || !right.closed {
		t.Fatal("cancellation retained child cursors")
	}
	released(t, q, dir)
}

func TestMergeResourceFailuresReleaseOwners(t *testing.T) {
	for _, scenario := range []string{"disk-full", "record", "group", "output", "work", "match-levels", "source"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			var fs runtime.FileSystem = runtime.OSFileSystem{}
			if scenario == "disk-full" {
				fs = fullFS{}
			}
			q := newQuery(t, fs, dir)
			o := options(t, q, ir.FullJoin)
			o.MemoryBytes = 0
			leftRows, rightRows := [][]model.Value{pair(1, 10)}, [][]model.Value{pair(1, 20), pair(1, 21)}
			want := error(runtime.ErrBudget)
			switch scenario {
			case "disk-full":
				want = syscall.ENOSPC
			case "record":
				o.LeftLayouts, o.OutputLayouts = []model.TypeLayout{integer, text}, []model.TypeLayout{text, integer}
				leftRows = [][]model.Value{{model.Int64Val(1), model.StringVal(strings.Repeat("x", 8192))}}
			case "group":
				o.PartitionBytes = 1
			case "output":
				o.BatchBytes = 1
			case "work":
				o.WorkBytes = 1
			case "match-levels":
				o.HashBytes, o.MaxDepth = 1, 1
			case "source":
				want = errMergeLater
			}
			left, right := newSource(t, o.LeftLayouts, leftRows), newSource(t, o.RightLayouts, rightRows)
			var rightCursor storage.BatchCursor = right
			if scenario == "source" {
				rightCursor = mergeFailSource{source: right, after: 0}
			}
			err := mergeFailure(t.Context(), left, rightCursor, o)
			if !errors.Is(err, want) {
				t.Fatalf("merge failure = %v, want %v", err, want)
			}
			if !left.closed || !right.closed {
				t.Fatal("failed merge retained children")
			}
			released(t, q, dir)
		})
	}
}

var errMergeRead = errors.New("merge replay read failure")

type mergeReadFS struct{ runtime.OSFileSystem }
type mergeReadFile struct{ *os.File }

func (mergeReadFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return mergeReadFile{file}, nil
}

func (mergeReadFile) ReadAt([]byte, int64) (int, error) { return 0, errMergeRead }

func TestMergeReplayReadFailureReleasesSpill(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, mergeReadFS{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.MemoryBytes = 0
	left, right := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20)})
	c := openMerge(t, t.Context(), left, right, o, []join.MergeOrder{{}})
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if err := c.Next(t.Context(), &out); !errors.Is(err, errMergeRead) {
		t.Fatalf("replay read error = %v", err)
	}
	released(t, q, dir)
}

func TestMergeSupportRejectsInvalidContractsBeforeConsumption(t *testing.T) {
	for _, scenario := range []string{"cross", "mark", "no-keys", "partial-order", "key-layout", "unsupported-key", "right-semi-output", "work", "depth"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			ordering := []join.MergeOrder{{}}
			switch scenario {
			case "cross":
				o.Kind = ir.CrossJoin
			case "mark":
				o.Kind, o.MarkKind = ir.MarkJoin, ir.ExistsSubquery
			case "no-keys":
				o.Keys, ordering = nil, nil
			case "partial-order":
				ordering = nil
			case "key-layout":
				o.Keys[0].Right = 4
			case "unsupported-key":
				o.LeftLayouts[0], o.RightLayouts[0] = model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeInt64}}, model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeInt64}}
			case "right-semi-output":
				o.Kind = ir.SemiJoin
			case "work":
				o.WorkBytes = 1
			case "depth":
				o.MaxDepth = 0
			}
			if err := join.CheckMergeSupport(t.Context(), o, ordering); err == nil {
				t.Fatal("invalid merge support accepted")
			}
			left, right := &forbiddenMarkSource{}, &forbiddenMarkSource{}
			c, err := join.NewMerge(t.Context(), left, right, o, ordering)
			if err == nil {
				if closeErr := c.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("invalid merge constructor accepted")
			}
			if !left.closed || !right.closed || errors.Is(err, errEmptyMarkRight) {
				t.Fatalf("invalid merge consumed or retained child: %v", err)
			}
			released(t, q, dir)
		})
	}
}

func TestMergeSemiReplaysDemandedPrefixWithoutDrainingRight(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.SemiJoin)
	right := newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 21)})
	c := openMerge(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), mergeFailSource{source: right, after: 1}, o, []join.MergeOrder{{}})
	out := columnar.Batch{MaxRows: 1}
	for _, id := range []int64{10, 11} {
		if err := c.Next(t.Context(), &out); err != nil {
			t.Fatalf("semi duplicate %d: %v", id, err)
		}
		if out.Rows() != 1 || out.Columns[0].Int64[0] != id {
			t.Fatalf("semi duplicate = %+v, want %d", out, id)
		}
	}
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("semi exhaustion = %v", err)
	}
	released(t, q, dir)
}

func TestMergeByteLimitedOutputKeepsPendingReplayPair(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.MemoryBytes, o.BatchBytes = 0, 1600
	o.RightLayouts, o.OutputLayouts = []model.TypeLayout{integer, text}, []model.TypeLayout{integer, text}
	a, b := strings.Repeat("a", 600), strings.Repeat("b", 600)
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)})
	right := newSource(t, o.RightLayouts, [][]model.Value{{model.Int64Val(1), model.StringVal(a)}, {model.Int64Val(1), model.StringVal(b)}})
	c := openMerge(t, t.Context(), left, right, o, []join.MergeOrder{{}})
	got := make(map[string]int)
	for _, row := range collect(t, c) {
		got[fmt.Sprintf("%d:%s", row[0].IntVal, row[1].StrVal)]++
	}
	want := map[string]int{"10:" + a: 1, "10:" + b: 1, "11:" + a: 1, "11:" + b: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("byte-limited replay bag = %v, want %v", got, want)
	}
	released(t, q, dir)
}

var (
	errMergeCloseLeft  = errors.New("merge left close failure")
	errMergeCloseRight = errors.New("merge right close failure")
)

type mergeCloseSource struct {
	storage.BatchCursor
	failure error
}

func (s mergeCloseSource) Close() error {
	return errors.Join(s.BatchCursor.Close(), s.failure)
}

func TestMergeFailureJoinsBothChildCleanupCauses(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left, right := newSource(t, o.LeftLayouts, nil), newSource(t, o.RightLayouts, nil)
	l := mergeCloseSource{BatchCursor: mergeFailSource{source: left, after: 0}, failure: errMergeCloseLeft}
	r := mergeCloseSource{BatchCursor: right, failure: errMergeCloseRight}
	c, err := join.NewMerge(t.Context(), l, r, o, []join.MergeOrder{{}})
	if err != nil {
		t.Fatal(err)
	}
	var out columnar.Batch
	err = c.Next(t.Context(), &out)
	if !errors.Is(err, errMergeLater) || !errors.Is(err, errMergeCloseLeft) || !errors.Is(err, errMergeCloseRight) {
		t.Fatalf("joined merge failure = %v", err)
	}
	if err := c.Close(); !errors.Is(err, errMergeCloseLeft) || !errors.Is(err, errMergeCloseRight) {
		t.Fatalf("repeated close lost cleanup causes: %v", err)
	}
	if !left.closed || !right.closed {
		t.Fatal("cleanup error skipped a child")
	}
	released(t, q, dir)
}

func TestMergeSemiExtendsCachedPrefixForLaterLeftRows(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.SemiJoin)
	o.MemoryBytes = 256
	o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
	o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
	results := []bool{true, false, true, false, false, true}
	calls := 0
	o.Residual = effectPredicate(t, func(_ context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
		if calls >= len(results) {
			t.Fatal("semi join evaluated beyond a decisive match")
		}
		result := results[calls]
		calls++
		return model.BoolVal(result), nil
	})
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11), pair(1, 12)})
	right := newSource(t, o.RightLayouts, [][]model.Value{
		{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)},
		{model.Int64Val(1), model.Int64Val(21), model.BoolVal(true)},
		{model.Int64Val(1), model.Int64Val(22), model.BoolVal(true)},
	})
	c := openMerge(t, t.Context(), left, mergeFailSource{source: right, after: 3}, o, []join.MergeOrder{{}})
	out := columnar.Batch{MaxRows: 1}
	for _, id := range []int64{10, 11, 12} {
		if err := c.Next(t.Context(), &out); err != nil {
			t.Fatalf("extending prefix for %d: %v", id, err)
		}
		if out.Rows() != 1 || out.Columns[0].Int64[0] != id {
			t.Fatalf("semi output = %+v, want %d", out, id)
		}
	}
	if calls != 6 || q.Usage().Files != 1 {
		t.Fatalf("prefix effects=%d, usage=%+v", calls, q.Usage())
	}
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("semi exhaustion evaluated unused right suffix: %v", err)
	}
	released(t, q, dir)
}

func TestMergeReleasesGroupBeforeAdmittingNextGroup(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes, o.PartitionBytes = 0, 256
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 11), pair(3, 12), pair(4, 13)})
	right := newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(2, 21), pair(3, 22), pair(4, 23)})
	c := openMerge(t, t.Context(), left, right, o, []join.MergeOrder{{}})
	assertBag(t, collect(t, c), []string{"10,20", "11,21", "12,22", "13,23"})
	if q.Usage().PeakFiles != 1 {
		t.Fatalf("successive groups retained old files: %+v", q.Usage())
	}
	released(t, q, dir)
}

func TestMergeUnmatchedDrainDoesNotConsumeMatchQuota(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.FullJoin)
	o.MemoryBytes, o.HashBytes, o.MaxDepth = 0, 1, 1
	o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
	o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
	o.Residual = residual(t)
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})
	right := newSource(t, o.RightLayouts, [][]model.Value{
		{model.Int64Val(1), model.Int64Val(20), model.BoolVal(false)},
		{model.Int64Val(1), model.Int64Val(21), model.BoolVal(false)},
		{model.Int64Val(1), model.Int64Val(22), model.BoolVal(false)},
	})
	c := openMerge(t, t.Context(), left, right, o, []join.MergeOrder{{}})
	assertBag(t, collect(t, c), []string{"10,NULL", "NULL,20", "NULL,21", "NULL,22"})
	if q.Usage().PeakFiles != 1 {
		t.Fatalf("unmatched rows allocated match-set files: %+v", q.Usage())
	}
	released(t, q, dir)
}

func mergeFailure(ctx context.Context, left, right storage.BatchCursor, options join.Options) (err error) {
	cursor, err := join.NewMerge(ctx, left, right, options, []join.MergeOrder{{}})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	var batch columnar.Batch
	for {
		if err := cursor.Next(ctx, &batch); err != nil {
			return err
		}
	}
}

func TestMergeCanceledConstructorClosesInputs(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	options := options(t, q, ir.FullJoin)
	left := newSource(t, options.LeftLayouts, [][]model.Value{pair(1, 10)})
	right := newSource(t, options.RightLayouts, [][]model.Value{pair(1, 20)})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cursor, err := join.NewMerge(ctx, left, right, options, []join.MergeOrder{{}})
	if cursor != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor=%v,%v", cursor, err)
	}
	if !left.closed || !right.closed {
		t.Fatal("canceled constructor retained child cursors")
	}
	released(t, q, dir)
}
