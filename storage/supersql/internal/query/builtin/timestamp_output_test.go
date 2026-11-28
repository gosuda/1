package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func timestampOutputCallable(t *testing.T, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(1313, options)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireTimestampOutput(t *testing.T, function builtin.Callable, micros int64, want string) model.Value {
	t.Helper()
	value, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: micros}}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: uint64(len(want))})
	if err != nil || value.IsNull || value.Type != model.TypeString || value.StrVal != want {
		t.Fatalf("timestamp_out(%d) = %q (type %v, null %v), %v; want %q", micros, value.StrVal, value.Type, value.IsNull, err, want)
	}
	return value
}

func TestTimestampOutputStylesAndOrders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style builtin.DateStyle
		want  [3]string
		bc    [3]string
	}{
		{"ISO", builtin.DateISO,
			[3]string{"2000-02-03 04:05:06.1234", "2000-02-03 04:05:06.1234", "2000-02-03 04:05:06.1234"},
			[3]string{"0001-02-29 04:05:06.1234 BC", "0001-02-29 04:05:06.1234 BC", "0001-02-29 04:05:06.1234 BC"}},
		{"SQL", builtin.DateSQL,
			[3]string{"02/03/2000 04:05:06.1234", "03/02/2000 04:05:06.1234", "02/03/2000 04:05:06.1234"},
			[3]string{"02/29/0001 04:05:06.1234 BC", "29/02/0001 04:05:06.1234 BC", "02/29/0001 04:05:06.1234 BC"}},
		{"German", builtin.DateGerman,
			[3]string{"03.02.2000 04:05:06.1234", "03.02.2000 04:05:06.1234", "03.02.2000 04:05:06.1234"},
			[3]string{"29.02.0001 04:05:06.1234 BC", "29.02.0001 04:05:06.1234 BC", "29.02.0001 04:05:06.1234 BC"}},
		{"Postgres", builtin.DatePostgres,
			[3]string{"Thu Feb 03 04:05:06.1234 2000", "Thu 03 Feb 04:05:06.1234 2000", "Thu Feb 03 04:05:06.1234 2000"},
			[3]string{"Tue Feb 29 04:05:06.1234 0001 BC", "Tue 29 Feb 04:05:06.1234 0001 BC", "Tue Feb 29 04:05:06.1234 0001 BC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for order, name := range [...]string{"MDY", "DMY", "YMD"} {
				t.Run(name, func(t *testing.T) {
					function := timestampOutputCallable(t, builtin.ResolveOptions{DateStyle: tc.style, DateOrder: builtin.DateOrder(order)})
					requireTimestampOutput(t, function, 33*86_400_000_000+14_706_123_400, tc.want[order])
					requireTimestampOutput(t, function, -730426*86_400_000_000+14_706_123_400, tc.bc[order])
					requireTimestampOutput(t, function, math.MinInt64, "-infinity")
					requireTimestampOutput(t, function, math.MaxInt64, "infinity")
				})
			}
		})
	}
}

func TestTimestampOutputFiniteBoundsAndRollovers(t *testing.T) {
	function := timestampOutputCallable(t, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name   string
		micros int64
		want   string
	}{
		{"minimum", -211813488000000000, "4714-11-24 00:00:00 BC"},
		{"last BC microsecond", -730119*86_400_000_000 - 1, "0001-12-31 23:59:59.999999 BC"},
		{"first AD microsecond", -730119 * 86_400_000_000, "0001-01-01 00:00:00"},
		{"before epoch", -1, "1999-12-31 23:59:59.999999"},
		{"epoch", 0, "2000-01-01 00:00:00"},
		{"leading fraction zeros", 1, "2000-01-01 00:00:00.000001"},
		{"before second", 999999, "2000-01-01 00:00:00.999999"},
		{"whole second", 1000000, "2000-01-01 00:00:01"},
		{"whole day", 86_400_000_000, "2000-01-02 00:00:00"},
		{"maximum", 9223371331199999999, "294276-12-31 23:59:59.999999"},
		{"rounded past maximum", 9223371331200000000, "294277-01-01 00:00:00"},
		{"largest finite representation", math.MaxInt64 - 1, "294277-01-09 04:00:54.775806"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTimestampOutput(t, function, tc.micros, tc.want)
		})
	}
}

func TestTimestampOutputRejectsInvalidFiniteRepresentation(t *testing.T) {
	function := timestampOutputCallable(t, builtin.ResolveOptions{})
	for _, micros := range []int64{-211813488000000001, math.MinInt64 + 1} {
		_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: micros}}, builtin.CallBudget{OutputBytes: 100, MemoryBytes: 100})
		requireSQLState(t, err, "22008")
	}
}

func TestTimestampOutputAdmitsExactPayloadAndMemory(t *testing.T) {
	function := timestampOutputCallable(t, builtin.ResolveOptions{DateStyle: builtin.DatePostgres})
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{-211813488000000000 + 1, "Mon Nov 24 00:00:00.000001 4714 BC"},
		{9223371331199999999, "Sun Dec 31 23:59:59.999999 294276"},
		{math.MinInt64, "-infinity"},
	} {
		size := int64(len(tc.want))
		for _, budget := range []builtin.CallBudget{
			{OutputBytes: size - 1, MemoryBytes: uint64(size)},
			{OutputBytes: size, MemoryBytes: uint64(size - 1)},
			{OutputBytes: 0, MemoryBytes: uint64(size)},
		} {
			_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: tc.micros}}, budget)
			requireSQLState(t, err, "54000")
		}
		requireTimestampOutput(t, function, tc.micros, tc.want)
	}
}

func TestTimestampOutputCapturesSettingsAndRetainsOwnedResults(t *testing.T) {
	options := builtin.ResolveOptions{DateStyle: builtin.DateSQL, DateOrder: builtin.DateDMY}
	first := timestampOutputCallable(t, options)
	retained := requireTimestampOutput(t, first, 33*86_400_000_000, "03/02/2000 00:00:00")
	options.DateStyle, options.DateOrder = builtin.DatePostgres, builtin.DateYMD
	second := timestampOutputCallable(t, options)
	requireTimestampOutput(t, second, 33*86_400_000_000, "Thu Feb 03 00:00:00 2000")
	requireTimestampOutput(t, first, -1, "31/12/1999 23:59:59.999999")
	if retained.StrVal != "03/02/2000 00:00:00" {
		t.Fatalf("subsequent output changed retained result to %q", retained.StrVal)
	}
}

func TestTimestampOutputStrictNullAndCancellation(t *testing.T) {
	function := timestampOutputCallable(t, builtin.ResolveOptions{})
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeTimestamp)}, builtin.CallBudget{})
	if err != nil || !value.IsNull || value.Type != model.TypeString {
		t.Fatalf("timestamp_out(NULL) = %v, %v; want NULL cstring", value, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, []model.Value{{Type: model.TypeTimestamp}}, builtin.CallBudget{OutputBytes: 19, MemoryBytes: 19})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled timestamp_out = %v; want context cancellation", err)
	}
}
