package ir

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestExtendRewritesValuesWithoutChangingSource(t *testing.T) {
	f := newFixture(t)
	original := f.constant(1)
	output := f.slot(f.integer, false, 0, 0)
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{original}})
	source := f.freeze(root)
	builder, err := Extend(t.Context(), source, limitsForTest())
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.AddScopeCapture(t.Context(), child, output); err != nil {
		t.Fatal(err)
	}
	scalar, err := source.CloneScalarSpec(t.Context(), original, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	scalar.Value = model.Int64Val(2)
	changed, err := builder.AddScalar(t.Context(), scalar)
	if err != nil {
		t.Fatal(err)
	}
	relation, err := source.CloneRelSpec(t.Context(), root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	relation.Expressions[0] = changed
	replacement, err := builder.AddRel(t.Context(), relation)
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(t.Context(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	before, err := source.CloneConstant(t.Context(), original, 1024)
	if err != nil {
		t.Fatal(err)
	}
	resultExpression, _ := result.Expression(result.Root(), 0)
	after, err := result.CloneConstant(t.Context(), resultExpression, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if before.IntVal != 1 || after.IntVal != 2 {
		t.Fatalf("source=%+v rewritten=%+v", before, after)
	}
	if source.CaptureCount(child) != 0 || result.CaptureCount(child) != 1 {
		t.Fatal("extension modified source captures")
	}
}

func TestExtendAndSpecCopiesRejectResourceFailures(t *testing.T) {
	f := newFixture(t)
	value := f.constant(1)
	output := f.slot(f.integer, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{value}})
	source := f.freeze(root)
	limited := limitsForTest()
	limited.MaxBytes = 1
	if builder, err := Extend(t.Context(), source, limited); builder != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("extension limit: %v", err)
	}
	if _, err := source.CloneRelSpec(t.Context(), root, 1); !errors.Is(err, ErrLimit) {
		t.Fatalf("relation copy limit: %v", err)
	}
	if _, err := source.CloneScalarSpec(t.Context(), value, 1); !errors.Is(err, ErrLimit) {
		t.Fatalf("scalar copy limit: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if builder, err := Extend(ctx, source, limitsForTest()); builder != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("extension cancellation: %v", err)
	}
}
