package join_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type lookupFactoryFunc func(context.Context, []model.Value) (storage.BatchCursor, error)

func (f lookupFactoryFunc) Open(ctx context.Context, row []model.Value) (storage.BatchCursor, error) {
	return f(ctx, row)
}

type lookupOwner struct {
	storage.BatchCursor
	closes   int
	closeErr error
}

func (c *lookupOwner) Close() error {
	c.closes++
	return errors.Join(c.BatchCursor.Close(), c.closeErr)
}

func openLookup(t *testing.T, ctx context.Context, left storage.BatchCursor, factory join.LookupFactory, o join.Options) storage.BatchCursor {
	t.Helper()
	c, err := join.NewLookup(ctx, left, factory, o)
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

func indexedLookup(t *testing.T, layouts []model.TypeLayout, rows map[int64][][]model.Value) join.LookupFactory {
	t.Helper()
	return lookupFactoryFunc(func(_ context.Context, left []model.Value) (storage.BatchCursor, error) {
		return newSource(t, layouts, rows[left[0].IntVal]), nil
	})
}

func TestLookupPreservesDuplicateBagsWithResiduals(t *testing.T) {
	cases := []struct {
		kind ir.JoinKind
		want []string
	}{
		{ir.InnerJoin, []string{"10,20", "10,20", "11,20", "11,20"}},
		{ir.LeftJoin, []string{"10,20", "10,20", "11,20", "11,20", "12,NULL", "13,NULL"}},
		{ir.SemiJoin, []string{"10", "11"}},
		{ir.AntiJoin, []string{"12", "13"}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.kind), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, tc.kind)
			o.MemoryBytes, o.HashBytes, o.PartitionBytes, o.MaxPartitions, o.MaxDepth = 0, 0, 0, 0, 0
			o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
			o.Residual, o.ResidualInputs = residual(t), []join.Column{{Side: join.Right, Index: 2}}
			indexed := map[int64][][]model.Value{
				1: {{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)}, {model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)}, {model.Int64Val(1), model.Int64Val(21), model.BoolVal(false)}},
				2: {{model.Int64Val(2), model.Int64Val(22), model.NullVal(model.TypeBoolean)}},
			}
			left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12), pair(3, 13)})
			c := openLookup(t, t.Context(), left, indexedLookup(t, o.RightLayouts, indexed), o)
			assertBag(t, collect(t, c), tc.want)
			released(t, q, dir)
		})
	}
}

func TestLookupNullKeysSkipOrOpenProbe(t *testing.T) {
	for _, equal := range []bool{false, true} {
		t.Run(fmt.Sprint(equal), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.LeftJoin)
			o.Keys[0].NullEqual = equal
			opens := 0
			factory := lookupFactoryFunc(func(_ context.Context, row []model.Value) (storage.BatchCursor, error) {
				opens++
				if !row[0].IsNull || row[1].IntVal != 10 {
					t.Fatalf("probe received incomplete left row: %v", row)
				}
				return newSource(t, o.RightLayouts, [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(20)}}), nil
			})
			left := newSource(t, o.LeftLayouts, [][]model.Value{{model.NullVal(model.TypeInt64), model.Int64Val(10)}})
			c := openLookup(t, t.Context(), left, factory, o)
			want, wantOpens := []string{"10,NULL"}, 0
			if equal {
				want, wantOpens = []string{"10,20"}, 1
			}
			assertBag(t, collect(t, c), want)
			if opens != wantOpens {
				t.Fatalf("opened %d probes, want %d", opens, wantOpens)
			}
			released(t, q, dir)
		})
	}
}

func TestLookupEmptyLeftNeverOpensProbe(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, nil)}
	factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) {
		t.Fatal("empty left opened a probe")
		return nil, nil
	})
	c := openLookup(t, t.Context(), left, factory, o)
	if rows := collect(t, c); len(rows) != 0 || left.closes != 1 {
		t.Fatalf("empty lookup rows=%v left closes=%d", rows, left.closes)
	}
	released(t, q, dir)
}

var (
	errLookupLater      = errors.New("later lookup failure")
	errLookupLeftClose  = errors.New("lookup left close failure")
	errLookupRightClose = errors.New("lookup right close failure")
)

func TestLookupFirstDemandPrecedesLaterFactoryFailure(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 11)})}
	var probe *lookupOwner
	factory := lookupFactoryFunc(func(_ context.Context, row []model.Value) (storage.BatchCursor, error) {
		if row[0].IntVal == 2 {
			return nil, errLookupLater
		}
		probe = &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20)})}
		return probe, nil
	})
	c := openLookup(t, t.Context(), left, factory, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 10 || out.Columns[1].Int64[0] != 20 {
		t.Fatalf("first demanded pair = %+v", out)
	}
	if err := c.Next(t.Context(), &out); !errors.Is(err, errLookupLater) {
		t.Fatalf("second demand = %v", err)
	}
	if left.closes != 1 || probe.closes != 1 {
		t.Fatalf("left closes=%d probe closes=%d", left.closes, probe.closes)
	}
	released(t, q, dir)
}

func TestLookupFactoryCursorWithErrorJoinsCleanup(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)}), closeErr: errLookupLeftClose}
	right := &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, nil), closeErr: errLookupRightClose}
	factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) { return right, errLookupLater })
	c, err := join.NewLookup(t.Context(), left, factory, o)
	if err != nil {
		t.Fatal(err)
	}
	var out columnar.Batch
	err = c.Next(t.Context(), &out)
	for _, want := range []error{errLookupLater, errLookupLeftClose, errLookupRightClose} {
		if !errors.Is(err, want) {
			t.Errorf("lookup error %v does not retain %v", err, want)
		}
	}
	if err := c.Close(); !errors.Is(err, errLookupLeftClose) {
		t.Errorf("repeated Close = %v", err)
	}
	if left.closes != 1 || right.closes != 1 {
		t.Fatalf("left closes=%d right closes=%d", left.closes, right.closes)
	}
	released(t, q, dir)
}

func TestLookupSemiAntiCloseProbeBeforeNextLeft(t *testing.T) {
	for _, kind := range []ir.JoinKind{ir.SemiJoin, ir.AntiJoin} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, kind)
			var previous *lookupOwner
			factory := lookupFactoryFunc(func(_ context.Context, row []model.Value) (storage.BatchCursor, error) {
				if previous != nil && previous.closes != 1 {
					t.Fatal("opened overlapping probes")
				}
				s := newSource(t, o.RightLayouts, [][]model.Value{pair(row[0].IntVal, 20)})
				s.empty = false
				previous = &lookupOwner{BatchCursor: mergeFailSource{source: s, after: 1}}
				return previous, nil
			})
			left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(2, 11)})
			c := openLookup(t, t.Context(), left, factory, o)
			var want []string
			if kind == ir.SemiJoin {
				want = []string{"10", "11"}
			}
			assertBag(t, collect(t, c), want)
			if previous.closes != 1 {
				t.Fatalf("last probe closes=%d", previous.closes)
			}
			released(t, q, dir)
		})
	}
}

func TestLookupNestedOutputOwnsValuesAndLayouts(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	array := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	composite := model.TypeLayout{Type: model.Type{ID: 9001, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, array}}
	payload := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal(strings.Repeat("p", 600)), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -1}}, ArrayVal: []model.Value{model.Int64Val(9), model.NullVal(model.TypeInt64)}}}}
	o := options(t, q, ir.LeftJoin)
	o.RightLayouts, o.OutputLayouts = []model.TypeLayout{integer, composite}, []model.TypeLayout{integer, composite}
	o.BatchBytes = 16 << 10
	factory := indexedLookup(t, o.RightLayouts, map[int64][][]model.Value{1: {{model.Int64Val(1), payload}, {model.Int64Val(1), payload}}})
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11), pair(2, 12)})
	c := openLookup(t, t.Context(), left, factory, o)
	// Top-level metadata is mutable after construction; factory layouts remain its own.
	o.Output[1] = join.Column{Side: join.Left, Index: 0}
	rows := collect(t, c)
	if len(rows) != 5 {
		t.Fatalf("nested output rows=%d, want 5", len(rows))
	}
	for _, row := range rows {
		if row[0].IntVal == 12 {
			if !row[1].IsNull || row[1].Type != model.TypeComposite {
				t.Fatalf("outer extension=%#v", row[1])
			}
		} else if !reflect.DeepEqual(row[1], payload) {
			t.Fatalf("nested payload=%#v, want %#v", row[1], payload)
		}
	}
	released(t, q, dir)
}

func TestLookupByteLimitRetainsPendingPair(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.RightLayouts, o.OutputLayouts = []model.TypeLayout{integer, text}, []model.TypeLayout{integer, text}
	o.BatchBytes = 1600
	a, b := strings.Repeat("a", 600), strings.Repeat("b", 600)
	factory := indexedLookup(t, o.RightLayouts, map[int64][][]model.Value{1: {{model.Int64Val(1), model.StringVal(a)}, {model.Int64Val(1), model.StringVal(b)}}})
	left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)})
	c := openLookup(t, t.Context(), left, factory, o)
	var got []string
	for _, row := range collect(t, c) {
		got = append(got, fmt.Sprintf("%d:%s", row[0].IntVal, row[1].StrVal))
	}
	slices.Sort(got)
	want := []string{"10:" + a, "10:" + b, "11:" + a, "11:" + b}
	if !slices.Equal(got, want) {
		t.Fatalf("pending pairs=%v, want %v", got, want)
	}
	released(t, q, dir)
}

func TestLookupResidualFailureClosesActiveOwners(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
	o.ResidualInputs = []join.Column{{Side: join.Right, Index: 2}}
	o.Residual = effectPredicate(t, func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return model.Value{}, errLookupLater
	})
	left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})}
	right := &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, [][]model.Value{{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)}})}
	factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) { return right, nil })
	c := openLookup(t, t.Context(), left, factory, o)
	var out columnar.Batch
	if err := c.Next(t.Context(), &out); !errors.Is(err, errLookupLater) {
		t.Fatalf("residual error=%v", err)
	}
	if left.closes != 1 || right.closes != 1 {
		t.Fatalf("left closes=%d right closes=%d", left.closes, right.closes)
	}
	released(t, q, dir)
}

func TestLookupCancellationAndEarlyCloseReleaseProbe(t *testing.T) {
	for _, cancelNext := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelNext), func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})}
			right := &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20), pair(1, 21)})}
			factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) { return right, nil })
			c := openLookup(t, t.Context(), left, factory, o)
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if cancelNext {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := c.Next(ctx, &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled demand=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if left.closes != 1 || right.closes != 1 {
				t.Fatalf("left closes=%d right closes=%d", left.closes, right.closes)
			}
			released(t, q, dir)
		})
	}
}

type lookupSelectedSource struct{ *source }

func (s lookupSelectedSource) Next(ctx context.Context, out *columnar.Batch) error {
	request := out.MaxRows
	out.MaxRows = 3
	if err := s.source.Next(ctx, out); err != nil {
		out.MaxRows = request
		return err
	}
	out.MaxRows = request
	if out.Length == 3 {
		out.Selection = []uint32{2}
	}
	return nil
}

func TestLookupAcceptsNativePhysicalSelectionBeyondDemand(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left := lookupSelectedSource{newSource(t, o.LeftLayouts, [][]model.Value{pair(2, 98), pair(3, 99), pair(1, 10)})}
	factory := lookupFactoryFunc(func(_ context.Context, row []model.Value) (storage.BatchCursor, error) {
		if row[0].IntVal != 1 {
			t.Fatalf("inactive left lane opened probe: %v", row)
		}
		return lookupSelectedSource{newSource(t, o.RightLayouts, [][]model.Value{pair(1, 98), pair(1, 99), pair(1, 20)})}, nil
	})
	c := openLookup(t, t.Context(), left, factory, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[0].Int64[0] != 10 || out.Columns[1].Int64[0] != 20 {
		t.Fatalf("selected pair=%+v", out)
	}
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("selected EOF=%v", err)
	}
	released(t, q, dir)
}

func TestLookupResourceFailuresReleaseLeftAndProbe(t *testing.T) {
	for _, scenario := range []string{"work", "record", "output"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			rightRows := [][]model.Value{pair(1, 20)}
			switch scenario {
			case "work":
				o.WorkBytes = 1
			case "output":
				o.BatchBytes = 1
			case "record":
				o.RightLayouts, o.OutputLayouts = []model.TypeLayout{integer, text}, []model.TypeLayout{integer, text}
				rightRows = [][]model.Value{{model.Int64Val(1), model.StringVal(strings.Repeat("x", 8192))}}
			}
			left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})}
			var probe *lookupOwner
			factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) {
				probe = &lookupOwner{BatchCursor: newSource(t, o.RightLayouts, rightRows)}
				return probe, nil
			})
			c, err := join.NewLookup(t.Context(), left, factory, o)
			if err == nil {
				var out columnar.Batch
				err = c.Next(t.Context(), &out)
				err = errors.Join(err, c.Close())
			}
			if !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("%s error=%v, want budget", scenario, err)
			}
			if left.closes != 1 || (probe != nil && probe.closes != 1) {
				t.Fatalf("resource failure retained owners: left=%d probe=%+v", left.closes, probe)
			}
			released(t, q, dir)
		})
	}
}

type lookupBatchFunc func(context.Context, *columnar.Batch) error

func (f lookupBatchFunc) Next(ctx context.Context, out *columnar.Batch) error { return f(ctx, out) }
func (lookupBatchFunc) Close() error                                          { return nil }

func TestLookupRejectsInvalidProbeBatches(t *testing.T) {
	for _, scenario := range []string{"layout", "active-demand", "selection"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})}
			right := &lookupOwner{BatchCursor: lookupBatchFunc(func(_ context.Context, out *columnar.Batch) error {
				out.Length = 2
				out.Columns = []columnar.Vector{{Type: integer.Type, Length: 2, Int64: []int64{1, 1}}, {Type: integer.Type, Length: 2, Int64: []int64{20, 21}}}
				out.Selection = []uint32{0}
				switch scenario {
				case "layout":
					out.Columns[1].Type = boolean.Type
					out.Columns[1].Int64 = []int64{0, 1}
				case "active-demand":
					out.MaxRows = 2
					out.Selection = nil
				case "selection":
					out.Selection[0] = 2
				}
				return nil
			})}
			factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) { return right, nil })
			c := openLookup(t, t.Context(), left, factory, o)
			out := columnar.Batch{MaxRows: 1}
			err := c.Next(t.Context(), &out)
			want := columnar.ErrInvalidBatch
			if scenario == "layout" {
				want = columnar.ErrValueLayout
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s probe error=%v, want %v", scenario, err, want)
			}
			if left.closes != 1 || right.closes != 1 {
				t.Fatalf("left closes=%d right closes=%d", left.closes, right.closes)
			}
			released(t, q, dir)
		})
	}
}

func TestLookupRejectsUnsupportedContractsWithoutConsumption(t *testing.T) {
	for _, scenario := range []string{"right", "full", "mark", "cross", "no-keys", "key-layout", "unsupported-key", "right-semi-output", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := options(t, q, ir.InnerJoin)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "right":
				o.Kind = ir.RightJoin
			case "full":
				o.Kind = ir.FullJoin
			case "mark":
				o.Kind, o.MarkKind = ir.MarkJoin, ir.ExistsSubquery
			case "cross":
				o.Kind = ir.CrossJoin
			case "no-keys":
				o.Keys = nil
			case "key-layout":
				o.Keys[0].Right = 4
			case "unsupported-key":
				o.LeftLayouts[0], o.RightLayouts[0] = model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeInt64}}, model.TypeLayout{Type: model.Type{ID: 1082, Storage: model.TypeInt64}}
			case "right-semi-output":
				o.Kind = ir.SemiJoin
			case "canceled":
				cancel()
			}
			if err := join.CheckLookupSupport(ctx, o); err == nil {
				t.Fatal("unsupported lookup accepted")
			}
			left := &lookupOwner{BatchCursor: lookupBatchFunc(func(context.Context, *columnar.Batch) error {
				t.Fatal("invalid constructor consumed left")
				return nil
			})}
			factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) {
				t.Fatal("invalid constructor opened probe")
				return nil, nil
			})
			c, err := join.NewLookup(ctx, left, factory, o)
			if err == nil {
				if closeErr := c.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("unsupported constructor accepted")
			}
			if left.closes != 1 {
				t.Fatalf("constructor left closes=%d", left.closes)
			}
			released(t, q, dir)
		})
	}
}

func TestLookupFirstDemandPrecedesLaterProbeFailure(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := options(t, q, ir.InnerJoin)
	left := &lookupOwner{BatchCursor: newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})}
	s := newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20)})
	s.empty = false
	right := &lookupOwner{BatchCursor: mergeFailSource{source: s, after: 1}}
	factory := lookupFactoryFunc(func(context.Context, []model.Value) (storage.BatchCursor, error) { return right, nil })
	c := openLookup(t, t.Context(), left, factory, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || out.Columns[1].Int64[0] != 20 {
		t.Fatalf("first probe pair=%+v", out)
	}
	if err := c.Next(t.Context(), &out); !errors.Is(err, errMergeLater) {
		t.Fatalf("later probe error=%v", err)
	}
	if left.closes != 1 || right.closes != 1 {
		t.Fatalf("left closes=%d right closes=%d", left.closes, right.closes)
	}
	released(t, q, dir)
}
