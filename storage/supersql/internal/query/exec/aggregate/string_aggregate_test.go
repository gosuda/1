package aggregate_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

var bytea = model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}

func stringAccumulator(t *testing.T, q *runtime.Query, layout model.TypeLayout, limit uint64) *aggregate.Accumulator {
	t.Helper()
	function := model.FunctionID(3538)
	if layout.Type.Storage == model.TypeBytes {
		function = 3545
	}
	o := options(t, q, []model.TypeLayout{layout, layout}, []model.TypeLayout{layout}, nil, nil)
	a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: function, InputLayouts: o.InputLayouts, OutputLayout: layout, Encoder: o.Encoder, Query: q, MaxRecordBytes: limit, WorkBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func stringResult(t *testing.T, a *aggregate.Accumulator, want model.Value) {
	t.Helper()
	got, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.IsNull != want.IsNull || got.StrVal != want.StrVal || !bytes.Equal(got.BytesVal, want.BytesVal) {
		t.Fatalf("string_agg result=%+v, want %+v", got, want)
	}
}

func TestStringAggregateDistinguishesNullValuesFromEmptyValues(t *testing.T) {
	for _, layout := range []model.TypeLayout{text, bytea} {
		t.Run(map[model.DataType]string{model.TypeString: "text", model.TypeBytes: "bytea"}[layout.Type.Storage], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := stringAccumulator(t, q, layout, 1024)
			value := func(s string) model.Value {
				if layout.Type.Storage == model.TypeBytes {
					return model.BytesVal([]byte(s))
				}
				return model.StringVal(s)
			}
			null := model.NullVal(layout.Type.Storage)
			if err := a.Advance(t.Context(), []model.Value{null, value("ignored")}); err != nil {
				t.Fatal(err)
			}
			stringResult(t, a, null)
			if err := a.Advance(t.Context(), []model.Value{value(""), value("ignored")}); err != nil {
				t.Fatal(err)
			}
			stringResult(t, a, value(""))
			for _, row := range [][]model.Value{{value("α"), value("/")}, {null, value("ignored")}, {value("β"), null}, {value(""), value("!")}, {value("終"), value("::")}} {
				if err := a.Advance(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			stringResult(t, a, value("/αβ!::終"))
			if err := a.Reset(); err != nil {
				t.Fatal(err)
			}
			stringResult(t, a, null)
			if err := a.Advance(t.Context(), []model.Value{value("new"), value("ignored")}); err != nil {
				t.Fatal(err)
			}
			stringResult(t, a, value("new"))
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestStringAggregateOwnsBorrowedBinaryInputs(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := stringAccumulator(t, q, bytea, 1024)
	input, delimiter := []byte{0, 255}, []byte{128, 0}
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal(input), model.BytesVal(delimiter)}); err != nil {
		t.Fatal(err)
	}
	input[0], input[1] = 1, 254
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal(input), model.BytesVal(delimiter)}); err != nil {
		t.Fatal(err)
	}
	clear(input)
	clear(delimiter)
	stringResult(t, a, model.BytesVal([]byte{0, 255, 128, 0, 1, 254}))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestStringAggregateFailedAdvancePreservesVisibleResult(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := stringAccumulator(t, q, bytea, 128)
	prefix := bytes.Repeat([]byte{'x'}, 127)
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal(prefix), model.NullVal(model.TypeBytes)}); err != nil {
		t.Fatal(err)
	}
	visible, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal([]byte{'y'}), model.BytesVal([]byte{','})}); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("oversized result error=%v, want budget error", err)
	}
	stringResult(t, a, model.BytesVal(prefix))
	if !bytes.Equal(visible.BytesVal, prefix) {
		t.Fatalf("failed Advance changed borrowed result: %q", visible.BytesVal)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Advance(ctx, []model.Value{model.BytesVal([]byte{'z'}), model.NullVal(model.TypeBytes)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Advance=%v", err)
	}
	stringResult(t, a, model.BytesVal(prefix))
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal([]byte{'z'}), model.NullVal(model.TypeBytes)}); err != nil {
		t.Fatal(err)
	}
	stringResult(t, a, model.BytesVal(append(bytes.Clone(prefix), 'z')))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

type stringAggregateCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *stringAggregateCancelContext) Value(any) any { return nil }

func (c *stringAggregateCancelContext) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestStringAggregateCancellationDuringTailCopyPreservesState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := stringAccumulator(t, q, bytea, 128<<10)
	prefix := bytes.Repeat([]byte{'x'}, 64<<10)
	null := model.NullVal(model.TypeBytes)
	for _, value := range []model.Value{model.BytesVal(prefix), model.BytesVal([]byte{'a'})} {
		if err := a.Advance(t.Context(), []model.Value{value, null}); err != nil {
			t.Fatal(err)
		}
	}
	visible, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelDuringCopy := &stringAggregateCancelContext{Context: ctx, cancel: cancel, checks: 4}
	row := []model.Value{model.BytesVal(bytes.Repeat([]byte{'v'}, 10000)), model.BytesVal(bytes.Repeat([]byte{'/'}, 40000))}
	if err := a.Advance(cancelDuringCopy, row); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-copy cancellation=%v", err)
	}
	want := append(bytes.Clone(prefix), 'a')
	if !bytes.Equal(visible.BytesVal, want) {
		t.Fatal("cancelled tail copy changed the prior borrowed result")
	}
	stringResult(t, a, model.BytesVal(want))
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal([]byte{'b'}), null}); err != nil {
		t.Fatal(err)
	}
	stringResult(t, a, model.BytesVal(append(want, 'b')))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestStringAggregateDistinctIncludesNullDelimiterInTuple(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{text, text, boolean}
	rows := [][]model.Value{
		{model.StringVal("a"), model.NullVal(model.TypeString), model.BoolVal(false)},
		{model.StringVal("a"), model.NullVal(model.TypeString), model.BoolVal(true)},
		{model.StringVal("a"), model.NullVal(model.TypeString), model.BoolVal(true)},
		{model.StringVal("a"), model.StringVal(""), model.BoolVal(true)},
		{model.StringVal("a"), model.StringVal(""), model.BoolVal(true)},
		{model.NullVal(model.TypeString), model.StringVal("!"), model.BoolVal(true)},
	}
	calls := []aggregate.Call{{Function: 3538, Arguments: []int{0, 1}, Distinct: true, FilterColumn: new(2)}}
	got, err := drain(t, open(t, newSource(t, layouts, rows), options(t, q, layouts, []model.TypeLayout{text}, nil, calls)))
	if err != nil || len(got) != 1 {
		t.Fatalf("DISTINCT rows=%v error=%v", got, err)
	}
	if got[0][0].IsNull || got[0][0].StrVal != "aa" {
		t.Fatalf("DISTINCT string_agg=%v, want aa from NULL and empty delimiters", got)
	}
	released(t, q, dir)
}

func TestStringAggregatePreservesRowDelimitersThroughGroupSpill(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{int8, text, text}
	var rows [][]model.Value
	for _, item := range []struct{ value, delimiter string }{{"α", "ignored"}, {"β", "/"}, {"", "!"}, {"終", "::"}} {
		for id := range 12 {
			rows = append(rows, []model.Value{model.Int64Val(int64(id)), model.StringVal(item.value), model.StringVal(item.delimiter)})
		}
	}
	o := options(t, q, layouts, []model.TypeLayout{int8, text}, []int{0}, []aggregate.Call{{Function: 3538, Arguments: []int{1, 2}}})
	o.GroupBytes = 32 << 10
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil || len(got) != 12 {
		t.Fatalf("spilled string groups=%v error=%v", got, err)
	}
	seen := make(map[int64]bool)
	for _, row := range got {
		id := row[0].IntVal
		if id < 0 || id >= 12 || seen[id] || row[1].IsNull || row[1].StrVal != "α/β!::終" {
			t.Fatalf("spilled group=%v, want unique id and α/β!::終", row)
		}
		seen[id] = true
	}
	if q.Usage().Spill.Peak == 0 {
		t.Fatal("string groups did not spill")
	}
	released(t, q, dir)
}

func TestStringAggregateSpillCloseAndCancellationReleaseResources(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelled], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			layouts := []model.TypeLayout{int8, text, text}
			var rows [][]model.Value
			for id := range 40 {
				rows = append(rows, []model.Value{model.Int64Val(int64(id)), model.StringVal("owned"), model.NullVal(model.TypeString)})
			}
			s := newSource(t, layouts, rows)
			o := options(t, q, layouts, []model.TypeLayout{int8, text}, []int{0}, []aggregate.Call{{Function: 3538, Arguments: []int{1, 2}}})
			o.GroupBytes = 32 << 10
			c, err := aggregate.New(ctx, s, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if q.Usage().Spill.Live == 0 {
				t.Fatal("early-close case did not retain deferred spill")
			}
			if cancelled {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled string_agg=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !s.closed {
				t.Fatal("string aggregate retained source")
			}
			released(t, q, dir)
		})
	}
}

func TestStringAggregateResultLimitFailureReleasesResources(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{text, text}
	row := []model.Value{model.StringVal(strings.Repeat("x", 400)), model.StringVal(",")}
	s := newSource(t, layouts, [][]model.Value{row, row, row})
	c := open(t, s, options(t, q, layouts, []model.TypeLayout{text}, nil, []aggregate.Call{{Function: 3538, Arguments: []int{0, 1}}}))
	if _, err := drain(t, c); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("oversized string result=%v, want budget error", err)
	}
	if !s.closed {
		t.Fatal("failed string aggregate retained source")
	}
	released(t, q, dir)
}

func TestStringAggregateRejectsInvalidLayoutsBeforeInputPull(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		function  model.FunctionID
		layouts   []model.TypeLayout
		output    model.TypeLayout
		arguments []int
	}{
		{"missing delimiter", 3538, []model.TypeLayout{text, text}, text, []int{0}},
		{"extra argument", 3538, []model.TypeLayout{text, text}, text, []int{0, 1, 0}},
		{"wrong value oid", 3538, []model.TypeLayout{bytea, text}, text, []int{0, 1}},
		{"wrong delimiter oid", 3545, []model.TypeLayout{bytea, text}, bytea, []int{0, 1}},
		{"wrong value storage", 3538, []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeBytes}}, text}, text, []int{0, 1}},
		{"wrong delimiter storage", 3545, []model.TypeLayout{bytea, {Type: model.Type{ID: 17, Storage: model.TypeString}}}, bytea, []int{0, 1}},
		{"wrong output storage", 3538, []model.TypeLayout{text, text}, model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeBytes}}, []int{0, 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			s := newSource(t, []model.TypeLayout{text, text}, nil)
			o := options(t, q, scenario.layouts, []model.TypeLayout{scenario.output}, nil, []aggregate.Call{{Function: scenario.function, Arguments: scenario.arguments}})
			_, err := aggregate.New(t.Context(), s, o)
			if !errors.Is(err, columnar.ErrValueLayout) || s.pulls != 0 || !s.closed {
				t.Fatalf("invalid string call: error=%v pulls=%d closed=%v", err, s.pulls, s.closed)
			}
			released(t, q, dir)
		})
	}
}
