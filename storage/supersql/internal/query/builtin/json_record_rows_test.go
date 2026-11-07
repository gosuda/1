package builtin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func jsonRecordRowsPlan(t *testing.T, field model.Type) builtin.RowSourcePlan {
	t.Helper()
	layout := model.TypeLayout{
		Type: model.Type{ID: 81000, Storage: model.TypeComposite, Modifier: -1},
		FieldNames: []string{"a"},
		Children: []model.TypeLayout{{Type: field}},
	}
	plan, err := builtin.ResolveJSONRecordRows(t.Context(), 3491, []model.TypeLayout{{Type: model.Type{ID: 3802, Storage: model.TypeJSONB, Modifier: -1}}}, layout, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestJSONRecordRowsReleaseReservationsAtEndAndClose(t *testing.T) {
	plan := jsonRecordRowsPlan(t, model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1})
	for _, end := range []string{"exhaustion", "early close", "cancellation"} {
		t.Run(end, func(t *testing.T) {
			query := numericRangeQuery(t, 1<<20)
			options := builtin.RowSourceOptions{MaxInputBytes: 4096, MaxRowBytes: 4096, MaxMemoryBytes: 1<<20, Allocator: query.Allocator(runtime.WorkMemory)}
			iterator, err := plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":7},{"a":9}]`}}, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { if err := iterator.Close(); err != nil { t.Error(err) } })
			row, ok, err := iterator.Next(t.Context())
			if err != nil || !ok || len(row) != 1 || row[0].IntVal != 7 {
				t.Fatalf("first row = %v, %t, %v", row, ok, err)
			}
			switch end {
			case "exhaustion":
				row, ok, err = iterator.Next(t.Context())
				if err != nil || !ok || len(row) != 1 || row[0].IntVal != 9 {
					t.Fatalf("second row = %v, %t, %v", row, ok, err)
				}
				_, ok, err = iterator.Next(t.Context())
				if err != nil || ok {
					t.Fatalf("end = %t, %v", ok, err)
				}
			case "early close":
				if err := iterator.Close(); err != nil {
					t.Fatal(err)
				}
			case "cancellation":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, _, err = iterator.Next(ctx)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled next = %v", err)
				}
			}
			if usage := query.Usage(); usage.Work.Live != 0 {
				t.Fatalf("retained bytes after %s = %d", end, usage.Work.Live)
			}
			if _, ok, err := iterator.Next(t.Context()); err != nil || ok {
				t.Fatalf("next after %s = %t, %v", end, ok, err)
			}
		})
	}
}

func TestJSONRecordRowsPreflightFailureExposesNoRowsAndReleasesMemory(t *testing.T) {
	plan := jsonRecordRowsPlan(t, model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1})
	query := numericRangeQuery(t, 1<<20)
	options := builtin.RowSourceOptions{MaxInputBytes: 4096, MaxRowBytes: 4096, MaxMemoryBytes: 1<<20, Allocator: query.Allocator(runtime.WorkMemory)}
	iterator, err := plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":7},{"a":"invalid"}]`}}, options)
	if iterator != nil {
		_ = iterator.Close()
		t.Fatal("preflight exposed an iterator before a later conversion failure")
	}
	requireSQLState(t, err, "22P02")
	if usage := query.Usage(); usage.Work.Live != 0 {
		t.Fatalf("retained bytes after failed open = %d", usage.Work.Live)
	}
}

type jsonRecordTemporalSource struct {
	fixedTemporalInstant
	calls int
	err error
}

func (source *jsonRecordTemporalSource) TransactionTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	source.calls++
	if source.err != nil {
		return 0, source.err
	}
	if source.calls > 1 {
		return 0, errors.New("transaction timestamp capability consumed twice")
	}
	return source.fixedTemporalInstant.TransactionTimestamp(ctx)
}

func TestJSONRecordRowsReplayReusesTemporalSnapshot(t *testing.T) {
	plan := jsonRecordRowsPlan(t, model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1})
	source := &jsonRecordTemporalSource{fixedTemporalInstant: 689578212123456}
	iterator, err := plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":"now"},{"a":"now"}]`}}, builtin.RowSourceOptions{MaxInputBytes: 4096, MaxRowBytes: 4096, MaxMemoryBytes: 1<<20, Temporal: source})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { if err := iterator.Close(); err != nil { t.Error(err) } }()
	for i := range 2 {
		row, ok, err := iterator.Next(t.Context())
		if err != nil || !ok || len(row) != 1 || row[0].IsNull || row[0].IntVal != int64(source.fixedTemporalInstant) {
			t.Fatalf("row %d = %v, %t, %v", i, row, ok, err)
		}
	}
	if source.calls != 1 {
		t.Fatalf("temporal capability consumed %d times", source.calls)
	}
}

func TestJSONRecordRowsPreserveTemporalDependencyFailure(t *testing.T) {
	plan := jsonRecordRowsPlan(t, model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1})
	failure := errors.New("temporal source unavailable")
	_, err := plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":"now"}]`}}, builtin.RowSourceOptions{MaxInputBytes: 4096, MaxRowBytes: 4096, MaxMemoryBytes: 1<<20, Temporal: &jsonRecordTemporalSource{err: failure}})
	if !errors.Is(err, failure) {
		t.Fatalf("temporal input error = %v, want %v", err, failure)
	}
}
