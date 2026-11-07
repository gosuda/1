package normalize_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func (f *fixture) rowComparisonCall(name string, parameter ir.LayoutID, args ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	var selected builtin.Signature
	for signature := range builtin.SignaturesNamed(name) {
		selected = signature
	}
	if selected.ID == 0 || int(selected.ArgumentCount) != len(args) {
		f.t.Fatalf("missing binary comparison/input function %s", name)
	}
	parameters := make([]ir.LayoutID, len(args))
	for i := range parameters {
		parameters[i] = parameter
	}
	effects := ir.Effects{MayError: true}
	signature, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: selected.ID, Result: f.integer, Strict: selected.Strict, Nullable: true, Effects: effects}, Parameters: parameters})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.integer, Function: selected.ID, Signature: signature, Strict: selected.Strict, Nullable: true, Effects: effects}, Operands: args})
}

func (f *fixture) rowCompare(kind ir.RowComparisonKind, calls ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.RowCompare, Scope: f.scope, Layout: f.boolean, RowComparison: kind, Nullable: true, Effects: ir.Effects{MayError: true}}, Operands: calls})
}

func TestRowCompareNormalizesAllLeftInputsBeforeRightInputs(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"unused later left", "22012"},
		{"unused later right", "22012"},
		{"null prefix still visits later left", "22012"},
		{"later left before earlier right", "22003"},
		{"left fields in source order", "22012"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			zero := f.constant(f.integer, model.Int64Val(0))
			one := f.constant(f.integer, model.Int64Val(1))
			two := f.constant(f.integer, model.Int64Val(2))
			divide := f.rowComparisonCall("int4div", f.integer, one, zero)
			overflow := f.rowComparisonCall("int4pl", f.integer, f.constant(f.integer, model.Int64Val(2147483647)), one)
			left, right := []ir.ScalarID{one, one}, []ir.ScalarID{two, two}
			switch scenario.name {
			case "unused later left":
				left[1] = divide
			case "unused later right":
				right[1] = divide
			case "null prefix still visits later left":
				left[0], left[1] = f.constant(f.integer, model.NullVal(model.TypeInt64)), divide
			case "later left before earlier right":
				left[1], right[0] = overflow, divide
			case "left fields in source order":
				left[0], left[1] = divide, overflow
			}
			first := f.rowComparisonCall("btint4cmp", f.integer, left[0], right[0])
			second := f.rowComparisonCall("btint4cmp", f.integer, left[1], right[1])
			root := f.rowCompare(ir.RowCompareLess, first, second)
			_, err := normalize.Normalize(t.Context(), f.project(root, f.boolean, ir.Effects{MayError: true}, true), options())
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != scenario.want {
				t.Fatalf("normalization error=%v, want %s", err, scenario.want)
			}
		})
	}
}

func TestRowCompareSupportCallbackErrorsRemainDemandedAtRuntime(t *testing.T) {
	for _, equalPrefix := range []bool{false, true} {
		f := newFixture(t)
		one := f.constant(f.integer, model.Int64Val(1))
		right := one
		if !equalPrefix {
			right = f.constant(f.integer, model.Int64Val(2))
		}
		first := f.rowComparisonCall("btint4cmp", f.integer, one, right)
		text, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1}})
		if err != nil {
			t.Fatal(err)
		}
		second := f.rowComparisonCall("bttextcmp", text, f.constant(text, model.StringVal("a")), f.constant(text, model.StringVal("b")))
		root := f.rowCompare(ir.RowCompareLess, first, second)
		original := f.project(root, f.boolean, ir.Effects{MayError: true}, true)
		extension, err := ir.Extend(t.Context(), original, options().Limits)
		if err != nil {
			t.Fatal(err)
		}
		extended, err := extension.Build(t.Context(), original.Root())
		if err != nil {
			t.Fatal(err)
		}
		graph := normalized(t, extended)
		values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, nil, 1)
		if equalPrefix {
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "42P22" {
				t.Fatalf("demanded comparison error=%v, want 42P22", err)
			}
		} else if err != nil || values[0][0].IsNull || values[0][0].IntVal != 1 {
			t.Fatalf("unused support callback result=%v, error=%v", values, err)
		}
	}
}

func TestRowComparePrivateSupportCallsDoNotSuppressStandaloneFolding(t *testing.T) {
	f := newFixture(t)
	first := f.rowComparisonCall("btint4cmp", f.integer, f.constant(f.integer, model.Int64Val(1)), f.constant(f.integer, model.Int64Val(2)))
	text, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1}})
	if err != nil {
		t.Fatal(err)
	}
	shared := f.rowComparisonCall("bttextcmp", text, f.constant(text, model.StringVal("a")), f.constant(text, model.StringVal("b")))
	root := f.rowCompare(ir.RowCompareLess, first, shared)
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}})
	rowOutput, callOutput := f.slot(f.boolean, true), f.slot(f.integer, true)
	graph := f.freeze(f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{rowOutput, callOutput}, Bindings: []ir.Binding{{Slot: rowOutput, Expr: root}, {Slot: callOutput, Expr: shared}}}))
	_, err = normalize.Normalize(t.Context(), graph, options())
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "42P22" {
		t.Fatalf("standalone comparison was not folded: %v", err)
	}
}

func TestRowCompareIgnoresFoldedStandaloneSupportCallMemo(t *testing.T) {
	f := newFixture(t)
	shared := f.rowComparisonCall("btint4cmp", f.integer, f.constant(f.integer, model.Int64Val(1)), f.constant(f.integer, model.Int64Val(2)))
	root := f.rowCompare(ir.RowCompareGreaterEqual, shared, shared)
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}})
	callOutput, rowOutput := f.slot(f.integer, true), f.slot(f.boolean, true)
	graph := f.freeze(f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{callOutput, rowOutput}, Bindings: []ir.Binding{{Slot: callOutput, Expr: shared}, {Slot: rowOutput, Expr: root}}}))
	graph = normalized(t, graph)
	call, _ := graph.Binding(graph.Root(), 0)
	row, _ := graph.Binding(graph.Root(), 1)
	values, err := evaluate(t, graph, []ir.ScalarID{call.Expr, row.Expr}, nil, 1)
	if err != nil || values[0][0].IsNull || values[0][0].IntVal >= 0 || values[1][0].IsNull || values[1][0].IntVal != 0 {
		t.Fatalf("shared scalar normalization result=%v, error=%v", values, err)
	}
}
