package unnest_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/unnest"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestMultirangeByteBackpressurePreservesPendingRangeAndOrdinality(t *testing.T) {
	multi := model.TypeLayout{Type: model.Type{ID: 4532, Storage: model.TypeBytes, Modifier: -1}}
	element := model.TypeLayout{Type: model.Type{ID: 3906, Storage: model.TypeBytes, Modifier: -1}}
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1}}
	args := []model.TypeLayout{{Type: model.Type{ID: 2275, Storage: model.TypeString, Modifier: -1}}, {Type: model.Type{ID: 26, Storage: model.TypeInt64, Modifier: -1}}, integer}
	parse, err := builtin.ResolveTyped(t.Context(), 4231, args, multi, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := builtin.ResolveTyped(t.Context(), 3848, []model.TypeLayout{element}, numeric, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	input.WriteByte('{')
	expected := make([]string, 40)
	for i := range expected {
		if i != 0 {
			input.WriteByte(',')
		}
		expected[i] = fmt.Sprintf("%d.%s", i*2, strings.Repeat("0", 128+i))
		fmt.Fprintf(&input, "[%s,%d)", expected[i], i*2+1)
	}
	input.WriteByte('}')
	budget := builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 4 << 20}
	value, err := parse.Call(t.Context(), []model.Value{model.StringVal(input.String()), model.Int64Val(4532), model.Int64Val(-1)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, multi)
	root := f.scalar(ir.Scalar{Kind: ir.Const}, value)
	q := query(t, t.Context())
	opts := unnest.Options{Arguments: f.compile(root), Element: element, Output: []model.TypeLayout{element, bigint}, Ordinality: true, Multirange: 4532, Query: q, BatchRows: 40, BatchBytes: 2048}
	cursor := open(t, t.Context(), opts)
	var batch columnar.Batch
	next, batches := 0, 0
	for {
		err := cursor.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		batches++
		for _, row := range rows(t, &batch) {
			bound, err := lower.Call(t.Context(), []model.Value{row[0]}, budget)
			if err != nil || next >= len(expected) || bound.StrVal != expected[next] || row[1].IntVal != int64(next+1) {
				t.Fatalf("pending range %d = %v ordinal %d, %v", next, bound, row[1].IntVal, err)
			}
			next++
		}
	}
	if next != len(expected) || batches < 2 {
		t.Fatalf("returned %d ranges in %d batches", next, batches)
	}
	released(t, q)
}
