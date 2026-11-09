package builtin_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTemporalInRangeInfinityConflicts(t *testing.T) {
	call, err := builtin.Resolve(4134, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infinity := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	for _, arguments := range [][]model.Value{
		{{Type: model.TypeTimestamp}, {Type: model.TypeTimestamp, IntVal: math.MaxInt64}, {Type: model.TypeInterval, IntervalVal: infinity}, model.BoolVal(true), model.BoolVal(false)},
		{{Type: model.TypeTimestamp}, {Type: model.TypeTimestamp, IntVal: math.MinInt64}, {Type: model.TypeInterval, IntervalVal: infinity}, model.BoolVal(false), model.BoolVal(true)},
	} {
		result, err := call.Call(t.Context(), arguments, builtin.CallBudget{})
		if err != nil || result.IsNull || result.Type != model.TypeBoolean || result.IntVal != 1 {
			t.Fatalf("in_range infinity = %v, %v", result, err)
		}
	}
}

func TestTemporalInRangeUsesTypeSpecificIntervalSign(t *testing.T) {
	negativeByComparison := model.Interval{Months: 1, Days: -31}
	timestamp, err := builtin.Resolve(4134, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{{Type: model.TypeTimestamp}, {Type: model.TypeTimestamp}, {Type: model.TypeInterval, IntervalVal: negativeByComparison}, model.BoolVal(false), model.BoolVal(false)}
	_, err = timestamp.Call(t.Context(), arguments, builtin.CallBudget{})
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "22013" {
		t.Fatalf("timestamp negative offset = %v", err)
	}

	clock, err := builtin.Resolve(4137, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := clock.Call(t.Context(), arguments, builtin.CallBudget{})
	if err != nil || result.IsNull || result.IntVal != 1 {
		t.Fatalf("time offset sign = %v, %v", result, err)
	}
}
