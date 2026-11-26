package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalAggregateDefersMixedInfinityUntilResult(t *testing.T) {
	var state builtin.IntervalAggregate
	positive := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	negative := model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
	for _, v := range []model.Interval{positive, negative} {
		if err := state.Advance(v); err != nil {
			t.Fatal(err)
		}
	}
	_, err := state.Sum()
	requireSQLState(t, err, "22008")
	_, err = state.Average()
	requireSQLState(t, err, "22008")
	if err := state.Advance(model.Interval{Months: math.MaxInt32}); err != nil {
		t.Fatal(err)
	}
	requireSQLState(t, state.Advance(model.Interval{Months: 1}), "22008")
}

func TestIntervalAggregateAverageCascadesFractions(t *testing.T) {
	var state builtin.IntervalAggregate
	empty, err := state.Average()
	if err != nil || !empty.IsNull {
		t.Fatalf("empty avg=%v,%v", empty, err)
	}
	for _, v := range []model.Interval{{Months: 1}, {Days: 1}} {
		if err := state.Advance(v); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := state.Sum()
	if err != nil || sum.IntervalVal != (model.Interval{Months: 1, Days: 1}) {
		t.Fatalf("sum=%v,%v", sum, err)
	}
	avg, err := state.Average()
	if err != nil || avg.IntervalVal != (model.Interval{Days: 15, Microseconds: 43200000000}) {
		t.Fatalf("avg=%v,%v", avg, err)
	}
	state = builtin.IntervalAggregate{}
	empty, err = state.Sum()
	if err != nil || !empty.IsNull {
		t.Fatalf("reset sum=%v,%v", empty, err)
	}
}
