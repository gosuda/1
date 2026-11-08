package span_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (f *fixture) timestampPredicate(function model.FunctionID, bound int64) ir.ScalarID {
	typ := f.keys[0].Column.Type
	typ.Modifier = -1
	layout, err := f.b.AddLayout(f.t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		f.t.Fatal(err)
	}
	right := f.add(ir.Scalar{Kind: ir.Const, Layout: layout}, nil, model.Value{Type: model.TypeTimestamp, IntVal: bound})
	effects := ir.Effects{MayError: builtin.ScalarMayError(function)}
	sig, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: f.boolean, Strict: true, Nullable: true}, Parameters: []ir.LayoutID{f.layouts[0], layout}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Call, Function: function, Signature: sig, Strict: true, Nullable: true, Layout: f.boolean, Effects: effects}, []ir.ScalarID{f.slots[0], right}, model.Value{})
}

func TestTimestampSpansPreservePrecisionAndInfinityOrder(t *testing.T) {
	for _, id := range []model.TypeID{1114, 1184} {
		for _, descending := range []bool{false, true} {
			for _, nullsFirst := range []bool{false, true} {
				f := setup(t, model.Type{ID: id, Storage: model.TypeTimestamp, Modifier: 3})
				f.keys[0].Column.Descending = descending
				f.keys[0].Column.NullsFirst = nullsFirst
				lower, upper := model.FunctionID(2056), model.FunctionID(2055)
				if id == 1184 {
					lower, upper = 1156, 1155
				}
				predicate := f.booleanExpr(ir.And, f.timestampPredicate(lower, -1500), f.timestampPredicate(upper, 1500))
				result := extract(t, f.graph(), predicate, f.keys, nil, limits())
				if result.Residual != 0 {
					t.Fatalf("timestamp %d range retained residual", id)
				}
				for _, n := range []int64{math.MinInt64, -2000, -1000, 0, 1000, 2000, math.MaxInt64} {
					got := matches(t, f.keys, result, model.Value{Type: model.TypeTimestamp, IntVal: n})
					if got != (n >= -1500 && n <= 1500) {
						t.Fatalf("timestamp%d descending=%v nullsFirst=%v value=%d matched=%v", id, descending, nullsFirst, n, got)
					}
				}
				if matches(t, f.keys, result, model.NullVal(model.TypeTimestamp)) {
					t.Fatal("strict timestamp range included NULL")
				}
			}
		}
	}
}

func TestTimestampSpanExcludesPositiveInfinityAtStrictBound(t *testing.T) {
	for _, id := range []model.TypeID{1114, 1184} {
		f := setup(t, model.Type{ID: id, Storage: model.TypeTimestamp, Modifier: -1})
		function := model.FunctionID(2054)
		if id == 1184 {
			function = 1154
		}
		predicate := f.timestampPredicate(function, math.MaxInt64)
		result := extract(t, f.graph(), predicate, f.keys, nil, limits())
		if result.Residual != 0 {
			t.Fatal("strict timestamp bound retained residual")
		}
		for _, n := range []int64{math.MinInt64, 0, math.MaxInt64 - 1, math.MaxInt64} {
			got := matches(t, f.keys, result, model.Value{Type: model.TypeTimestamp, IntVal: n})
			if got != (n < math.MaxInt64) {
				t.Fatalf("timestamp%d value%d matched=%v", id, n, got)
			}
		}
	}
}
