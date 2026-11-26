package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func dateOutputCallable(t *testing.T, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(1085, options)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireDateOutput(t *testing.T, function builtin.Callable, days int64, want string) model.Value {
	t.Helper()
	value, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: days}}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: uint64(len(want))})
	if err != nil || value.IsNull || value.Type != model.TypeString || value.StrVal != want {
		t.Fatalf("date_out(%d) = %q (type %v, null %v), %v; want %q", days, value.StrVal, value.Type, value.IsNull, err, want)
	}
	return value
}

func TestDateOutputStylesAndOrders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style builtin.DateStyle
		want  [3]string
		bc    [3]string
	}{
		{"ISO", builtin.DateISO, [3]string{"2000-02-03", "2000-02-03", "2000-02-03"}, [3]string{"0001-02-29 BC", "0001-02-29 BC", "0001-02-29 BC"}},
		{"SQL", builtin.DateSQL, [3]string{"02/03/2000", "03/02/2000", "02/03/2000"}, [3]string{"02/29/0001 BC", "29/02/0001 BC", "02/29/0001 BC"}},
		{"Postgres", builtin.DatePostgres, [3]string{"02-03-2000", "03-02-2000", "02-03-2000"}, [3]string{"02-29-0001 BC", "29-02-0001 BC", "02-29-0001 BC"}},
		{"German", builtin.DateGerman, [3]string{"03.02.2000", "03.02.2000", "03.02.2000"}, [3]string{"29.02.0001 BC", "29.02.0001 BC", "29.02.0001 BC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for order, name := range [...]string{"MDY", "DMY", "YMD"} {
				t.Run(name, func(t *testing.T) {
					function := dateOutputCallable(t, builtin.ResolveOptions{DateStyle: tc.style, DateOrder: builtin.DateOrder(order)})
					requireDateOutput(t, function, 33, tc.want[order])
					requireDateOutput(t, function, -730426, tc.bc[order])
					requireDateOutput(t, function, math.MinInt32, "-infinity")
					requireDateOutput(t, function, math.MaxInt32, "infinity")
				})
			}
		})
	}
}

func TestDateOutputFiniteBoundsAndEraTransition(t *testing.T) {
	function := dateOutputCallable(t, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name string
		days int64
		want string
	}{
		{"minimum", -2451545, "4714-11-24 BC"},
		{"last BC day", -730120, "0001-12-31 BC"},
		{"first AD day", -730119, "0001-01-01"},
		{"epoch", 0, "2000-01-01"},
		{"maximum", 2145031948, "5874897-12-31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDateOutput(t, function, tc.days, tc.want)
		})
	}
}

func TestDateOutputRejectsInvalidFiniteRepresentation(t *testing.T) {
	function := dateOutputCallable(t, builtin.ResolveOptions{})
	for _, days := range []int64{math.MinInt64, math.MaxInt64} {
		_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: days}}, builtin.CallBudget{OutputBytes: 100, MemoryBytes: 100})
		requireSQLState(t, err, "22008")
	}
}

func TestDateOutputAdmitsExactPayloadAndMemory(t *testing.T) {
	function := dateOutputCallable(t, builtin.ResolveOptions{})
	for _, tc := range []struct {
		days int64
		want string
	}{
		{-2451545, "4714-11-24 BC"},
		{2145031948, "5874897-12-31"},
		{-3000000, "11753007-05-03"},
		{-2451547, "4714-11-22 BC"},
		{2145031949, "5874898-01-01"},
		{math.MinInt32, "-infinity"},
	} {
		size := int64(len(tc.want))
		for _, budget := range []builtin.CallBudget{
			{OutputBytes: size - 1, MemoryBytes: uint64(size)},
			{OutputBytes: size, MemoryBytes: uint64(size - 1)},
		} {
			_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: tc.days}}, budget)
			requireSQLState(t, err, "54000")
		}
		requireDateOutput(t, function, tc.days, tc.want)
	}
}

func TestDateOutputCapturesSettingsAndRetainsOwnedResults(t *testing.T) {
	options := builtin.ResolveOptions{DateStyle: builtin.DateSQL, DateOrder: builtin.DateDMY}
	first := dateOutputCallable(t, options)
	retained := requireDateOutput(t, first, 33, "03/02/2000")
	options.DateStyle, options.DateOrder = builtin.DatePostgres, builtin.DateYMD
	second := dateOutputCallable(t, options)
	requireDateOutput(t, second, 33, "02-03-2000")
	requireDateOutput(t, first, -2451545, "24/11/4714 BC")
	if retained.StrVal != "03/02/2000" {
		t.Fatalf("subsequent output changed retained result to %q", retained.StrVal)
	}
}

func TestDateOutputStrictNullAndCancellation(t *testing.T) {
	function := dateOutputCallable(t, builtin.ResolveOptions{})
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeTimestamp)}, builtin.CallBudget{})
	if err != nil || !value.IsNull || value.Type != model.TypeString {
		t.Fatalf("date_out(NULL) = %v, %v; want NULL cstring", value, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, []model.Value{{Type: model.TypeTimestamp}}, builtin.CallBudget{OutputBytes: 10, MemoryBytes: 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled date_out = %v; want context cancellation", err)
	}
}

func TestDateOutputRejectsInvalidSettingsAtResolution(t *testing.T) {
	for _, options := range []builtin.ResolveOptions{
		{DateStyle: builtin.DateStyle(255)},
		{DateOrder: builtin.DateOrder(255)},
	} {
		_, err := builtin.Resolve(1085, options)
		requireSQLState(t, err, "22023")
	}
}
