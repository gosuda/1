package aggregate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestCallOrdersUseIndependentNullAndDirectionKeys(t *testing.T) {
	layouts := []model.TypeLayout{int8, float8, text, boolean}
	calls := []aggregate.Call{
		{Function: 2111, Arguments: []int{1}, FilterColumn: new(3), OrderBy: []aggregate.OrderKey{{Column: 2, NullsFirst: true}}},
		{Function: 2111, Arguments: []int{1}, FilterColumn: new(3), OrderBy: []aggregate.OrderKey{{Column: 2, Descending: true}}},
		{Function: 2111, Arguments: []int{1}, FilterColumn: new(3)},
	}
	for _, spill := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "spill"}[spill], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			var rows [][]model.Value
			for group := range 20 {
				id := model.Int64Val(int64(group))
				rows = append(rows,
					[]model.Value{id, model.Float64Val(1e16), model.NullVal(model.TypeString), model.BoolVal(true)},
					[]model.Value{id, model.Float64Val(1), model.StringVal("c"), model.BoolVal(true)},
					[]model.Value{id, model.Float64Val(-1e16), model.StringVal("b"), model.BoolVal(true)},
					[]model.Value{id, model.Float64Val(100), model.StringVal("a"), model.BoolVal(false)},
				)
			}
			rows = append(rows, []model.Value{model.Int64Val(20), model.Float64Val(7), model.StringVal("b"), model.NullVal(model.TypeBoolean)})
			o := options(t, q, layouts, []model.TypeLayout{int8, float8, float8, float8}, []int{0}, calls)
			o.PartitionMemoryBytes = 1 << 20
			if spill {
				o.PartitionMemoryBytes = 1024
			}
			s := newSource(t, layouts, rows)
			got, err := drain(t, open(t, s, o))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 21 {
				t.Fatalf("groups=%d, want 21", len(got))
			}
			for _, row := range got {
				if row[0].IntVal == 20 {
					if !row[1].IsNull || !row[2].IsNull || !row[3].IsNull {
						t.Fatalf("filtered group=%v, want NULL sums", row)
					}
					continue
				}
				first := !row[1].IsNull && row[1].FloatVal == 1
				second := !row[2].IsNull && row[2].FloatVal == 0
				third := !row[3].IsNull && row[3].FloatVal == 0
				if !first || !second || !third {
					t.Fatalf("independent ordered sums=%v, want 1, 0, 0", row)
				}
			}
			if spill != (q.Usage().Spill.Peak != 0) {
				t.Fatalf("spill=%v, peak=%d", spill, q.Usage().Spill.Peak)
			}
			if !s.closed {
				t.Fatal("source retained after EOF")
			}
			released(t, q, dir)
		})
	}
}

func TestDistinctOrderingPreservesSourceTiesAndSiblingCalls(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{float8, int8, boolean}
	rows := [][]model.Value{
		{model.Float64Val(1e16), model.Int64Val(0), model.BoolVal(true)},
		{model.Float64Val(-1e16), model.Int64Val(0), model.BoolVal(true)},
		{model.Float64Val(1), model.Int64Val(0), model.BoolVal(false)},
		{model.Float64Val(1), model.Int64Val(0), model.BoolVal(true)},
		{model.Float64Val(1), model.Int64Val(0), model.BoolVal(true)},
	}
	calls := []aggregate.Call{
		{Function: 2111, Arguments: []int{0}, Distinct: true, FilterColumn: new(2), OrderBy: []aggregate.OrderKey{{Column: 0}}},
		{Function: 2147, Arguments: []int{0}, Distinct: true, FilterColumn: new(2), OrderBy: []aggregate.OrderKey{{Column: 0, Descending: true}}},
		{Function: 2111, Arguments: []int{0}, FilterColumn: new(2), OrderBy: []aggregate.OrderKey{{Column: 1}, {Column: 1, Descending: true}}},
		{Function: 2111, Arguments: []int{0}, FilterColumn: new(2)},
	}
	o := options(t, q, layouts, []model.TypeLayout{float8, int8, float8, float8}, nil, calls)
	o.PartitionMemoryBytes = 1024
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("groups=%d, want one", len(got))
	}
	distinctSum := !got[0][0].IsNull && got[0][0].FloatVal == 0
	siblings := got[0][2].FloatVal == 2 && got[0][3].FloatVal == 2
	if !distinctSum || got[0][1].IntVal != 3 || !siblings {
		t.Fatalf("DISTINCT and stable siblings=%v, want 0, 3, 2, 2", got)
	}
	released(t, q, dir)
}

func TestOrderedSpillCloseAndCancellationReleaseResources(t *testing.T) {
	for _, cancelQuery := range []bool{false, true} {
		t.Run(map[bool]string{false: "early close", true: "cancel"}[cancelQuery], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var rows [][]model.Value
			for i := range 40 {
				rows = append(rows, []model.Value{model.Int64Val(int64(i)), model.Int64Val(1)})
			}
			layouts := []model.TypeLayout{int8, int4}
			s := newSource(t, layouts, rows)
			o := options(t, q, layouts, []model.TypeLayout{int8, int8}, []int{0}, []aggregate.Call{{Function: 2108, Arguments: []int{1}, OrderBy: []aggregate.OrderKey{{Column: 1}}}})
			o.PartitionMemoryBytes = 1024
			c, err := aggregate.New(ctx, s, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Close(); err != nil {
					t.Error(err)
				}
			})
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Rows() != 1 || out.Columns[1].Int64[0] != 1 || q.Usage().Spill.Live == 0 {
				t.Fatalf("first result=%v, usage=%+v", out, q.Usage())
			}
			if cancelQuery {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled ordered aggregate=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !s.closed {
				t.Fatal("source remains open")
			}
			released(t, q, dir)
		})
	}
}

func TestOrderKeysValidateBeforePullingSource(t *testing.T) {
	for _, badColumn := range []int{-1, 2, 1} {
		q, dir := query(t, false, 32<<20)
		layouts := []model.TypeLayout{int4, {Type: model.Type{ID: 23, Storage: model.TypeString}}}
		s := newSource(t, []model.TypeLayout{int4}, nil)
		o := options(t, q, layouts, []model.TypeLayout{int8}, nil, []aggregate.Call{{Function: 2108, Arguments: []int{0}, OrderBy: []aggregate.OrderKey{{Column: badColumn}}}})
		_, err := aggregate.New(t.Context(), s, o)
		if badColumn == 1 {
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "42804" {
				t.Fatalf("bad order type: %v", err)
			}
		} else if !errors.Is(err, columnar.ErrValueLayout) {
			t.Fatalf("bad order column %d: %v", badColumn, err)
		}
		if s.pulls != 0 || !s.closed {
			t.Fatalf("constructor ownership: pulls=%d closed=%v", s.pulls, s.closed)
		}
		released(t, q, dir)
	}
}

func TestOrderedSpillFailureReleasesResources(t *testing.T) {
	q, dir := query(t, false, 64)
	layouts := []model.TypeLayout{int4}
	s := newSource(t, layouts, [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}})
	o := options(t, q, layouts, []model.TypeLayout{int8}, nil, []aggregate.Call{{Function: 2108, Arguments: []int{0}, OrderBy: []aggregate.OrderKey{{Column: 0}}}})
	o.PartitionMemoryBytes = 128
	_, err := drain(t, open(t, s, o))
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("spill quota error=%v", err)
	}
	if !s.closed {
		t.Fatal("failed ordered aggregate retained input")
	}
	released(t, q, dir)
}

func TestOrderMetadataIsOwnedAfterConstruction(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{float8, int8}
	rows := [][]model.Value{{model.Float64Val(1e16), model.Int64Val(0)}, {model.Float64Val(1), model.Int64Val(2)}, {model.Float64Val(-1e16), model.Int64Val(1)}}
	order := []aggregate.OrderKey{{Column: 1}}
	o := options(t, q, layouts, []model.TypeLayout{float8}, nil, []aggregate.Call{{Function: 2111, Arguments: []int{0}, OrderBy: order}})
	c := open(t, newSource(t, layouts, rows), o)
	order[0] = aggregate.OrderKey{Column: -1}
	got, err := drain(t, c)
	if err != nil || len(got) != 1 || got[0][0].FloatVal != 1 {
		t.Fatalf("owned ORDER BY result=%v error=%v", got, err)
	}
	released(t, q, dir)
}

var errOrderedSource = errors.New("ordered aggregate source failure")
var errOrderedSourceClose = errors.New("ordered aggregate source close failure")

type failingOrderedSource struct{ *source }

func (s *failingOrderedSource) Next(ctx context.Context, out *columnar.Batch) error {
	if s.index >= 32 {
		return errOrderedSource
	}
	return s.source.Next(ctx, out)
}

func (s *failingOrderedSource) Close() error {
	return errors.Join(s.source.Close(), errOrderedSourceClose)
}

func TestOrderedSourceFailureJoinsCleanupAfterSpill(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	rows := make([][]model.Value, 40)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i))}
	}
	layouts := []model.TypeLayout{int4}
	s := &failingOrderedSource{newSource(t, layouts, rows)}
	o := options(t, q, layouts, []model.TypeLayout{int8}, nil, []aggregate.Call{{Function: 2108, Arguments: []int{0}, OrderBy: []aggregate.OrderKey{{Column: 0}}}})
	o.PartitionMemoryBytes = 128
	_, err := drain(t, open(t, s, o))
	if !errors.Is(err, errOrderedSource) || !errors.Is(err, errOrderedSourceClose) {
		t.Fatalf("ordered source failure=%v, want source and cleanup causes", err)
	}
	if q.Usage().Spill.Peak == 0 {
		t.Fatal("source failed before spill was exercised")
	}
	released(t, q, dir)
}
