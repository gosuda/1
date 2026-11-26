package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func zonedSeriesInstant(t *testing.T, text string) int64 {
	t.Helper()
	value, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return value.UnixMicro() - 946684800000000
}

func TestTimestamptzSeriesCalendarSteps(t *testing.T) {
	for _, test := range []struct {
		name, zone string
		explicit   bool
		step       model.Interval
		want       []string
	}{
		{"spring day", "America/New_York", false, model.Interval{Days: 1}, []string{"2024-03-09T12:00:00-05:00", "2024-03-10T12:00:00-04:00", "2024-03-11T12:00:00-04:00"}},
		{"spring elapsed hours", "America/New_York", false, model.Interval{Microseconds: 86400000000}, []string{"2024-03-09T12:00:00-05:00", "2024-03-10T13:00:00-04:00", "2024-03-11T13:00:00-04:00"}},
		{"spring missing local time", "America/New_York", true, model.Interval{Days: 1}, []string{"2024-03-09T02:30:00-05:00", "2024-03-10T03:30:00-04:00", "2024-03-11T03:30:00-04:00"}},
		{"fall repeated local time", "America/New_York", true, model.Interval{Days: 1}, []string{"2024-11-02T01:30:00-04:00", "2024-11-03T01:30:00-05:00", "2024-11-04T01:30:00-05:00"}},
		{"descending fall", "America/New_York", false, model.Interval{Days: -1}, []string{"2024-11-04T12:00:00-05:00", "2024-11-03T12:00:00-05:00", "2024-11-02T12:00:00-04:00"}},
		{"month clamp across DST", "America/New_York", true, model.Interval{Months: 1}, []string{"2024-01-31T12:00:00-05:00", "2024-02-29T12:00:00-05:00", "2024-03-29T12:00:00-04:00"}},
		{"dynamic abbreviation full zone", "MSK", true, model.Interval{Days: 1}, []string{"2010-03-27T12:00:00+03:00", "2010-03-28T12:00:00+04:00", "2010-03-29T12:00:00+04:00"}},
		{"POSIX rules", "abc5def,M3.2.0,M11.1.0", false, model.Interval{Days: 1}, []string{"2024-03-09T12:00:00-05:00", "2024-03-10T12:00:00-04:00", "2024-03-11T12:00:00-04:00"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := numericRangeQuery(t, 4096)
			options := builtin.TimestamptzSeriesOptions{TimeZone: builtin.TimeZoneContext{Name: test.zone}, MemoryBytes: 4096, Allocator: query.Allocator(runtime.WorkMemory)}
			if test.explicit {
				options.TimeZone.Name = "UTC"
				options.ExplicitZone = &test.zone
			}
			series, err := builtin.NewTimestamptzSeries(t.Context(), zonedSeriesInstant(t, test.want[0]), zonedSeriesInstant(t, test.want[len(test.want)-1]), test.step, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := series.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := series.Advance(); err == nil {
				t.Fatal("Advance accepted an unprepared row")
			}
			for _, text := range test.want {
				want := zonedSeriesInstant(t, text)
				for range 3 {
					value, ok, err := series.Peek(t.Context())
					if err != nil || !ok || value.Type != model.TypeTimestamp || value.IsNull || value.IntVal != want {
						t.Fatalf("Peek = %+v, %v, %v; want %s (%d)", value, ok, err, text, want)
					}
				}
				if err := series.Advance(); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, err := series.Peek(t.Context()); err != nil || ok {
				t.Fatalf("exhaustion = %v, %v", ok, err)
			}
			if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("exhaustion retained work: %+v", usage)
			}
		})
	}
}

func TestTimestamptzSeriesRejectsInvalidZonesAndSteps(t *testing.T) {
	zone := "no/such/zone"
	query := numericRangeQuery(t, 4096)
	_, err := builtin.NewTimestamptzSeries(t.Context(), math.MaxInt64, math.MaxInt64, model.Interval{Days: 1}, builtin.TimestamptzSeriesOptions{ExplicitZone: &zone, MemoryBytes: 4096, Allocator: query.Allocator(runtime.WorkMemory)})
	requireSQLState(t, err, "22023")
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("invalid zone retained work: %+v", usage)
	}
	for _, step := range []model.Interval{{}, {Months: 1, Days: -30}, {Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}} {
		_, err := builtin.NewTimestamptzSeries(t.Context(), 0, 0, step, builtin.TimestamptzSeriesOptions{MemoryBytes: 4096, Allocator: query.Allocator(runtime.WorkMemory)})
		requireSQLState(t, err, "22023")
		if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
			t.Fatalf("constructor failure retained work: %+v", usage)
		}
	}
}

func TestTimestamptzSeriesInfiniteEndpointCancellation(t *testing.T) {
	for _, endpoint := range []int64{math.MinInt64, math.MaxInt64} {
		query := numericRangeQuery(t, 4096)
		options := builtin.TimestamptzSeriesOptions{MemoryBytes: 4096, Allocator: query.Allocator(runtime.WorkMemory)}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if series, err := builtin.NewTimestamptzSeries(ctx, endpoint, endpoint, model.Interval{Days: 1}, options); series != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled construction = %v, %v", series, err)
		}
		series, err := builtin.NewTimestamptzSeries(t.Context(), endpoint, endpoint, model.Interval{Days: 1}, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := series.Close(); err != nil {
				t.Error(err)
			}
		})
		admitted := query.Usage().Work
		for range 1000 {
			value, ok, err := series.Peek(t.Context())
			if err != nil || !ok || value.IntVal != endpoint {
				t.Fatalf("infinite Peek = %+v, %v, %v", value, ok, err)
			}
			if err := series.Advance(); err != nil {
				t.Fatal(err)
			}
		}
		if usage := query.Usage().Work; usage.Live != admitted.Live || usage.Reservations != admitted.Reservations {
			t.Fatalf("iteration grew retained work: before %+v after %+v", admitted, usage)
		}
		if _, ok, err := series.Peek(t.Context()); err != nil || !ok {
			t.Fatalf("pending Peek = %v, %v", ok, err)
		}
		if _, ok, err := series.Peek(ctx); ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled pending Peek = %v, %v", ok, err)
		}
		if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
			t.Fatalf("cancellation retained work: %+v", usage)
		}
	}
}

func TestTimestamptzSeriesStateAdmissionAndFinalIncrement(t *testing.T) {
	bytes := uint64(unsafe.Sizeof(builtin.TimestamptzSeries{}))
	for _, test := range []struct {
		name          string
		local, shared uint64
		start         int64
		step          model.Interval
		state         string
	}{
		{"local quota", bytes - 1, 4096, 0, model.Interval{Days: 1}, "54000"},
		{"shared quota", bytes, bytes - 1, 0, model.Interval{Days: 1}, ""},
		{"final increment overflow", bytes, 4096, 9223371331199999999, model.Interval{Microseconds: 1}, "22008"},
		{"exact admission early close", bytes, bytes, 0, model.Interval{Days: 1}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := numericRangeQuery(t, test.shared)
			series, err := builtin.NewTimestamptzSeries(t.Context(), test.start, test.start, test.step, builtin.TimestamptzSeriesOptions{MemoryBytes: test.local, Allocator: query.Allocator(runtime.WorkMemory)})
			if err == nil {
				var ok bool
				_, ok, err = series.Peek(t.Context())
				if test.state != "" && ok {
					t.Fatal("final increment error exposed a row")
				}
				if test.state == "" && (!ok || err != nil) {
					t.Fatalf("exact admission Peek = %v, %v", ok, err)
				}
				if closeErr := series.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if closeErr := series.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if test.state != "" {
				requireSQLState(t, err, test.state)
			} else if test.shared < bytes && !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("shared admission = %v", err)
			} else if test.shared >= bytes && err != nil {
				t.Fatal(err)
			}
			if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("terminal state retained work: %+v", usage)
			}
		})
	}
}
