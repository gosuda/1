package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestamptzOutputDateStylesAndZoneSuffix(t *testing.T) {
	value, err := timestampConstruct(t, 2000, 3, 3, 4, 5, 6.123456)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		style builtin.DateStyle
		order builtin.DateOrder
		want  string
	}{
		{builtin.DateISO, builtin.DateMDY, "2000-03-02 23:05:06.123456-05"},
		{builtin.DateSQL, builtin.DateMDY, "03/02/2000 23:05:06.123456 EST"},
		{builtin.DateSQL, builtin.DateDMY, "02/03/2000 23:05:06.123456 EST"},
		{builtin.DatePostgres, builtin.DateDMY, "Thu 02 Mar 23:05:06.123456 2000 EST"},
		{builtin.DateGerman, builtin.DateMDY, "02.03.2000 23:05:06.123456 EST"},
	} {
		call, err := builtin.Resolve(1151, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}, DateStyle: tc.style, DateOrder: tc.order})
		if err != nil {
			t.Fatal(err)
		}
		got, err := call.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: uint64(len(tc.want))})
		if err != nil || got.StrVal != tc.want {
			t.Fatalf("style%d order%d: %v,%v; want%q", tc.style, tc.order, got, err, tc.want)
		}
	}
}

func TestTimestamptzOutputLocalDisplayCanPrecedeTimestampRange(t *testing.T) {
	call, err := builtin.Resolve(1151, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "<-30>30"}, DateStyle: builtin.DatePostgres})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{timestampDatum(-211813488000000000)}
	const want = "Sat Nov 22 18:00:00 4714 -30 BC"
	got, err := call.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: uint64(len(want))})
	if err != nil || got.StrVal != want {
		t.Fatalf("local display below timestamp range: %v,%v", got, err)
	}
	_, err = call.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: uint64(len(want))})
	requireSQLState(t, err, "54000")
}

func TestTimestamptzOutputPOSIXLabelCaseAndByteClipping(t *testing.T) {
	for _, tc := range []struct{ zone, want string }{
		{"<abcdefghijklmnop>0", "01/01/2000 00:00:00 ABCDEFGHIJ"},
		{"<aαβγδε>0", "01/01/2000 00:00:00 Aαβγδ\xce"},
	} {
		call, err := builtin.Resolve(1151, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: tc.zone}, DateStyle: builtin.DateSQL})
		if err != nil {
			t.Fatal(err)
		}
		got, err := call.Call(t.Context(), []model.Value{timestampDatum(0)}, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64})
		if err != nil || got.StrVal != tc.want {
			t.Fatalf("POSIX output bytes: %q, %v; want %q", got.StrVal, err, tc.want)
		}
	}
}
