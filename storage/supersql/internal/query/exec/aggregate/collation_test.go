package aggregate_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestExtremaIndeterminateCollationWaitsForComparison(t *testing.T) {
	for _, function := range []model.FunctionID{2145, 2129} {
		q, dir := query(t, true, 0)
		layout := text
		layout.Type.Collation = 0
		o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{layout}, nil, nil)
		a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: function, InputLayouts: o.InputLayouts, OutputLayout: layout, Encoder: o.Encoder, Query: q, MaxRecordBytes: 1024, WorkBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, input := range []model.Value{model.NullVal(model.TypeString), model.StringVal("z"), model.NullVal(model.TypeString)} {
			if err := a.Advance(t.Context(), []model.Value{input}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := a.Result(t.Context())
		if err != nil || got.IsNull || got.StrVal != "z" {
			t.Fatalf("first value=%v, error=%v", got, err)
		}
		err = a.Advance(t.Context(), []model.Value{model.StringVal("a")})
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != "42P22" {
			t.Fatalf("second value error=%v, want 42P22", err)
		}
		got, err = a.Result(t.Context())
		if err != nil || got.IsNull || got.StrVal != "z" {
			t.Fatalf("failed comparison changed state: %v, %v", got, err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		released(t, q, dir)
	}
}

func TestAggregateBoundCollationDoesNotRelabelInput(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		q, dir := query(t, false, 64<<20)
		layout := text
		layout.Type.Collation = 0
		call := aggregate.Call{Function: 2145, InputCollation: 951, Arguments: []int{0}, Distinct: distinct}
		o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{layout}, nil, []aggregate.Call{call})
		input := newSource(t, o.InputLayouts, [][]model.Value{{model.StringVal("z")}, {model.StringVal("a")}, {model.StringVal("z")}})
		got, err := drain(t, open(t, input, o))
		if err != nil || len(got) != 1 || got[0][0].IsNull || got[0][0].StrVal != "a" {
			t.Fatalf("distinct=%v rows=%v, error=%v", distinct, got, err)
		}
		released(t, q, dir)
	}
}

func TestDistinctRejectsIndeterminateCollationBeforeInput(t *testing.T) {
	q, dir := query(t, true, 0)
	layout := text
	layout.Type.Collation = 0
	o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{int8}, nil, []aggregate.Call{{Function: 2147, Arguments: []int{0}, Distinct: true}})
	input := newSource(t, o.InputLayouts, nil)
	_, err := aggregate.New(t.Context(), input, o)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "42P22" {
		t.Fatalf("DISTINCT error=%v, want 42P22", err)
	}
	if input.pulls != 0 || !input.closed {
		t.Fatalf("invalid DISTINCT read or retained input: pulls=%d closed=%v", input.pulls, input.closed)
	}
	released(t, q, dir)
}
