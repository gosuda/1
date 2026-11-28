package aggregate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

// Match the session's aggregate admission limits without changing query budgets.
func percentileAdmissionOptions(t *testing.T, q *runtime.Query, input, output []model.TypeLayout, calls []aggregate.Call) aggregate.Options {
	t.Helper()
	o := options(t, q, input, output, nil, calls)
	o.MaxRecordBytes = 64 << 10
	o.GroupBytes = 16 << 20
	o.WorkBytes = 64 << 20
	o.PartitionMemoryBytes = 4 << 20
	o.MaxPartitions = 32
	return o
}

func TestWideFloatPercentileAdmission(t *testing.T) {
	for _, test := range []struct {
		name           string
		repeated       bool
		rows           int
		spill          bool
		fractionOffset float64
		wantOffset     float64
	}{
		{name: "distinct", rows: 2},
		{name: "duplicate", repeated: true, rows: 2},
		{name: "distinct spill", rows: 1025, spill: true},
		{name: "duplicate spill", repeated: true, rows: 1025, spill: true},
		{name: "distinct interpolation spill", rows: 1025, spill: true, fractionOffset: 1.0 / 2048, wantOffset: 1.0 / 16},
		{name: "duplicate interpolation spill", repeated: true, rows: 1025, spill: true, fractionOffset: 1.0 / 2048, wantOffset: 1.0 / 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, false, 64<<20)
			calls := make([]aggregate.Call, 128)
			outputs := make([]model.TypeLayout, len(calls))
			evaluators := make([]*fixedDirect, len(calls))
			for i := range calls {
				fraction := float64(i) / 128
				if test.repeated {
					fraction = .5
				}
				// On the 1025-point grid, 1/2048 shifts rank by exactly one half.
				fraction += test.fractionOffset
				evaluators[i] = &fixedDirect{values: []model.Value{model.Float64Val(fraction)}}
				calls[i] = aggregate.Call{
					Function: 3974, Arguments: []int{0}, OrderBy: []aggregate.OrderKey{{Column: 0}},
					DirectLayouts: []model.TypeLayout{float8}, DirectEvaluator: evaluators[i],
				}
				outputs[i] = float8
			}
			values := make([][]model.Value, test.rows)
			for i := range values {
				index := i
				if test.spill {
					index = len(values) - 1 - i
				}
				// Both grids span [0, 128] exactly; spill cases arrive descending.
				values[i] = []model.Value{model.Float64Val(float64(index) * 128 / float64(len(values)-1))}
			}
			input := newSource(t, []model.TypeLayout{float8}, values)
			cursor, err := aggregate.New(t.Context(), input, percentileAdmissionOptions(t, q, []model.TypeLayout{float8}, outputs, calls))
			if err != nil {
				t.Fatalf("128 inline float percentile states: %v", err)
			}
			t.Cleanup(func() { _ = cursor.Close() })
			rows, err := drain(t, cursor)
			if err != nil || len(rows) != 1 || len(rows[0]) != 128 {
				t.Fatalf("rows=%v error=%v, want one 128-column row", rows, err)
			}
			for i, value := range rows[0] {
				want := float64(i)
				if test.repeated {
					want = 64
				}
				want += test.wantOffset
				if value.Type != model.TypeFloat64 || value.IsNull || value.FloatVal != want {
					t.Fatalf("column %d=%+v, want %v", i, value, want)
				}
				if evaluators[i].calls != 1 {
					t.Fatalf("column %d direct evaluations=%d, want 1", i, evaluators[i].calls)
				}
			}
			usage := q.Usage()
			t.Logf("wide percentile usage: spill=%t work_peak=%d spill_peak=%d peak_files=%d peak_readers=%d", test.spill, usage.Work.Peak, usage.Spill.Peak, usage.PeakFiles, usage.PeakReaders)
			if (usage.Spill.Peak > 0) != test.spill || (usage.PeakFiles > 0) != test.spill || (usage.PeakReaders > 0) != test.spill {
				t.Fatalf("spill=%v, usage=%+v", test.spill, usage)
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			if !input.closed {
				t.Fatal("completed aggregate retained input")
			}
			released(t, q, dir)
		})
	}
}

func TestPercentileAdmissionRetainsOtherCallAllowances(t *testing.T) {
	interval := model.TypeLayout{Type: model.Type{ID: 1186, Storage: model.TypeInterval}}
	floatArray := model.TypeLayout{Type: model.Type{ID: 1022, Storage: model.TypeArray}, Children: []model.TypeLayout{float8}}
	fractions := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.Float64Val(.5)}}
	for _, test := range []struct {
		name          string
		function      model.FunctionID
		input, output model.TypeLayout
		directLayout  model.TypeLayout
		direct        []model.Value
		ordered       bool
	}{
		{"discrete text", 3972, text, text, float8, []model.Value{model.Float64Val(.5)}, true},
		{"mode text", 3984, text, text, model.TypeLayout{}, nil, true},
		{"continuous array", 3980, float8, floatArray, floatArray, []model.Value{fractions}, true},
		{"continuous interval", 3976, interval, interval, float8, []model.Value{model.Float64Val(.5)}, true},
		{"hypothetical rank", 3986, float8, int8, float8, []model.Value{model.Float64Val(.5)}, true},
		{"ordinary float sum", 2111, float8, float8, model.TypeLayout{}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, false, 64<<20)
			calls := make([]aggregate.Call, 128)
			outputs := make([]model.TypeLayout, len(calls))
			for i := range calls {
				calls[i] = aggregate.Call{Function: test.function, Arguments: []int{0}}
				if test.ordered {
					calls[i].OrderBy = []aggregate.OrderKey{{Column: 0}}
				}
				if test.direct != nil {
					calls[i].DirectLayouts = []model.TypeLayout{test.directLayout}
					calls[i].DirectEvaluator = &fixedDirect{values: test.direct}
				}
				outputs[i] = test.output
			}
			input := newSource(t, []model.TypeLayout{test.input}, nil)
			cursor, err := aggregate.New(t.Context(), input, percentileAdmissionOptions(t, q, []model.TypeLayout{test.input}, outputs, calls))
			if cursor != nil {
				_ = cursor.Close()
			}
			var diagnostic *model.Error
			if cursor != nil || !errors.As(err, &diagnostic) || diagnostic.Code != "53200" || diagnostic.Message != "aggregate group exceeds group workspace" {
				t.Fatalf("cursor=%v error=%v, want unchanged group workspace rejection", cursor, err)
			}
			if input.pulls != 0 || !input.closed {
				t.Fatalf("rejected aggregate: pulls=%d closed=%v", input.pulls, input.closed)
			}
			released(t, q, dir)
		})
	}
}

func TestPercentileAdmissionPreservesResourceFailures(t *testing.T) {
	for _, name := range []string{"group bytes", "work bytes", "cancellation"} {
		t.Run(name, func(t *testing.T) {
			q, dir := query(t, false, 64<<20)
			direct := &fixedDirect{values: []model.Value{model.Float64Val(.5)}}
			call := aggregate.Call{Function: 3974, Arguments: []int{0}, DirectLayouts: []model.TypeLayout{float8}, DirectEvaluator: direct, OrderBy: []aggregate.OrderKey{{Column: 0}}}
			o := percentileAdmissionOptions(t, q, []model.TypeLayout{float8}, []model.TypeLayout{float8}, []aggregate.Call{call})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "group bytes":
				o.GroupBytes = 9*(64<<10) - 1
			case "work bytes":
				o.WorkBytes = 1
			case "cancellation":
				cancel()
			}
			input := newSource(t, []model.TypeLayout{float8}, [][]model.Value{{model.Float64Val(0)}, {model.Float64Val(128)}})
			cursor, err := aggregate.New(ctx, input, o)
			if cursor != nil {
				_ = cursor.Close()
			}
			if name == "cancellation" {
				if cursor != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("cursor=%v error=%v, want canceled", cursor, err)
				}
			} else {
				want := "aggregate group exceeds group workspace"
				if name == "work bytes" {
					want = "admit aggregate ordering"
				}
				var diagnostic *model.Error
				if cursor != nil || !errors.As(err, &diagnostic) || diagnostic.Code != "53200" || diagnostic.Message != want {
					t.Fatalf("cursor=%v error=%v, want 53200 %s", cursor, err, want)
				}
			}
			if input.pulls != 0 || !input.closed || direct.calls != 0 {
				t.Fatalf("rejected aggregate: pulls=%d closed=%v direct evaluations=%d", input.pulls, input.closed, direct.calls)
			}
			released(t, q, dir)
		})
	}
}
