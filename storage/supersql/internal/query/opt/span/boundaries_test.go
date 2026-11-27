package span_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
)

func TestBoundParameterMustMatchEveryGraphDeclaration(t *testing.T) {
	f := setup(t, integer)
	param := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[0], Nullable: true, Parameter: 1}, nil, model.Value{})
	predicate := f.call(467, 0, f.slots[0], param)
	g := f.graph()
	good := span.Parameter{Number: 1, Layout: model.TypeLayout{Type: integer}, Value: model.Int64Val(19)}
	result := extract(t, g, predicate, f.keys, []span.Parameter{good}, limits())
	if result.Residual != 0 || !matches(t, f.keys, result, model.Int64Val(19)) || matches(t, f.keys, result, model.Int64Val(20)) {
		t.Fatalf("bound parameter was not an exact point: %+v", result)
	}
	for _, tc := range []struct {
		name   string
		params []span.Parameter
	}{
		{"missing", nil},
		{"duplicate", []span.Parameter{good, good}},
		{"undeclared", []span.Parameter{{Number: 2, Layout: good.Layout, Value: good.Value}}},
		{"sql_identity", []span.Parameter{{Number: 1, Layout: model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}, Value: good.Value}}},
		{"datum_storage", []span.Parameter{{Number: 1, Layout: good.Layout, Value: model.StringVal("19")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := span.Extract(t.Context(), g, predicate, f.keys, tc.params, limits()); !errors.Is(err, span.ErrInvalid) {
				t.Fatalf("parameter mismatch error=%v", err)
			}
		})
	}
	good.Value = model.NullVal(model.TypeInt64)
	result = extract(t, g, predicate, f.keys, []span.Parameter{good}, limits())
	if !result.Empty {
		t.Fatalf("NULL bound parameter did not prove empty: %+v", result)
	}
}

func TestResultPayloadsDoNotAliasParametersOrGraphConstants(t *testing.T) {
	bytea := model.Type{ID: 17, Storage: model.TypeBytes}
	f := setup(t, bytea)
	param := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[0], Nullable: true, Parameter: 1}, nil, model.Value{})
	constantInput := []byte("constant")
	constant := f.constant(0, model.BytesVal(constantInput))
	predicate := f.booleanExpr(ir.Or, f.call(1948, 0, f.slots[0], param), f.call(1948, 0, f.slots[0], constant))
	g := f.graph()
	params := []span.Parameter{{Number: 1, Layout: model.TypeLayout{Type: bytea}, Value: model.BytesVal([]byte("parameter"))}}
	result := extract(t, g, predicate, f.keys, params, limits())
	params[0].Value.BytesVal[0] = 'X'
	constantInput[0] = 'X'
	if !matches(t, f.keys, result, model.BytesVal([]byte("parameter"))) || !matches(t, f.keys, result, model.BytesVal([]byte("constant"))) {
		t.Fatal("input mutation changed returned bounds")
	}
	result.Spans[0].Lower.Values[0].BytesVal[0] = 'Y'
	if string(result.Spans[0].Upper.Values[0].BytesVal) != "constant" {
		t.Fatal("lower endpoint aliases upper endpoint")
	}
	value, err := g.CloneConstant(t.Context(), constant, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.BytesVal) != "constant" {
		t.Fatal("returned bounds changed immutable graph")
	}
	if string(params[0].Value.BytesVal) != "Xarameter" {
		t.Fatal("returned bounds changed borrowed parameter")
	}
}

func TestUnknownIdentityAndNonBinaryCollationRetainFullResidual(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      model.Type
		function model.FunctionID
		value    model.Value
	}{
		{"unknown_identity", integer, 999999, model.Int64Val(1)},
		{"locale_collation", model.Type{ID: 25, Storage: model.TypeString, Collation: 12345}, 67, model.StringVal("a")},
		{"unresolved_collation", model.Type{ID: 25, Storage: model.TypeString, Collation: 100}, 67, model.StringVal("a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.typ)
			predicate := f.call(tc.function, 0, f.slots[0], f.constant(0, tc.value))
			result := extract(t, f.graph(), predicate, f.keys, nil, limits())
			if result.Empty || result.Residual != predicate || len(result.Spans) != 1 || !result.Spans[0].Lower.Unbounded || !result.Spans[0].Upper.Unbounded {
				t.Fatalf("unsupported comparison restricted rows: %+v", result)
			}
		})
	}
}

func TestResolvedBinaryCollationExtractsTextEquality(t *testing.T) {
	f := setup(t, model.Type{ID: 25, Storage: model.TypeString, Collation: 950})
	predicate := f.call(67, 0, f.slots[0], f.constant(0, model.StringVal("a")))
	result := extract(t, f.graph(), predicate, f.keys, nil, limits())
	if result.Residual != 0 || !matches(t, f.keys, result, model.StringVal("a")) || matches(t, f.keys, result, model.StringVal("A")) {
		t.Fatalf("binary equality was not exact: %+v", result)
	}
}

func TestCastIsNotEvaluatedToInferAnEmptyRange(t *testing.T) {
	f := setup(t, integer, model.Type{ID: 25, Storage: model.TypeString, Collation: 950})
	text := f.constant(1, model.StringVal("not an integer"))
	sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 999999, Result: f.layouts[0], Strict: true, Nullable: true, Effects: ir.Effects{MayError: true}}, Parameters: []ir.LayoutID{f.layouts[1]}})
	if err != nil {
		t.Fatal(err)
	}
	cast := f.add(ir.Scalar{Kind: ir.Cast, Function: 999999, Signature: sig, Layout: f.layouts[0], Strict: true, Nullable: true, CastContext: ir.ExplicitCast, Effects: ir.Effects{MayError: true}}, []ir.ScalarID{text}, model.Value{})
	predicate := f.call(467, 0, f.slots[0], cast)
	result := extract(t, f.graph(), predicate, f.keys, nil, limits())
	if result.Empty || result.Residual != predicate || !result.Spans[0].Lower.Unbounded || !result.Spans[0].Upper.Unbounded {
		t.Fatalf("cast was used as an executable bound: %+v", result)
	}
}

func TestIntersectedAlternativesFormCompositeEqualityPrefixes(t *testing.T) {
	f := setup(t, integer, integer)
	tenants := f.booleanExpr(ir.Or, f.compare(467, 0, 1), f.compare(467, 0, 3))
	ids := f.booleanExpr(ir.Or, f.compare(467, 1, 10), f.compare(467, 1, 30))
	predicate := f.booleanExpr(ir.And, tenants, ids)
	g := f.graph()
	result := extract(t, g, predicate, f.keys, nil, limits())
	if len(result.Spans) != 4 || result.Residual != 0 {
		t.Fatalf("expected four composite points: %+v", result)
	}
	for _, tenant := range []int64{1, 2, 3} {
		for _, id := range []int64{10, 20, 30} {
			want := tenant != 2 && id != 20
			if got := matches(t, f.keys, result, model.Int64Val(tenant), model.Int64Val(id)); got != want {
				t.Fatalf("row (%d,%d) selected=%t want=%t", tenant, id, got, want)
			}
		}
	}
	budget := limits()
	budget.MaxSpans = 2
	wide := extract(t, g, predicate, f.keys, nil, budget)
	if !wide.Widened || wide.Residual != predicate {
		t.Fatalf("Cartesian budget did not retain residual: %+v", wide)
	}
	for _, tenant := range []int64{1, 3} {
		for _, id := range []int64{10, 30} {
			if !matches(t, f.keys, wide, model.Int64Val(tenant), model.Int64Val(id)) {
				t.Fatalf("Cartesian widening lost (%d,%d)", tenant, id)
			}
		}
	}
}

func TestUnionMergesTouchingInclusiveRangesWithoutBridgingOpenGap(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		f := setup(t, integer)
		upper := model.FunctionID(469)
		if inclusive {
			upper = 471
		}
		predicate := f.booleanExpr(ir.Or, f.compare(upper, 0, 0), f.compare(470, 0, 0))
		result := extract(t, f.graph(), predicate, f.keys, nil, limits())
		count := 2
		if inclusive {
			count = 1
		}
		if len(result.Spans) != count || result.Residual != 0 {
			t.Fatalf("inclusive=%t expected %d exact spans: %+v", inclusive, count, result)
		}
		if got := matches(t, f.keys, result, model.Int64Val(0)); got != inclusive {
			t.Fatalf("inclusive=%t boundary included=%t", inclusive, got)
		}
		if matches(t, f.keys, result, model.NullVal(model.TypeInt64)) {
			t.Fatal("merged ranges selected NULL")
		}
	}
}
