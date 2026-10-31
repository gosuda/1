package series_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/series"
)

var numeric = model.TypeLayout{Type: model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}}

func numericValue(value string) model.Value {
	return model.Value{Type: model.TypeNumeric, StrVal: value}
}

func numericOptions(q *runtime.Query, program *expr.Program) series.Options {
	opts := options(q, program)
	opts.Layout, opts.WorkBytes = numeric, 64<<10
	return opts
}

func TestNumericSeriesEarlyCloseReleasesState(t *testing.T) {
	q := query(t, t.Context())
	opts := numericOptions(q, constants(t, numeric, numericValue("0.1"), numericValue("1e100"), numericValue("0.1")))
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 || value.StrVal != "0.1" {
		t.Fatalf("first row = %+v", value)
	}
	out.Reset()
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNumericSeriesStateBudgetIsDemandDriven(t *testing.T) {
	for _, null := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-null", true: "null"}[null], func(t *testing.T) {
			q := query(t, t.Context())
			start := numericValue("0")
			start.IsNull = null
			opts := numericOptions(q, constants(t, numeric, start, numericValue("1")))
			opts.WorkBytes = 1
			cursor := open(t, t.Context(), opts)
			var out columnar.Batch
			err := cursor.Next(t.Context(), &out)
			if null {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("NULL series allocated numeric state: %v", err)
				}
			} else if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("numeric state ignored budget: %v", err)
			}
			if out.Rows() != 0 {
				t.Fatalf("budget failure published %d rows", out.Rows())
			}
			released(t, q)
		})
	}
}

func TestNumericSeriesNullStillEvaluatesStep(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, numeric)
	sentinel := errors.New("step expression failed")
	step := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return model.Value{}, sentinel
	})
	start := numericValue("")
	start.IsNull = true
	cursor := open(t, t.Context(), numericOptions(q, f.compile(f.constant(start), f.constant(numericValue("1")), step)))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, sentinel) {
		t.Fatalf("step error = %v", err)
	}
	released(t, q)
}

func TestNumericSeriesCancellationReleasesState(t *testing.T) {
	q := query(t, t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cursor := open(t, ctx, numericOptions(q, constants(t, numeric, numericValue("0"), numericValue("1e100"))))
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	cancel()
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled series = %v", err)
	}
	released(t, q)
}

func TestNumericSeriesOutputFailureReleasesState(t *testing.T) {
	q := query(t, t.Context())
	opts := numericOptions(q, constants(t, numeric, numericValue("0.1"), numericValue("1000"), numericValue("0.1")))
	opts.BatchRows = 64
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	usage := q.Usage().Output
	blocker, err := q.TryReserve(runtime.OutputMemory, usage.Limit-usage.Live)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	out.MaxRows = 64
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("output pressure = %v", err)
	}
	if out.Rows() != 0 {
		t.Fatalf("failure published %d rows", out.Rows())
	}
	blocker.Release()
	released(t, q)
}

func TestNumericSeriesRejectsNoncanonicalResult(t *testing.T) {
	q := query(t, t.Context())
	opts := numericOptions(q, constants(t, numeric, numericValue("1"), numericValue("2")))
	opts.Layout.Type.Modifier = 4
	cursor, err := series.New(t.Context(), opts)
	if !errors.Is(err, columnar.ErrValueLayout) || cursor != nil {
		t.Fatalf("noncanonical result = %v, %v", cursor, err)
	}
	released(t, q)
}
