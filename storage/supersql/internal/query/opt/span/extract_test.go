package span_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
)

var integer = model.Type{ID: 20, Storage: model.TypeInt64}

func limits() span.Limits {
	return span.Limits{MaxSpans: 1024, MaxNodes: 4096, MaxWork: 1 << 20, MaxBytes: 16 << 20}
}

type fixture struct {
	t       *testing.T
	b       *ir.Builder
	scope   ir.ScopeID
	boolean ir.LayoutID
	layouts []ir.LayoutID
	keys    []span.Key
	slots   []ir.ScalarID
	nodes   map[ir.ScalarID]ir.Scalar
}

func setup(t *testing.T, types ...model.Type) *fixture {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 4096, MaxOperands: 16384, MaxSlots: 256, MaxDepth: 128, MaxBytes: 16 << 20, SourceBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, b: b, nodes: make(map[ir.ScalarID]ir.Scalar)}
	f.scope, err = b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.boolean, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	for i, typ := range types {
		layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		slot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: layout, Nullable: true, Object: 1, Column: model.ColumnID(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		f.layouts = append(f.layouts, layout)
		f.keys = append(f.keys, span.Key{Slot: slot, Column: storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: typ}})
		f.slots = append(f.slots, f.add(ir.Scalar{Kind: ir.Slot, Layout: layout, Slot: slot, Nullable: true}, nil, model.Value{}))
	}
	return f
}

func (f *fixture) add(n ir.Scalar, operands []ir.ScalarID, value model.Value) ir.ScalarID {
	f.t.Helper()
	n.Scope = f.scope
	for _, id := range operands {
		n.Effects = n.Effects.Union(f.nodes[id].Effects)
	}
	id, err := f.b.AddScalar(f.t.Context(), ir.ScalarSpec{Scalar: n, Operands: operands, Value: value})
	if err != nil {
		f.t.Fatal(err)
	}
	f.nodes[id] = n
	return id
}

func (f *fixture) constant(key int, v model.Value) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.Const, Layout: f.layouts[key], Nullable: v.IsNull}, nil, v)
}

func (f *fixture) call(function model.FunctionID, operator model.OperatorID, left, right ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	sig, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: function, Operator: operator, Result: f.boolean, Strict: true, Nullable: true}, Parameters: []ir.LayoutID{f.nodes[left].Layout, f.nodes[right].Layout}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Call, Function: function, Operator: operator, Signature: sig, Strict: true, Nullable: true, Layout: f.boolean}, []ir.ScalarID{left, right}, model.Value{})
}

func (f *fixture) compare(function model.FunctionID, key int, v int64) ir.ScalarID {
	return f.call(function, 0, f.slots[key], f.constant(key, model.Int64Val(v)))
}

func (f *fixture) booleanExpr(op ir.BoolOp, args ...ir.ScalarID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.Bool, Layout: f.boolean, BoolOp: op, Nullable: true}, args, model.Value{})
}

func (f *fixture) graph() *ir.Graph {
	f.t.Helper()
	outputs := make([]model.SlotID, len(f.keys))
	for i, key := range f.keys {
		outputs[i] = key.Slot
	}
	root, err := f.b.AddRel(f.t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: outputs})
	if err != nil {
		f.t.Fatal(err)
	}
	g, err := f.b.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

func extract(t *testing.T, g *ir.Graph, predicate ir.ScalarID, keys []span.Key, params []span.Parameter, budget span.Limits) span.Result {
	t.Helper()
	result, err := span.Extract(t.Context(), g, predicate, keys, params, budget)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func matches(t *testing.T, keys []span.Key, result span.Result, values ...model.Value) bool {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	layout := make([]storage.KeyColumn, len(keys))
	for i, key := range keys {
		layout[i] = key.Column
	}
	row, err := encoder.EncodePrefix(t.Context(), layout, values, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	within := func(bound storage.KeyBound, lower bool) bool {
		if bound.Unbounded {
			return true
		}
		encoded, err := encoder.EncodePrefix(t.Context(), layout, bound.Values, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(row, encoded) {
			return bound.Inclusive
		}
		order := bytes.Compare(row, encoded)
		return (lower && order > 0) || (!lower && order < 0)
	}
	for _, s := range result.Spans {
		if within(s.Lower, true) && within(s.Upper, false) {
			return true
		}
	}
	return false
}

func TestCompositeRangePreservesStrictAndInclusiveTupleExtensions(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		lower, upper               model.FunctionID
		includeLower, includeUpper bool
	}{
		{"open", 470, 469, false, false},
		{"closed", 472, 471, true, true},
		{"left_closed", 472, 469, true, false},
		{"right_closed", 470, 471, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, integer, integer, integer)
			tenant := f.call(0, 410, f.slots[0], f.constant(0, model.Int64Val(7)))
			predicate := f.booleanExpr(ir.And, tenant, f.compare(tc.lower, 1, 10), f.compare(tc.upper, 1, 20))
			result := extract(t, f.graph(), predicate, f.keys, nil, limits())
			if result.Empty || result.Residual != 0 || len(result.Spans) != 1 {
				t.Fatalf("expected exact interval, got %+v", result)
			}
			s := result.Spans[0]
			if len(s.Lower.Values) != 2 || len(s.Upper.Values) != 2 || s.Lower.Inclusive != tc.includeLower || s.Upper.Inclusive != tc.includeUpper {
				t.Fatalf("wrong composite cuts: %+v", s)
			}
			for _, tenant := range []int64{6, 7, 8} {
				for _, id := range []int64{9, 10, 11, 19, 20, 21} {
					for _, suffix := range []int64{math.MinInt64, 0, math.MaxInt64} {
						want := tenant == 7 && (id > 10 || id == 10 && tc.includeLower) && (id < 20 || id == 20 && tc.includeUpper)
						if got := matches(t, f.keys, result, model.Int64Val(tenant), model.Int64Val(id), model.Int64Val(suffix)); got != want {
							t.Fatalf("row (%d,%d,%d): got %t, want %t", tenant, id, suffix, got, want)
						}
					}
				}
			}
		})
	}
}

func TestLoweredINDeduplicatesDisjointAlternativesAndIgnoresNull(t *testing.T) {
	f := setup(t, integer)
	once := f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[0], Nullable: true}, []ir.ScalarID{f.slots[0]}, model.Value{})
	first := f.call(467, 0, once, f.constant(0, model.Int64Val(1)))
	last := f.call(467, 0, once, f.constant(0, model.Int64Val(math.MaxInt64)))
	null := f.call(467, 0, once, f.constant(0, model.NullVal(model.TypeInt64)))
	predicate := f.booleanExpr(ir.Or, first, last, first, null)
	budget := limits()
	budget.MaxSpans = 2
	result := extract(t, f.graph(), predicate, f.keys, nil, budget)
	if len(result.Spans) != 2 || result.Residual != 0 || result.Widened {
		t.Fatalf("expected two exact disjoint points: %+v", result)
	}
	for _, value := range []int64{math.MinInt64, 0, 1, 2, math.MaxInt64} {
		if got := matches(t, f.keys, result, model.Int64Val(value)); got != (value == 1 || value == math.MaxInt64) {
			t.Fatalf("value %d: selected=%t", value, got)
		}
	}
	if matches(t, f.keys, result, model.NullVal(model.TypeInt64)) {
		t.Fatal("IN selected NULL")
	}
}

func TestDescendingNullOrderingDoesNotChangeComparisonTruth(t *testing.T) {
	for _, descending := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			f := setup(t, integer)
			f.keys[0].Column.Descending, f.keys[0].Column.NullsFirst = descending, nullsFirst
			predicate := f.compare(470, 0, math.MinInt64)
			result := extract(t, f.graph(), predicate, f.keys, nil, limits())
			if result.Residual != 0 {
				t.Fatalf("comparison left residual: %+v", result)
			}
			for _, value := range []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(math.MinInt64), model.Int64Val(0), model.Int64Val(math.MaxInt64)} {
				want := !value.IsNull && value.IntVal > math.MinInt64
				if got := matches(t, f.keys, result, value); got != want {
					t.Fatalf("descending=%t nullsFirst=%t value=%+v got=%t want=%t", descending, nullsFirst, value, got, want)
				}
			}
		}
	}
}

func TestNullEqualityProvesEmptyButNullTestSelectsNull(t *testing.T) {
	f := setup(t, integer)
	equality := f.call(467, 0, f.slots[0], f.constant(0, model.NullVal(model.TypeInt64)))
	nullTest := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.boolean}, []ir.ScalarID{f.slots[0]}, model.Value{})
	g := f.graph()
	empty := extract(t, g, equality, f.keys, nil, limits())
	if !empty.Empty || len(empty.Spans) != 0 || empty.Residual != 0 {
		t.Fatalf("NULL equality must prove empty: %+v", empty)
	}
	result := extract(t, g, nullTest, f.keys, nil, limits())
	if !matches(t, f.keys, result, model.NullVal(model.TypeInt64)) || matches(t, f.keys, result, model.Int64Val(0)) {
		t.Fatalf("IS NULL selected wrong domain: %+v", result)
	}
}

func TestConjunctionProvesContradictoryEndpointsEmpty(t *testing.T) {
	f := setup(t, integer)
	predicate := f.booleanExpr(ir.And, f.compare(470, 0, math.MaxInt64), f.compare(471, 0, math.MaxInt64))
	result := extract(t, f.graph(), predicate, f.keys, nil, limits())
	if !result.Empty || result.Residual != 0 {
		t.Fatalf("strict/inclusive contradiction was not empty: %+v", result)
	}
}

func TestSuffixConstraintRetainsOriginalResidual(t *testing.T) {
	f := setup(t, integer, integer)
	predicate := f.compare(467, 1, 42)
	result := extract(t, f.graph(), predicate, f.keys, nil, limits())
	if result.Empty || result.Residual != predicate || len(result.Spans) != 1 || !result.Spans[0].Lower.Unbounded || !result.Spans[0].Upper.Unbounded {
		t.Fatalf("suffix-only constraint must retain full span and residual: %+v", result)
	}
}

func TestSpanBudgetWideningCannotLoseDisjuncts(t *testing.T) {
	f := setup(t, integer, integer)
	choices := f.booleanExpr(ir.Or, f.compare(467, 1, 1), f.compare(467, 1, 7), f.compare(467, 1, 99))
	predicate := f.booleanExpr(ir.And, f.compare(467, 0, 5), choices)
	budget := limits()
	budget.MaxSpans = 2
	g := f.graph()
	result := extract(t, g, predicate, f.keys, nil, budget)
	if !result.Widened || result.Residual != predicate || result.Empty || len(result.Spans) > 2 {
		t.Fatalf("missing conservative widening: %+v", result)
	}
	for _, value := range []int64{1, 7, 99} {
		if !matches(t, f.keys, result, model.Int64Val(5), model.Int64Val(value)) {
			t.Fatalf("widening lost id %d", value)
		}
	}
	if matches(t, f.keys, result, model.Int64Val(6), model.Int64Val(7)) {
		t.Fatal("widening discarded the safe tenant prefix")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := span.Extract(ctx, g, predicate, f.keys, nil, budget); !errors.Is(err, context.Canceled) {
		t.Fatalf("widening hid cancellation: %v", err)
	}
}

func TestByteWorkAndNodeLimitsRejectWithoutPartialResult(t *testing.T) {
	f := setup(t, integer)
	predicate := f.compare(467, 0, 42)
	g := f.graph()
	for _, field := range []string{"bytes", "work", "nodes"} {
		t.Run(field, func(t *testing.T) {
			budget := limits()
			switch field {
			case "bytes":
				budget.MaxBytes = 1
			case "work":
				budget.MaxWork = 1
			case "nodes":
				budget.MaxNodes = 1
			}
			result, err := span.Extract(t.Context(), g, predicate, f.keys, nil, budget)
			if !errors.Is(err, span.ErrLimit) || result.Empty || len(result.Spans) != 0 {
				t.Fatalf("limit returned unsafe partial result=%+v err=%v", result, err)
			}
		})
	}
}

func TestSharedBooleanDAGDoesNotExpandByPathCount(t *testing.T) {
	f := setup(t, integer)
	predicate := f.compare(467, 0, 42)
	for range 48 {
		predicate = f.booleanExpr(ir.Or, predicate, predicate)
	}
	budget := limits()
	budget.MaxSpans = 1
	budget.MaxWork = 10000
	result := extract(t, f.graph(), predicate, f.keys, nil, budget)
	if result.Widened || result.Residual != 0 || !matches(t, f.keys, result, model.Int64Val(42)) || matches(t, f.keys, result, model.Int64Val(43)) {
		t.Fatalf("shared DAG lost its exact point: %+v", result)
	}
}
