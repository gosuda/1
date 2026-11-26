package series_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/series"
)

func TestTimestamptzArgumentsEvaluateOnceAcrossDemand(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Modifier: -1, Storage: model.TypeTimestamp}}
	f := newFixture(t, layout)
	interval, err := f.builder.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1186, Modifier: -1, Storage: model.TypeInterval}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := f.builder.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}})
	if err != nil {
		t.Fatal(err)
	}
	var calls [4]int
	argument := func(index int, value model.Value) func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls[index]++
			return value, nil
		}
	}
	start := f.function(argument(0, model.Value{Type: model.TypeTimestamp, IntVal: 0}))
	stop := f.function(argument(1, model.Value{Type: model.TypeTimestamp, IntVal: 2 * 86400000000}))
	step := f.functionWithLayout(interval, argument(2, model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Days: 1}}))
	zone := f.functionWithLayout(text, argument(3, model.StringVal("UTC")))
	q := query(t, t.Context())
	cursor := open(t, t.Context(), series.Options{Arguments: f.compile(start, stop, step, zone), Layout: layout, Query: q, BatchRows: 3, BatchBytes: 4096, WorkBytes: 64 << 10, Ordinality: true})
	var next int64
	batch := columnar.Batch{MaxRows: 1}
	for {
		err := cursor.Next(t.Context(), &batch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if batch.Length != 1 || batch.Columns[0].Int64[0] != next*86400000000 || batch.Columns[1].Int64[0] != next+1 {
			t.Fatalf("row %d: %+v", next, batch)
		}
		next++
	}
	if next != 3 || calls != [4]int{1, 1, 1, 1} {
		t.Fatalf("rows=%d argument evaluations=%v", next, calls)
	}
	released(t, q)
}

func TestTimestamptzNullStillEvaluatesZoneExpression(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Modifier: -1, Storage: model.TypeTimestamp}}
	f := newFixture(t, layout)
	interval, err := f.builder.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1186, Modifier: -1, Storage: model.TypeInterval}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := f.builder.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}})
	if err != nil {
		t.Fatal(err)
	}
	start := f.constant(model.Value{Type: model.TypeTimestamp, IsNull: true})
	stop := f.constant(model.Value{Type: model.TypeTimestamp})
	step := f.functionWithLayout(interval, func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return model.Value{Type: model.TypeInterval}, nil
	})
	failure := errors.New("zone expression failed")
	zone := f.functionWithLayout(text, func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return model.Value{}, failure
	})
	q := query(t, t.Context())
	cursor := open(t, t.Context(), series.Options{Arguments: f.compile(start, stop, step, zone), Layout: layout, Query: q, BatchRows: 3, BatchBytes: 4096, WorkBytes: 64 << 10})
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); !errors.Is(err, failure) || batch.Length != 0 {
		t.Fatalf("NULL suppressed argument evaluation: rows=%d error=%v", batch.Length, err)
	}
	released(t, q)
}
