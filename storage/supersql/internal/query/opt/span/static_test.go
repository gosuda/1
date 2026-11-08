package span_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
)

func TestStaticSpanExtractionLeavesUnknownParameterResidual(t *testing.T) {
	f := setup(t, integer)
	parameter := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[0], Nullable: true, Parameter: 1}, nil, model.Value{})
	predicate := f.call(467, 0, f.slots[0], parameter)
	result, err := span.ExtractStatic(t.Context(), f.graph(), predicate, f.keys, limits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Empty || result.Residual != predicate {
		t.Fatalf("unknown parameter became a fixed filter: %+v", result)
	}
	for _, value := range []model.Value{model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1)} {
		if !matches(t, f.keys, result, value) {
			t.Fatalf("unknown parameter excluded %v", value)
		}
	}
}

func TestStaticSpanExtractionIgnoresUnrelatedParameters(t *testing.T) {
	f := setup(t, integer)
	_ = f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[0], Nullable: true, Parameter: 1}, nil, model.Value{})
	predicate := f.compare(467, 0, 19)
	result, err := span.ExtractStatic(t.Context(), f.graph(), predicate, f.keys, limits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Residual != 0 || !matches(t, f.keys, result, model.Int64Val(19)) || matches(t, f.keys, result, model.Int64Val(20)) {
		t.Fatalf("unrelated parameter prevented exact point bound: %+v", result)
	}
}
