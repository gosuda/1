package aggregate_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func recordValue(values ...model.Value) model.Value {
	return model.Value{Type: model.TypeComposite, ArrayVal: values}
}

func TestRecordExtremaPreserveConcreteLayoutsAndNestedNullOrdering(t *testing.T) {
	for _, id := range []model.TypeID{2249, 80000} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			layout := model.TypeLayout{Type: model.Type{ID: id, Storage: model.TypeComposite}, Children: []model.TypeLayout{money, fixedBits}}
			low := recordValue(model.Int64Val(100), packedBits(9, 0xa0))
			high := recordValue(model.Int64Val(100), model.NullVal(model.TypeBytes))
			input := [][]model.Value{{high}, {model.NullVal(model.TypeComposite)}, {recordValue(model.Int64Val(100), packedBits(9, 0xc0))}, {low}, {low}}
			calls := []aggregate.Call{
				{Function: 6374, Arguments: []int{0}, Distinct: true, OrderBy: []aggregate.OrderKey{{Column: 0, Descending: true}}},
				{Function: 6373, Arguments: []int{0}},
			}
			o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{layout, layout}, nil, calls)
			rows, err := drain(t, open(t, newSource(t, o.InputLayouts, input), o))
			if err != nil || len(rows) != 1 {
				t.Fatalf("record extrema=%v, error=%v", rows, err)
			}
			for i, result := range rows[0] {
				if result.IsNull || result.Type != model.TypeComposite || len(result.ArrayVal) != 2 || result.ArrayVal[0].IntVal != 100 {
					t.Fatalf("record extreme %d=%+v", i, result)
				}
			}
			requireBits(t, rows[0][0].ArrayVal[1], low.ArrayVal[1])
			requireBits(t, rows[0][1].ArrayVal[1], high.ArrayVal[1])
			released(t, q, dir)
		})
	}
}

func TestRecordExtremaRejectMismatchedShapeAndUnsupportedComparator(t *testing.T) {
	for _, function := range []model.FunctionID{6373, 6374} {
		t.Run(fmt.Sprint(function), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			input := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{money}}
			output := model.TypeLayout{Type: input.Type, Children: []model.TypeLayout{int8}}
			o := options(t, q, []model.TypeLayout{input}, []model.TypeLayout{output}, nil, []aggregate.Call{{Function: function, Arguments: []int{0}}})
			s := newSource(t, o.InputLayouts, nil)
			c, err := aggregate.New(t.Context(), s, o)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if !errors.Is(err, columnar.ErrValueLayout) || s.pulls != 0 || !s.closed {
				t.Fatalf("record result shape: error=%v, pulls=%d, closed=%v", err, s.pulls, s.closed)
			}
			input.Children[0] = model.TypeLayout{Type: model.Type{ID: 114, Storage: model.TypeJSONB}}
			o.OutputLayouts[0] = input
			var missing *builtin.MissingKeyImplementationError
			if err := aggregate.CheckSupport(t.Context(), o); !errors.As(err, &missing) || missing.Type.ID != 114 {
				t.Fatalf("record JSON comparator=%v, want explicit missing type 114", err)
			}
			released(t, q, dir)
		})
	}
}

func TestRecordExtremaOwnSelectedPayloadAcrossAccumulatorReset(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{fixedBits}}
	a := sameTypeAccumulator(t, q, 6374, layout, 1024)
	value := recordValue(packedBits(9, 0xa0))
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	clear(value.ArrayVal[0].BytesVal)
	if err := a.Advance(t.Context(), []model.Value{recordValue(packedBits(9, 0xc0))}); err != nil {
		t.Fatal(err)
	}
	got, err := a.Result(t.Context())
	if err != nil || got.IsNull || len(got.ArrayVal) != 1 {
		t.Fatalf("record minimum=%+v, error=%v", got, err)
	}
	requireBits(t, got.ArrayVal[0], packedBits(9, 0xa0))
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	got, err = a.Result(t.Context())
	if err != nil || !got.IsNull {
		t.Fatalf("reset record minimum=%+v, error=%v", got, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}
