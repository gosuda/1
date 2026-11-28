package aggregate_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

var money = model.TypeLayout{Type: model.Type{ID: 790, Storage: model.TypeInt64}}
var fixedBits = model.TypeLayout{Type: model.Type{ID: 1560, Storage: model.TypeBytes, Modifier: -1}}

func sameTypeAccumulator(t *testing.T, q *runtime.Query, function model.FunctionID, layout model.TypeLayout, limit uint64) *aggregate.Accumulator {
	t.Helper()
	o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{layout}, nil, nil)
	a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: function, InputLayouts: o.InputLayouts, OutputLayout: layout, Encoder: o.Encoder, Query: q, MaxRecordBytes: limit, WorkBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func packedBits(length int, fill byte) model.Value {
	raw := make([]byte, 4+(length+7)/8)
	binary.BigEndian.PutUint32(raw, uint32(length))
	for i := 4; i < len(raw); i++ {
		raw[i] = fill
	}
	if length%8 != 0 {
		raw[len(raw)-1] &= byte(255 << (8 - uint(length%8)))
	}
	return model.BytesVal(raw)
}

func requireBits(t *testing.T, got, want model.Value) {
	t.Helper()
	if got.IsNull != want.IsNull || got.Type != model.TypeBytes || !bytes.Equal(got.BytesVal, want.BytesVal) {
		t.Fatalf("bit aggregate = %x (null=%v), want %x (null=%v)", got.BytesVal, got.IsNull, want.BytesVal, want.IsNull)
	}
}

func TestBitAggregatesOwnLongNonByteAlignedState(t *testing.T) {
	for _, tc := range []struct {
		function model.FunctionID
		want     byte
	}{{2242, 0x80}, {2243, 0xe0}, {6167, 0x60}} {
		t.Run(fmt.Sprint(tc.function), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := sameTypeAccumulator(t, q, tc.function, fixedBits, 4096)
			null := model.NullVal(model.TypeBytes)
			got, err := a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, null)
			first := packedBits(8193, 0xa0)
			if err := a.Advance(t.Context(), []model.Value{first}); err != nil {
				t.Fatal(err)
			}
			clear(first.BytesVal)
			got, err = a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, packedBits(8193, 0xa0))
			for _, value := range []model.Value{null, packedBits(8193, 0xc0)} {
				if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
					t.Fatal(err)
				}
			}
			got, err = a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, packedBits(8193, tc.want))
			if err := a.Reset(); err != nil {
				t.Fatal(err)
			}
			got, err = a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, null)
			if err := a.Advance(t.Context(), []model.Value{packedBits(0, 0)}); err != nil {
				t.Fatal(err)
			}
			got, err = a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, packedBits(0, 0))
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestBitAggregateErrorsPreservePreviousState(t *testing.T) {
	for _, function := range []model.FunctionID{2242, 2243, 6167} {
		t.Run(fmt.Sprint(function), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := sameTypeAccumulator(t, q, function, fixedBits, 128)
			for _, tc := range []struct {
				value model.Value
				code  string
			}{{model.BytesVal([]byte{0, 0, 0, 1, 1}), "22P03"}, {packedBits(1024, 0xff), "54000"}} {
				err := a.Advance(t.Context(), []model.Value{tc.value})
				var sql *model.Error
				if !errors.As(err, &sql) || sql.Code != tc.code {
					t.Fatalf("invalid initial bit value error=%v, want %s", err, tc.code)
				}
			}
			want := packedBits(9, 0xa0)
			if err := a.Advance(t.Context(), []model.Value{want}); err != nil {
				t.Fatal(err)
			}
			var sql *model.Error
			if err := a.Advance(t.Context(), []model.Value{packedBits(8, 0xa0)}); !errors.As(err, &sql) || sql.Code != "22026" {
				t.Fatalf("unequal-length transition=%v, want 22026", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := a.Advance(ctx, []model.Value{want}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled transition=%v", err)
			}
			got, err := a.Result(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requireBits(t, got, want)
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestMoneyAggregateRejectsOverflowWithoutWidening(t *testing.T) {
	for _, tc := range []struct{ first, overflow, correction int64 }{{math.MaxInt64, 1, -1}, {math.MinInt64, -1, 1}} {
		t.Run(fmt.Sprint(tc.first), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := sameTypeAccumulator(t, q, 2112, money, 128)
			if err := a.Advance(t.Context(), []model.Value{model.NullVal(model.TypeInt64)}); err != nil {
				t.Fatal(err)
			}
			got, err := a.Result(t.Context())
			if err != nil || !got.IsNull {
				t.Fatalf("all-NULL money sum=%+v, error=%v", got, err)
			}
			if err := a.Advance(t.Context(), []model.Value{model.Int64Val(tc.first)}); err != nil {
				t.Fatal(err)
			}
			var sql *model.Error
			if err := a.Advance(t.Context(), []model.Value{model.Int64Val(tc.overflow)}); !errors.As(err, &sql) || sql.Code != "22003" {
				t.Fatalf("money sum overflow=%v", err)
			}
			if err := a.Advance(t.Context(), []model.Value{model.Int64Val(tc.correction)}); err != nil {
				t.Fatal(err)
			}
			got, err = a.Result(t.Context())
			if err != nil || got.IsNull || got.IntVal != tc.first+tc.correction {
				t.Fatalf("money sum after failed transition=%+v, error=%v", got, err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestMoneyBitAggregatesGroupSpillDistinctOrder(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		t.Run(fmt.Sprintf("distinct=%v", distinct), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			layouts := []model.TypeLayout{int8, money, fixedBits, boolean}
			var input [][]model.Value
			for round := range 5 {
				for id := range 12 {
					amount, fill := int64(100), byte(0xa0)
					if round%2 != 0 {
						amount, fill = 200, 0xc0
					}
					input = append(input, []model.Value{model.Int64Val(int64(id)), model.Int64Val(amount), packedBits(9, fill), model.BoolVal(round != 4)})
				}
			}
			calls := []aggregate.Call{{Function: 2112, Arguments: []int{1}}, {Function: 2242, Arguments: []int{2}}, {Function: 2243, Arguments: []int{2}}, {Function: 6167, Arguments: []int{2}}}
			for i := range calls {
				calls[i].FilterColumn = new(3)
				calls[i].Distinct = distinct
				if distinct {
					calls[i].OrderBy = []aggregate.OrderKey{{Column: calls[i].Arguments[0], Descending: true}}
				}
			}
			o := options(t, q, layouts, []model.TypeLayout{int8, money, fixedBits, fixedBits, fixedBits}, []int{0}, calls)
			o.GroupBytes = 32 << 10
			o.PartitionMemoryBytes = 16384
			rows, err := drain(t, open(t, newSource(t, layouts, input), o))
			if err != nil || len(rows) != 12 {
				t.Fatalf("money/bit groups=%v, error=%v", rows, err)
			}
			wantSum, wantXor := int64(600), byte(0)
			if distinct {
				wantSum, wantXor = 300, 0x60
			}
			seen := make(map[int64]bool)
			for _, row := range rows {
				id := row[0].IntVal
				if id < 0 || id >= 12 || seen[id] || row[1].IsNull || row[1].IntVal != wantSum {
					t.Fatalf("invalid money group=%v", row)
				}
				seen[id] = true
				requireBits(t, row[2], packedBits(9, 0x80))
				requireBits(t, row[3], packedBits(9, 0xe0))
				requireBits(t, row[4], packedBits(9, wantXor))
			}
			if q.Usage().Spill.Peak == 0 {
				t.Fatal("money/bit aggregation did not spill")
			}
			released(t, q, dir)
		})
	}
}

func TestBitAggregateFailureClosesSource(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{fixedBits}
	s := newSource(t, layouts, [][]model.Value{{packedBits(9, 0xff)}, {packedBits(8, 0xff)}})
	o := options(t, q, layouts, layouts, nil, []aggregate.Call{{Function: 2242, Arguments: []int{0}}})
	var sql *model.Error
	if _, err := drain(t, open(t, s, o)); !errors.As(err, &sql) || sql.Code != "22026" {
		t.Fatalf("unequal-length source=%v", err)
	}
	if !s.closed {
		t.Fatal("failed bit aggregate retained input")
	}
	released(t, q, dir)
}

func TestBitAggregateSpillCancellationReleasesResources(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	layouts := []model.TypeLayout{int8, fixedBits}
	var input [][]model.Value
	for id := range 12 {
		input = append(input, []model.Value{model.Int64Val(int64(id)), packedBits(9, 0xa0)})
	}
	s := newSource(t, layouts, input)
	o := options(t, q, layouts, layouts, []int{0}, []aggregate.Call{{Function: 6167, Arguments: []int{1}}})
	o.GroupBytes = 32 << 10
	c, err := aggregate.New(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if q.Usage().Spill.Live == 0 {
		t.Fatal("cancellation case did not retain a spill partition")
	}
	cancel()
	if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled bit aggregate=%v", err)
	}
	if !s.closed {
		t.Fatal("cancelled bit aggregate retained source")
	}
	released(t, q, dir)
}
