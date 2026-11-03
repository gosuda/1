package aggregate_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestBooleanAccumulatorRejectsNoncanonicalInputWithoutChangingState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	configuration := options(t, q, []model.TypeLayout{boolean}, []model.TypeLayout{boolean}, nil, nil)
	state, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: 2517, InputLayouts: []model.TypeLayout{boolean}, OutputLayout: boolean, Encoder: configuration.Encoder, Query: q, MaxRecordBytes: 4096, WorkBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := state.Advance(t.Context(), []model.Value{model.BoolVal(true)}); err != nil {
		t.Fatal(err)
	}
	if err := state.Advance(t.Context(), []model.Value{{Type: model.TypeBoolean, IntVal: 2}}); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("noncanonical boolean=%v", err)
	}
	value, err := state.Result(t.Context())
	if err != nil || value.IsNull || value.Type != model.TypeBoolean || value.IntVal != 1 {
		t.Fatalf("state after rejected boolean=%v err=%v", value, err)
	}
	if err := state.Advance(t.Context(), []model.Value{model.BoolVal(false)}); err != nil {
		t.Fatal(err)
	}
	value, err = state.Result(t.Context())
	if err != nil || value.IsNull || value.IntVal != 0 {
		t.Fatalf("state after false=%v err=%v", value, err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}
