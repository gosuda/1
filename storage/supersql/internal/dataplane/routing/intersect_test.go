package routing_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestIntersectNormalizesCoverage(t *testing.T) {
	point := storage.KeySpan{Lower: bound(true, 1), Upper: bound(true, 1)}
	cases := []struct {
		name        string
		left, right []storage.KeySpan
		want        []storage.KeySpan
	}{
		{name: "both empty"},
		{name: "left empty", right: []storage.KeySpan{full()}},
		{name: "right empty", left: []storage.KeySpan{full()}},
		{name: "full", left: []storage.KeySpan{full()}, right: []storage.KeySpan{full()}, want: []storage.KeySpan{full()}},
		{name: "disjoint", left: []storage.KeySpan{{Lower: full().Lower, Upper: bound(false, 1)}}, right: []storage.KeySpan{{Lower: bound(false, 1), Upper: full().Upper}}},
		{name: "duplicate prefixes", left: []storage.KeySpan{point, point}, right: []storage.KeySpan{full(), point}, want: []storage.KeySpan{point}},
		{
			name: "unsorted overlapping unions",
			left: []storage.KeySpan{
				{Lower: bound(true, 6), Upper: bound(true, 9)},
				{Lower: bound(true, 1), Upper: bound(true, 4)},
				{Lower: bound(false, 3), Upper: bound(false, 8)},
			},
			right: []storage.KeySpan{
				{Lower: bound(true, 7), Upper: bound(true, 10)},
				{Lower: bound(false, 0), Upper: bound(false, 2)},
			},
			want: []storage.KeySpan{
				{Lower: bound(true, 1), Upper: bound(false, 2)},
				{Lower: bound(true, 7), Upper: bound(true, 9)},
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := routing.Intersect(t.Context(), test.left, test.right, spec().KeyLayout, encoder(t), routing.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("intersection = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestIntersectEqualityCuts(t *testing.T) {
	for _, upperInclusive := range []bool{false, true} {
		for _, lowerInclusive := range []bool{false, true} {
			t.Run(fmt.Sprintf("upper_%t_lower_%t", upperInclusive, lowerInclusive), func(t *testing.T) {
				left := storage.KeySpan{Lower: full().Lower, Upper: bound(upperInclusive, 1)}
				right := storage.KeySpan{Lower: bound(lowerInclusive, 1), Upper: full().Upper}
				got, err := routing.Intersect(t.Context(), []storage.KeySpan{left}, []storage.KeySpan{right}, spec().KeyLayout, encoder(t), routing.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				var want []storage.KeySpan
				if upperInclusive && lowerInclusive {
					want = []storage.KeySpan{{Lower: bound(true, 1), Upper: bound(true, 1)}}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("equality intersection = %+v, want %+v", got, want)
				}
				got, err = routing.Intersect(t.Context(), []storage.KeySpan{right, left}, []storage.KeySpan{full()}, spec().KeyLayout, encoder(t), routing.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				want = []storage.KeySpan{full()}
				if !upperInclusive && !lowerInclusive {
					want = []storage.KeySpan{left, right}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("touching union = %+v, want %+v", got, want)
				}
			})
		}
	}
}

func TestIntersectCompositePrefixCoverage(t *testing.T) {
	values := []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(math.MinInt64), model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1), model.Int64Val(math.MaxInt64)}
	for mode := range 4 {
		descending, nullsFirst := mode&1 != 0, mode&2 != 0
		t.Run(fmt.Sprintf("descending_%t_nulls_first_%t", descending, nullsFirst), func(t *testing.T) {
			layout := spec().KeyLayout
			layout[0].Descending, layout[1].Descending = descending, !descending
			layout[0].NullsFirst, layout[1].NullsFirst = nullsFirst, !nullsFirst
			checkPrefixIntersections(t, values, layout)
		})
	}
}

func checkPrefixIntersections(t *testing.T, values []model.Value, layout []storage.KeyColumn) {
	t.Helper()
	codec := encoder(t)
	requests := prefixRequests(values)
	for _, left := range requests {
		for _, right := range requests {
			got, err := routing.Intersect(t.Context(), left, right, layout, codec, routing.DefaultLimits())
			if err != nil {
				t.Fatalf("intersect %+v and %+v: %v", left, right, err)
			}
			checkIntersectionRows(t, values, layout, left, right, got)
		}
	}
}

func checkIntersectionRows(t *testing.T, values []model.Value, layout []storage.KeyColumn, left, right, got []storage.KeySpan) {
	t.Helper()
	for _, tenant := range values {
		for _, id := range values {
			row := []model.Value{tenant, id}
			want := 0
			if unionContains(row, left, layout) && unionContains(row, right, layout) {
				want = 1
			}
			seen := intersectionCoverage(row, got, layout)
			if seen != want {
				t.Fatalf("row %+v covered %d times, want %d; left %+v right %+v result %+v", row, seen, want, left, right, got)
			}
		}
	}
}

func intersectionCoverage(row []model.Value, spans []storage.KeySpan, layout []storage.KeyColumn) int {
	seen := 0
	for _, span := range spans {
		if contains(row, span, layout) {
			seen++
		}
	}
	return seen
}

func unionContains(row []model.Value, spans []storage.KeySpan, layout []storage.KeyColumn) bool {
	for _, span := range spans {
		if contains(row, span, layout) {
			return true
		}
	}
	return false
}

func TestIntersectNumericEqualityClasses(t *testing.T) {
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{
			{ColumnID: 1, Type: model.Type{ID: 1700, Storage: model.TypeNumeric}, Descending: descending},
			{ColumnID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		}
		for _, texts := range [][2]string{{"1.00", "1e0"}, {"-0", "0.000"}, {"-Infinity", "-inf"}, {"NaN", "nan"}, {"1e32767", "10e32766"}} {
			prefix := storage.KeyBound{Inclusive: true, Values: []model.Value{{Type: model.TypeNumeric, StrVal: texts[0]}}}
			row := storage.KeyBound{Inclusive: true, Values: []model.Value{{Type: model.TypeNumeric, StrVal: texts[1]}, model.Int64Val(math.MaxInt64)}}
			left := []storage.KeySpan{{Lower: prefix, Upper: prefix}}
			right := []storage.KeySpan{{Lower: row, Upper: row}}
			got, err := routing.Intersect(t.Context(), left, right, layout, encoder(t), routing.DefaultLimits())
			if err != nil || !reflect.DeepEqual(got, right) {
				t.Fatalf("numeric equality %q, descending %t: %+v, %v; want %+v", texts, descending, got, err, right)
			}
		}
	}
}

func TestIntersectOwnsEveryBound(t *testing.T) {
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	endpoint := storage.KeyBound{Inclusive: true, Values: []model.Value{model.BytesVal([]byte("m"))}}
	left := []storage.KeySpan{{Lower: endpoint, Upper: endpoint}}
	right := []storage.KeySpan{{Lower: endpoint, Upper: endpoint}}
	got, err := routing.Intersect(t.Context(), left, right, layout, encoder(t), routing.DefaultLimits())
	if err != nil || len(got) != 1 {
		t.Fatalf("intersection = %+v, %v", got, err)
	}
	endpoint.Values[0].BytesVal[0] = 'z'
	left[0].Lower.Inclusive = false
	right[0].Upper.Unbounded = true
	layout[0].Descending = true
	if string(got[0].Lower.Values[0].BytesVal) != "m" || string(got[0].Upper.Values[0].BytesVal) != "m" || !got[0].Lower.Inclusive || got[0].Upper.Unbounded {
		t.Fatalf("caller mutation changed intersection: %+v", got)
	}
	got[0].Lower.Values[0].BytesVal[0] = 'a'
	if string(got[0].Upper.Values[0].BytesVal) != "m" || string(endpoint.Values[0].BytesVal) != "z" {
		t.Fatal("result bounds share payloads with one another or the caller")
	}
}

func TestIntersectValidatesEmptyOppositeInput(t *testing.T) {
	invalid := []storage.KeySpan{
		{Lower: bound(false, 1), Upper: bound(true, 1)},
		{Lower: bound(true, 2), Upper: bound(true, 1)},
		{Lower: storage.KeyBound{}, Upper: full().Upper},
		{Lower: storage.KeyBound{Unbounded: true, Inclusive: true}, Upper: full().Upper},
		{Lower: bound(true, 1), Upper: storage.KeyBound{Unbounded: true, Values: []model.Value{model.Int64Val(1)}}},
		{Lower: bound(true, 1, 2, 3), Upper: full().Upper},
		{Lower: storage.KeyBound{Values: []model.Value{model.StringVal("wrong type")}}, Upper: full().Upper},
	}
	for _, span := range invalid {
		for _, swap := range []bool{false, true} {
			left, right := []storage.KeySpan{full(), span}, []storage.KeySpan(nil)
			if swap {
				left, right = right, left
			}
			got, err := routing.Intersect(t.Context(), left, right, spec().KeyLayout, encoder(t), routing.DefaultLimits())
			if got != nil || !errors.Is(err, storage.ErrRangeInvalid) {
				t.Fatalf("invalid span %+v, swap %t: %+v, %v", span, swap, got, err)
			}
		}
	}
	for _, mutate := range []func([]storage.KeyColumn){
		func(layout []storage.KeyColumn) { layout[1].ColumnID = 0 },
		func(layout []storage.KeyColumn) { layout[1].ColumnID = layout[0].ColumnID },
		func(layout []storage.KeyColumn) { layout[1].Type.Collation = 999 },
	} {
		layout := spec().KeyLayout
		mutate(layout)
		got, err := routing.Intersect(t.Context(), nil, nil, layout, encoder(t), routing.DefaultLimits())
		if got != nil || err == nil {
			t.Fatalf("invalid unused layout %+v: %+v, %v", layout, got, err)
		}
	}
}

func TestIntersectResourceFailuresReturnNoOutput(t *testing.T) {
	left := []storage.KeySpan{{Lower: bound(true, 0), Upper: bound(true, 4)}, {Lower: bound(true, 6), Upper: bound(true, 10)}}
	right := []storage.KeySpan{{Lower: bound(true, 1), Upper: bound(true, 2)}, {Lower: bound(true, 3), Upper: bound(true, 7)}}
	for _, test := range []struct {
		name   string
		change func(*routing.Limits)
	}{
		{"input spans", func(l *routing.Limits) { l.MaxSpans = 1 }},
		{"output spans", func(l *routing.Limits) { l.MaxSpans = 2 }},
		{"work", func(l *routing.Limits) { l.MaxWork = 1 }},
		{"bytes", func(l *routing.Limits) { l.MaxBytes = 1 }},
		{"nodes", func(l *routing.Limits) { l.MaxNodes = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := routing.DefaultLimits()
			test.change(&limits)
			got, err := routing.Intersect(t.Context(), left, right, spec().KeyLayout, encoder(t), limits)
			if got != nil || !errors.Is(err, routing.ErrLimit) {
				t.Fatalf("limited intersection = %+v, %v", got, err)
			}
		})
	}
}

func TestIntersectCancellationAndEncoderFailuresReturnNoOutput(t *testing.T) {
	codec := &controlledEncoder{KeyEncoder: encoder(t), fail: true}
	got, err := routing.Intersect(t.Context(), nil, nil, spec().KeyLayout, codec, routing.DefaultLimits())
	if got != nil || !errors.Is(err, errEncoder) {
		t.Fatalf("encoder failure = %+v, %v", got, err)
	}
	codec.fail = false
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	codec.cancel = cancel
	got, err = routing.Intersect(ctx, []storage.KeySpan{full()}, []storage.KeySpan{full()}, spec().KeyLayout, codec, routing.DefaultLimits())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-call cancellation = %+v, %v", got, err)
	}
	got, err = routing.Intersect(ctx, nil, nil, spec().KeyLayout, encoder(t), routing.DefaultLimits())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled empty intersection = %+v, %v", got, err)
	}
}
