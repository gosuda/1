package builtin_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type fixedTemporalInstant model.SQLTimestamp

func (instant fixedTemporalInstant) TransactionTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	return model.SQLTimestamp(instant), ctx.Err()
}

func (instant fixedTemporalInstant) StatementTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	return model.SQLTimestamp(instant), ctx.Err()
}

func (instant fixedTemporalInstant) ClockTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	return model.SQLTimestamp(instant), ctx.Err()
}

func TestTimetzNowPreservesActualOffsetDuringRepeatedHour(t *testing.T) {
	call, err := builtin.Resolve(1350, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		instant fixedTemporalInstant
		text    string
		west    int32
	}{
		{689578212123456, "now", 14400},
		{689581812123456, "now", 18000},
		{689578212123456, "now +01", -3600},
	} {
		value, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.text), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{Temporal: tc.instant, OutputBytes: 12, MemoryBytes: 12})
		if err != nil {
			t.Fatal(err)
		}
		micros, west, err := builtin.DecodeTimetz(value.BytesVal)
		if err != nil || micros != 5412123456 || west != tc.west {
			t.Fatalf("timetz %q at %d = %d/%d, %v; want 5412123456/%d", tc.text, tc.instant, micros, west, err, tc.west)
		}
	}
}

func TestTimetzIntervalZoneRetainsFullInt32Offset(t *testing.T) {
	budget := builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64}
	input, err := jsonCallable(t, 1350).Call(t.Context(), []model.Value{model.StringVal("00:00+00"), model.Int64Val(0), model.Int64Val(-1)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	span := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 2147483648000000}}
	value, err := jsonCallable(t, 2038).Call(t.Context(), []model.Value{span, input}, budget)
	if err != nil {
		t.Fatal(err)
	}
	output, err := jsonCallable(t, 1351).Call(t.Context(), []model.Value{value}, budget)
	if err != nil || output.StrVal != "20:45:52+4294370773:4294967282:4294967288" {
		t.Fatalf("full-width zone output = %q, %v", output.StrVal, err)
	}
	_, err = jsonCallable(t, 1351).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64})
	requireSQLState(t, err, "54000")
}

func TestTimeOfDayUsesInvocationClockAndAdmitsText(t *testing.T) {
	call := jsonCallable(t, 274)
	budget := builtin.CallBudget{Temporal: fixedTemporalInstant(689578212123456), OutputBytes: 64, MemoryBytes: 64}
	value, err := call.Call(t.Context(), nil, budget)
	if err != nil || value.StrVal != "Sun Nov 07 05:30:12.123456 2021 UTC" {
		t.Fatalf("timeofday = %q, %v", value.StrVal, err)
	}
	budget.MemoryBytes = 1
	_, err = call.Call(t.Context(), nil, budget)
	requireSQLState(t, err, "54000")
	budget.Temporal = nil
	_, err = call.Call(t.Context(), nil, budget)
	requireSQLState(t, err, "55000")
}
