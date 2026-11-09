package exchange

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestGatherPreservesSelectedNestedBagAcrossEmptyBatches(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	array := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{transportInt}}
	layout := model.TypeLayout{Type: model.Type{ID: 9000, Storage: model.TypeComposite}, Children: []model.TypeLayout{array}}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -4}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 3, MaxBytes: 16384})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []model.Value{value, model.NullVal(model.TypeComposite), value} {
		if err := builder.Append(t.Context(), []model.Value{item}); err != nil {
			t.Fatal(err)
		}
	}
	var native columnar.Batch
	if err := builder.Publish(&native); err != nil {
		t.Fatal(err)
	}
	selection := []uint32{2, 1, 2, 0}
	position, empty := 0, false
	input := &transportCursor{next: func(_ context.Context, out *columnar.Batch) error {
		out.Reset()
		if !empty {
			empty = true
			return nil
		}
		if position == len(selection) {
			return io.EOF
		}
		n := min(out.MaxRows, len(selection)-position)
		out.Columns = append(out.Columns, native.Columns...)
		out.Length = native.Length
		out.Selection = selection[position : position+n]
		position += n
		return nil
	}, cleanup: builder.Close}
	o := transportOptions(q)
	o.Layouts = []model.TypeLayout{layout}
	o.BatchBytes, o.QueueBytes = 16384, 16384
	c, err := NewGather(t.Context(), []storage.BatchCursor{input}, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	// The constructor must own nested layout metadata, not the caller's slices.
	o.Layouts[0].Children[0].Children[0] = model.TypeLayout{}
	var got []model.Value
	for {
		out := columnar.Batch{MaxRows: 1}
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() != 1 {
			t.Fatalf("positive demand yielded %d rows", out.Rows())
		}
		item, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 16384)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, item)
	}
	want := []model.Value{value, model.NullVal(model.TypeComposite), value, value}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested selected bag = %#v, want %#v", got, want)
	}
	if input.closes.Load() != 1 || q.Usage().Work.Live != 0 || q.Usage().Output.Live != 0 {
		t.Fatal("gather retained owned resources")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGatherFirstBatchDoesNotDrainInputs(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	left, right := transportInts(1, 1, 2, 3, 4), transportInts(5, 6)
	c, err := NewGather(t.Context(), []storage.BatchCursor{left, right}, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if transportValue(t, &out, 0) != 1 || left.calls.Load() != 1 || right.calls.Load() != 0 {
		t.Fatal("first batch pulled beyond synchronous demand")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if left.closes.Load() != 1 || right.closes.Load() != 1 {
		t.Fatal("early close leaked a child")
	}
}

func TestGatherEmptyInputBag(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	c, err := NewGather(t.Context(), nil, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != io.EOF {
		t.Fatalf("empty bag: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGatherOutputQuotaIsIndependentOfQueue(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{OutputBytes: 1})
	input := transportInts(1)
	_, err := NewGather(t.Context(), []storage.BatchCursor{input}, transportOptions(q))
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("output quota: %v", err)
	}
	if input.closes.Load() != 1 || input.calls.Load() != 0 || q.Usage().Work.Live != 0 {
		t.Fatal("output admission failure leaked or pulled input")
	}
}

func TestGatherPreservesDictionaryNullsAndConstantDuplicates(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	emitted := false
	dictionary := &transportCursor{next: func(_ context.Context, out *columnar.Batch) error {
		out.Reset()
		if emitted {
			return io.EOF
		}
		emitted = true
		out.Columns = append(out.Columns, columnar.Vector{Type: transportInt.Type, Encoding: columnar.Dictionary, Length: 3, Nulls: []byte{2}, Indices: []uint32{1, 0, 1}, Int64: []int64{4, 9}})
		out.Length = 3
		out.Selection = []uint32{2, 1, 0}
		return nil
	}}
	constantRows := 2
	constant := &transportCursor{next: func(_ context.Context, out *columnar.Batch) error {
		out.Reset()
		if constantRows == 0 {
			return io.EOF
		}
		n := min(constantRows, out.MaxRows)
		out.Columns = append(out.Columns, columnar.Vector{Type: transportInt.Type, Encoding: columnar.Constant, Length: n, Int64: []int64{17}})
		out.Length = n
		constantRows -= n
		return nil
	}}
	c, err := NewGather(t.Context(), []storage.BatchCursor{dictionary, constant}, transportOptions(q))
	if err != nil {
		t.Fatal(err)
	}
	counts, nulls := map[int64]int{}, 0
	for {
		out := columnar.Batch{MaxRows: 3}
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range out.Rows() {
			if out.Columns[0].IsNull(out.Lane(row)) {
				nulls++
			} else {
				counts[transportValue(t, &out, row)]++
			}
		}
	}
	if nulls != 1 || counts[9] != 2 || counts[17] != 2 || len(counts) != 2 {
		t.Fatalf("bag counts=%v nulls=%d", counts, nulls)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGatherExactOutputQuota(t *testing.T) {
	baseline, _ := transportRuntime(t, runtime.QueryConfig{})
	c, err := NewGather(t.Context(), []storage.BatchCursor{transportInts(1)}, transportOptions(baseline))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != nil {
		t.Fatal(err)
	}
	required := baseline.Usage().Output.Peak
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{0, -1} {
		q, _ := transportRuntime(t, runtime.QueryConfig{OutputBytes: required + delta})
		input := transportInts(1)
		c, err := NewGather(t.Context(), []storage.BatchCursor{input}, transportOptions(q))
		if err == nil {
			err = c.Next(t.Context(), &columnar.Batch{MaxRows: 1})
		}
		if delta == 0 && err != nil {
			t.Fatalf("exact output quota: %v", err)
		}
		if delta == -1 && !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("one output byte short: %v", err)
		}
		if c != nil {
			_ = c.Close()
		}
		if input.closes.Load() != 1 || q.Usage().Work.Live != 0 || q.Usage().Output.Live != 0 {
			t.Fatal("output boundary leaked resources")
		}
	}
}
