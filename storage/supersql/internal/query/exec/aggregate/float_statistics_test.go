package aggregate_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestFloatingAccumulatorOverflowPreservesPriorMoments(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		function     model.FunctionID
		prior, reset model.Value
	}{
		{"average", 2105, model.Float64Val(1e200), model.Float64Val(1)},
		{"population", 2722, model.Float64Val(0), model.Float64Val(1)},
		{"sample", 2645, model.NullVal(model.TypeFloat64), model.Float64Val(2)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			o := options(t, q, []model.TypeLayout{float8}, []model.TypeLayout{float8}, nil, nil)
			state, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: scenario.function, InputLayouts: []model.TypeLayout{float8}, OutputLayout: float8, Encoder: o.Encoder, Query: q, MaxRecordBytes: 4096, WorkBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := state.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := state.Advance(t.Context(), []model.Value{model.Float64Val(1e200)}); err != nil {
				t.Fatal(err)
			}
			err = state.Advance(t.Context(), []model.Value{model.Float64Val(-1e200)})
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "22003" {
				t.Fatalf("overflow=%v", err)
			}
			value, err := state.Result(t.Context())
			if err != nil || value.IsNull != scenario.prior.IsNull || value.FloatVal != scenario.prior.FloatVal {
				t.Fatalf("after rejected input=%v error=%v, want %v", value, err, scenario.prior)
			}
			if err := state.Reset(); err != nil {
				t.Fatal(err)
			}
			for _, v := range []float64{0, 2} {
				if err := state.Advance(t.Context(), []model.Value{model.Float64Val(v)}); err != nil {
					t.Fatal(err)
				}
			}
			value, err = state.Result(t.Context())
			if err != nil || value.IsNull || value.FloatVal != scenario.reset.FloatVal {
				t.Fatalf("after reset=%v error=%v, want %v", value, err, scenario.reset)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}
