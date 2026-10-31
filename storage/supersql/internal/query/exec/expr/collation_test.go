package expr_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (f *fixture) collate(operand ir.ScalarID, collation model.CollationID) ir.ScalarID {
	f.t.Helper()
	layout, err := f.b.AddLayout(f.t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: collation}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Collate, Layout: layout, Nullable: f.nodes[operand].Nullable}, []ir.ScalarID{operand}, model.Value{})
}

func (f *fixture) textLess(left, right ir.ScalarID, collation model.CollationID) ir.ScalarID {
	f.t.Helper()
	effects := ir.Effects{MayError: true}
	sig, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 740, InputCollation: collation, Result: f.layouts[16], Strict: true, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{f.nodes[left].Layout, f.nodes[right].Layout}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Call, Layout: f.layouts[16], Signature: sig, Function: 740, Strict: true, Nullable: true, Effects: effects}, []ir.ScalarID{left, right}, model.Value{})
}

func TestTextComparisonUsesBoundOrDerivedCollation(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		bound, left, right, environment model.CollationID
		wantError                       bool
	}{
		{"bound wins over environment", 951, 950, 951, 0, false},
		{"derive without environment", 0, 950, 950, 0, false},
		{"default yields to POSIX", 0, 100, 951, 0, false},
		{"environment cannot resolve conflict", 0, 950, 951, 950, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			left := f.collate(f.constant(25, model.StringVal("Z")), tc.left)
			right := f.collate(f.constant(25, model.StringVal("a")), tc.right)
			root := f.textLess(left, right, tc.bound)
			o := f.options()
			o.Builtin = builtin.ResolveOptions{Collation: tc.environment, DefaultCollation: 950}
			p := compile(t, f.graph(), []ir.ScalarID{root}, o)
			for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
				input, out := batch(1), columnar.Batch{}
				err := state(t, p).Eval(t.Context(), &input, nil, &out, mode)
				if tc.wantError {
					var sqlErr *model.Error
					if !errors.As(err, &sqlErr) || sqlErr.Code != "42P22" {
						t.Fatalf("comparison error=%v, want 42P22", err)
					}
					if out.Rows() != 0 {
						t.Fatalf("failed comparison published %d rows", out.Rows())
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				requireInt(t, value(t, &out, 0, 0), 1, false)
			}
		})
	}
}

func TestIndeterminateComparisonSkipsNullAndInactiveLanes(t *testing.T) {
	f := newFixture(t)
	left := f.collate(f.constant(25, model.StringVal("Z")), 950)
	right := f.collate(f.constant(25, model.StringVal("a")), 951)
	null := f.collate(f.constant(25, model.NullVal(model.TypeString)), 950)
	comparison := f.textLess(left, right, 0)
	nullComparison := f.textLess(null, right, 0)
	truth := f.constant(16, model.BoolVal(true))
	guarded := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{truth, truth, comparison}, model.Value{})
	o := f.options()
	o.Builtin = builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}
	p := compile(t, f.graph(), []ir.ScalarID{guarded, nullComparison}, o)
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		input, out := batch(3), columnar.Batch{}
		if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		for lane := range 3 {
			requireInt(t, value(t, &out, 0, lane), 1, false)
			requireInt(t, value(t, &out, 1, lane), 0, true)
		}
	}
}

func TestCollationRelabelBorrowsIntermediatePayload(t *testing.T) {
	f := newFixture(t)
	payload := strings.Repeat("x", 64<<10)
	root := f.collate(f.collate(f.constant(25, model.StringVal(payload)), 950), 951)
	o := f.options()
	o.Limits.MaxStateBytes = 32 << 10
	p := compile(t, f.graph(), []ir.ScalarID{root}, o)
	input, out := batch(1), columnar.Batch{}
	s := state(t, p)
	if err := s.Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	if out.Columns[0].Type.Collation != 951 {
		t.Fatalf("result collation=%d, want POSIX", out.Columns[0].Type.Collation)
	}
	if got := value(t, &out, 0, 0); got.IsNull || got.StrVal != payload {
		t.Fatalf("relabel changed payload: null=%v bytes=%d", got.IsNull, len(got.StrVal))
	}
}

func TestBinaryRelabelPreservesPayloadAndResultType(t *testing.T) {
	f := newFixture(t)
	payload := strings.Repeat("x", 64<<10)
	inputValue := f.collate(f.constant(25, model.StringVal(payload)), 950)
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1043, Modifier: -1, Storage: model.TypeString, Collation: 950}})
	if err != nil {
		t.Fatal(err)
	}
	root := f.add(ir.Scalar{Kind: ir.Relabel, Layout: layout, CastContext: ir.ExplicitCast}, []ir.ScalarID{inputValue}, model.Value{})
	options := f.options()
	options.Limits.MaxStateBytes = 32 << 10
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	input, output := batch(1), columnar.Batch{}
	if err := state(t, program).Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		t.Fatal(err)
	}
	if output.Columns[0].Type.ID != 1043 || output.Columns[0].Type.Collation != 950 {
		t.Fatalf("binary cast result type=%+v", output.Columns[0].Type)
	}
	if got := value(t, &output, 0, 0); got.IsNull || got.StrVal != payload {
		t.Fatalf("binary cast changed payload: null=%t bytes=%d", got.IsNull, len(got.StrVal))
	}
}
