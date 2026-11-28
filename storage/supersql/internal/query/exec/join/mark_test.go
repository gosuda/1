package join_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func markOptions(t *testing.T, q *runtime.Query, kind ir.SubqueryKind, memory uint64) join.Options {
	t.Helper()
	o := options(t, q, ir.MarkJoin)
	o.MarkKind = kind
	o.MemoryBytes = memory
	o.Output = []join.Column{{Side: join.Left, Index: 1}, {Side: join.Mark, Index: 0}}
	o.OutputLayouts = []model.TypeLayout{integer, boolean}
	return o
}

func TestMarksPreserveThreeValuedBagsAcrossSpill(t *testing.T) {
	null := model.NullVal(model.TypeInt64)
	left := [][]model.Value{pair(1, 10), pair(1, 10), pair(2, 20), {null, model.Int64Val(30)}}
	for _, memory := range []uint64{0, 1 << 20} {
		for _, tc := range []struct {
			name             string
			right            [][]model.Value
			exists, any, all []string
		}{
			{"empty", nil, []string{"10,0", "10,0", "20,0", "30,0"}, []string{"10,0", "10,0", "20,0", "30,0"}, []string{"10,1", "10,1", "20,1", "30,1"}},
			{"null only", [][]model.Value{{null, model.Int64Val(1)}}, []string{"10,0", "10,0", "20,0", "30,0"}, []string{"10,NULL", "10,NULL", "20,NULL", "30,NULL"}, []string{"10,NULL", "10,NULL", "20,NULL", "30,NULL"}},
			{"duplicates", [][]model.Value{pair(1, 1), pair(1, 2)}, []string{"10,1", "10,1", "20,0", "30,0"}, []string{"10,1", "10,1", "20,0", "30,NULL"}, []string{"10,1", "10,1", "20,0", "30,NULL"}},
			{"unknown before decisive", [][]model.Value{{null, model.Int64Val(1)}, pair(1, 2), pair(1, 3)}, []string{"10,1", "10,1", "20,0", "30,0"}, []string{"10,1", "10,1", "20,NULL", "30,NULL"}, []string{"10,NULL", "10,NULL", "20,0", "30,NULL"}},
			{"unknown after decisive", [][]model.Value{pair(1, 2), {null, model.Int64Val(1)}}, []string{"10,1", "10,1", "20,0", "30,0"}, []string{"10,1", "10,1", "20,NULL", "30,NULL"}, []string{"10,NULL", "10,NULL", "20,0", "30,NULL"}},
		} {
			for _, kind := range []ir.SubqueryKind{ir.ExistsSubquery, ir.AnySubquery, ir.AllSubquery} {
				t.Run(fmt.Sprintf("%s/%d/%d", tc.name, kind, memory), func(t *testing.T) {
					dir := t.TempDir()
					q := newQuery(t, runtime.OSFileSystem{}, dir)
					o := markOptions(t, q, kind, memory)
					c := open(t, t.Context(), newSource(t, o.LeftLayouts, left), newSource(t, o.RightLayouts, tc.right), o)
					want := tc.exists
					if kind == ir.AnySubquery {
						want = tc.any
					} else if kind == ir.AllSubquery {
						want = tc.all
					}
					assertBag(t, collect(t, c), want)
					released(t, q, dir)
				})
			}
		}
	}
}

func TestMarkCompoundKeysAndResidualsRetainFalseWitnesses(t *testing.T) {
	for _, memory := range []uint64{0, 1 << 20} {
		for _, tc := range []struct {
			name        string
			keys        []join.Key
			left, right []model.Value
			want        string
		}{
			{"unequal component dominates null", []join.Key{{Left: 0, Right: 0}, {Left: 1, Right: 1}}, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(10)}, []model.Value{model.Int64Val(1), model.Int64Val(20), model.BoolVal(true)}, "10,0"},
			{"false residual dominates null key", []join.Key{{Left: 0, Right: 0}}, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(10)}, []model.Value{model.Int64Val(1), model.Int64Val(20), model.BoolVal(false)}, "10,0"},
			{"null residual survives equal key", []join.Key{{Left: 0, Right: 0}}, pair(1, 10), []model.Value{model.Int64Val(1), model.Int64Val(20), model.NullVal(model.TypeBoolean)}, "10,NULL"},
			{"false key dominates null residual", []join.Key{{Left: 0, Right: 0}}, pair(2, 10), []model.Value{model.Int64Val(1), model.Int64Val(20), model.NullVal(model.TypeBoolean)}, "10,0"},
			{"null equal key", []join.Key{{Left: 0, Right: 0, NullEqual: true}}, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(10)}, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(20), model.BoolVal(true)}, "10,1"},
		} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, memory), func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := markOptions(t, q, ir.AnySubquery, memory)
				o.Keys = tc.keys
				o.RightLayouts = []model.TypeLayout{integer, integer, boolean}
				o.Residual, o.ResidualInputs = residual(t), []join.Column{{Side: join.Right, Index: 2}}
				c := open(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{tc.left}), newSource(t, o.RightLayouts, [][]model.Value{tc.right}), o)
				assertBag(t, collect(t, c), []string{tc.want})
				released(t, q, dir)
			})
		}
	}
}

// effectPredicate exposes observable residual effects without replacing replay or storage.
func effectPredicate(t *testing.T, call expr.ScalarCall) *expr.Program {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 64, MaxOperands: 128, MaxSlots: 8, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := b.AddLayout(t.Context(), boolean)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Nullable: true, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	arg, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: layout, Slot: slot, Nullable: true}})
	if err != nil {
		t.Fatal(err)
	}
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900001, Result: layout, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layout}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: scope, Layout: layout, Function: 900001, Signature: sig, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{arg}})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{slot}})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	program, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, expr.Options{Limits: expr.DefaultLimits(), Inputs: []expr.Input{{Slot: slot, Column: 0}}, Functions: markResolver{call: call}})
	if err != nil {
		t.Fatal(err)
	}
	return program
}

type markResolver struct{ call expr.ScalarCall }

func (r markResolver) Resolve(expr.Function) (expr.ScalarCall, error) { return r.call, nil }

func TestMarksEvaluateEffectsAfterDecisiveTruth(t *testing.T) {
	for _, memory := range []uint64{0, 1 << 20} {
		for _, kind := range []ir.SubqueryKind{ir.ExistsSubquery, ir.AnySubquery, ir.AllSubquery} {
			t.Run(fmt.Sprintf("%d/%d", kind, memory), func(t *testing.T) {
				dir := t.TempDir()
				q := newQuery(t, runtime.OSFileSystem{}, dir)
				o := markOptions(t, q, kind, memory)
				o.Keys = nil
				o.RightLayouts = []model.TypeLayout{boolean}
				o.ResidualInputs = []join.Column{{Side: join.Right, Index: 0}}
				var seen []model.Value
				o.Residual = effectPredicate(t, func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
					seen = append(seen, args[0])
					return args[0], nil
				})
				right := [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(false)}, {model.NullVal(model.TypeBoolean)}}
				c := open(t, t.Context(), newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, right), o)
				want := []string{"10,1", "11,1"}
				if kind == ir.AllSubquery {
					want = []string{"10,0", "11,0"}
				}
				assertBag(t, collect(t, c), want)
				values := []model.Value{model.BoolVal(true), model.BoolVal(false), model.NullVal(model.TypeBoolean), model.BoolVal(true), model.BoolVal(false), model.NullVal(model.TypeBoolean)}
				if !reflect.DeepEqual(seen, values) {
					t.Fatalf("effects=%v, want %v", seen, values)
				}
				released(t, q, dir)
			})
		}
	}
}

func TestMarkResidualFailureAndCancellationReleaseSpills(t *testing.T) {
	failure := errors.New("residual failure")
	for _, scenario := range []string{"error after true", "cancel after true", "early close"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := markOptions(t, q, ir.AnySubquery, 0)
			o.Keys = nil
			o.RightLayouts = []model.TypeLayout{boolean}
			o.ResidualInputs = []join.Column{{Side: join.Right, Index: 0}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			o.Residual = effectPredicate(t, func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				if args[0].IntVal == 0 {
					if scenario == "error after true" {
						return model.Value{}, failure
					}
					if scenario == "cancel after true" {
						cancel()
					}
				}
				return args[0], nil
			})
			c := open(t, ctx, newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10), pair(1, 11)}), newSource(t, o.RightLayouts, [][]model.Value{{model.BoolVal(true)}, {model.BoolVal(false)}}), o)
			out := columnar.Batch{MaxRows: 1}
			err := c.Next(t.Context(), &out)
			if scenario == "early close" {
				if err != nil {
					t.Fatal(err)
				}
				if q.Usage().Files == 0 {
					t.Fatal("mark replay did not spill")
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				want := failure
				if scenario == "cancel after true" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("Next=%v, want %v", err, want)
				}
			}
			out.Reset()
			released(t, q, dir)
		})
	}
}

var errEmptyMarkRight = errors.New("empty mark evaluated right child")

type forbiddenMarkSource struct{ closed bool }

func (*forbiddenMarkSource) Next(context.Context, *columnar.Batch) error {
	return errEmptyMarkRight
}
func (s *forbiddenMarkSource) Close() error { s.closed = true; return nil }

func TestEmptyMarkLeftDoesNotEvaluateRight(t *testing.T) {
	dir := t.TempDir()
	q := newQuery(t, runtime.OSFileSystem{}, dir)
	o := markOptions(t, q, ir.AnySubquery, 0)
	right := &forbiddenMarkSource{}
	c := open(t, t.Context(), newSource(t, o.LeftLayouts, nil), right, o)
	var out columnar.Batch
	if err := c.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("Next=%v, want EOF", err)
	}
	if !right.closed {
		t.Fatal("empty mark retained right child")
	}
	released(t, q, dir)
}

func TestInvalidMarkConstructionClosesBothChildren(t *testing.T) {
	for _, scenario := range []string{"missing kind", "scalar kind", "right output", "mark index", "mark type", "mark residual", "work budget", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			q := newQuery(t, runtime.OSFileSystem{}, dir)
			o := markOptions(t, q, ir.AnySubquery, 0)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := error(runtime.ErrInvalidConfig)
			switch scenario {
			case "missing kind":
				o.MarkKind = 0
			case "scalar kind":
				o.MarkKind = ir.ScalarSubquery
			case "right output":
				o.Output[0] = join.Column{Side: join.Right, Index: 1}
			case "mark index":
				o.Output[1].Index = 1
			case "mark type":
				o.OutputLayouts[1] = integer
			case "mark residual":
				o.Residual = residual(t)
				o.ResidualInputs = []join.Column{{Side: join.Mark, Index: 0}}
			case "work budget":
				o.WorkBytes = 1
				want = runtime.ErrBudget
			case "canceled":
				cancel()
				want = context.Canceled
			}
			left := newSource(t, o.LeftLayouts, [][]model.Value{pair(1, 10)})
			right := newSource(t, o.RightLayouts, [][]model.Value{pair(1, 20)})
			c, err := join.New(ctx, left, right, o)
			if c != nil {
				t.Cleanup(func() {
					if err := c.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if !errors.Is(err, want) {
				t.Fatalf("New=%v, want %v", err, want)
			}
			if !left.closed || !right.closed {
				t.Fatal("failed mark retained children")
			}
			released(t, q, dir)
		})
	}
}
