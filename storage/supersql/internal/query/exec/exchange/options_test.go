package exchange

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSourcesRejectInvalidConfigurationBeforePull(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	for _, tc := range []struct {
		name   string
		change func(*Options)
	}{
		{"missing-query", func(o *Options) { o.Query = nil }},
		{"zero-rows", func(o *Options) { o.BatchRows = 0 }},
		{"negative-rows", func(o *Options) { o.BatchRows = -1 }},
		{"zero-batch-bytes", func(o *Options) { o.BatchBytes = 0 }},
		{"zero-queue-bytes", func(o *Options) { o.QueueBytes = 0 }},
		{"zero-metadata-bytes", func(o *Options) { o.MetadataBytes = 0 }},
		{"negative-estimate", func(o *Options) { o.EstimatedRows = -1 }},
		{"nan-estimate", func(o *Options) { o.EstimatedRows = math.NaN() }},
		{"infinite-estimate", func(o *Options) { o.EstimatedRows = math.Inf(1) }},
		{"invalid-layout", func(o *Options) { o.Layouts = []model.TypeLayout{{}} }},
		{"missing-array-child", func(o *Options) {
			o.Layouts = []model.TypeLayout{{Type: model.Type{ID: 1007, Storage: model.TypeArray}}}
		}},
		{"overflow", func(o *Options) { o.QueueBytes = math.MaxUint64 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := transportOptions(q)
			tc.change(&o)
			input := transportInts(1)
			if _, err := NewSources(t.Context(), []storage.BatchCursor{input}, o); err == nil {
				t.Fatal("invalid options accepted")
			}
			if input.closes.Load() != 1 || input.calls.Load() != 0 || q.Usage().Work.Live != 0 {
				t.Fatal("failed constructor leaked or pulled input")
			}
		})
	}
}

func TestSourcesQueueBudgetIsAggregate(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	o := transportOptions(q)
	o.QueueBytes = 4096
	inputs := []storage.BatchCursor{transportInts(1), transportInts(2), transportInts(3), transportInts(4)}
	s, err := NewSources(t.Context(), inputs, o)
	if err != nil {
		t.Fatal(err)
	}
	for i := range inputs {
		out := columnar.Batch{MaxRows: 1}
		if err := s.Next(t.Context(), i, &out); err != nil {
			t.Fatal(err)
		}
		if got := transportValue(t, &out, 0); got != int64(i+1) {
			t.Fatalf("input %d = %d", i, got)
		}
	}
	if live := q.Usage().Work.Live; live > int64(o.QueueBytes+o.MetadataBytes) {
		t.Fatalf("aggregate queue expanded: %d", live)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	o.QueueBytes = 1
	left, right := transportInts(1), transportInts(2)
	if _, err := NewSources(t.Context(), []storage.BatchCursor{left, right}, o); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("queue cannot hold one slot/input: %v", err)
	}
	if left.closes.Load() != 1 || right.closes.Load() != 1 {
		t.Fatal("queue rejection leaked children")
	}
}

func TestSourcesExactWorkQuota(t *testing.T) {
	baseline, _ := transportRuntime(t, runtime.QueryConfig{})
	o := transportOptions(baseline)
	s, err := NewSources(t.Context(), []storage.BatchCursor{transportInts(1)}, o)
	if err != nil {
		t.Fatal(err)
	}
	required := baseline.Usage().Work.Live
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{0, -1} {
		q, _ := transportRuntime(t, runtime.QueryConfig{WorkBytes: required + delta})
		o.Query = q
		input := transportInts(1)
		s, err := NewSources(t.Context(), []storage.BatchCursor{input}, o)
		if delta == -1 {
			if !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("one byte short: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			out := columnar.Batch{MaxRows: 1}
			if err := s.Next(t.Context(), 0, &out); err != nil {
				t.Fatal(err)
			}
			if transportValue(t, &out, 0) != 1 {
				t.Fatal("exact quota changed row")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if input.closes.Load() != 1 || q.Usage().Work.Live != 0 {
			t.Fatal("quota boundary leaked resources")
		}
	}
}

func TestSourcesMetadataBoundsNestedLayoutCopies(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	o := transportOptions(q)
	o.MetadataBytes = 1
	input := transportInts(1)
	if _, err := NewSources(t.Context(), []storage.BatchCursor{input}, o); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("metadata quota: %v", err)
	}
	if input.closes.Load() != 1 || q.Usage().Work.Live != 0 {
		t.Fatal("metadata rejection leaked input")
	}
}

func TestSourcesRejectNegativeMaxRowsWithoutPull(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	input := transportInts(1)
	s, err := NewSources(t.Context(), []storage.BatchCursor{input}, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Next(t.Context(), 0, &columnar.Batch{MaxRows: -1}); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("negative demand: %v", err)
	}
	if input.calls.Load() != 0 || input.closes.Load() != 1 {
		t.Fatal("invalid demand pulled or leaked input")
	}
}

func TestSourcesCanceledConstructionRetainsCleanupError(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cleanup := errors.New("cleanup failure")
	input := transportInts(1)
	input.cleanup = func() error { return cleanup }
	_, err := NewSources(ctx, []storage.BatchCursor{input}, transportOptions(q))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cleanup) {
		t.Fatalf("canceled construction: %v", err)
	}
}

func TestSourcesExactMetadataQuota(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	o := transportOptions(q)
	s, err := NewSources(t.Context(), []storage.BatchCursor{transportInts(1)}, o)
	if err != nil {
		t.Fatal(err)
	}
	required := uint64(q.Usage().Work.Live) - o.BatchBytes
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []uint64{required, required - 1} {
		o.MetadataBytes = limit
		input := transportInts(1)
		s, err := NewSources(t.Context(), []storage.BatchCursor{input}, o)
		if limit == required {
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("one metadata byte short: %v", err)
		}
		if input.closes.Load() != 1 || q.Usage().Work.Live != 0 {
			t.Fatal("metadata boundary leaked input or memory")
		}
	}
}
