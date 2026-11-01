package window_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestWindowExtremaCollationErrorTiming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		collation model.CollationID
		values    []model.Value
		want      []model.Value
		wantError bool
	}{
		{"single datum", 0, []model.Value{model.StringVal("z")}, []model.Value{model.StringVal("z")}, false},
		{"NULL does not compare", 0, []model.Value{model.StringVal("z"), model.NullVal(model.TypeString)}, []model.Value{model.StringVal("z"), model.StringVal("z")}, false},
		{"second datum compares", 0, []model.Value{model.StringVal("z"), model.StringVal("a")}, []model.Value{model.StringVal("z")}, true},
		{"bound collation resolves", 951, []model.Value{model.StringVal("z"), model.StringVal("a")}, []model.Value{model.StringVal("z"), model.StringVal("a")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false)
			layout := text
			layout.Type.Collation = 0
			o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{layout}, []window.Call{{Function: 2145, InputCollation: tc.collation, Arguments: []int{0}}})
			o.Frame.Mode = ir.RowsFrame
			o.PartitionMemoryBytes = 0
			rows := make([][]model.Value, len(tc.values))
			for i, value := range tc.values {
				rows[i] = []model.Value{value}
			}
			input := newSource(t, o.InputLayouts, rows)
			got, err := drain(t, open(t, input, o))
			if tc.wantError {
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "42P22" {
					t.Fatalf("window error=%v, want 42P22", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("rows=%v, want %v", got, tc.want)
			}
			for i, want := range tc.want {
				if got[i][0].IsNull != want.IsNull || got[i][0].StrVal != want.StrVal {
					t.Fatalf("row %d=%v, want %v", i, got[i][0], want)
				}
			}
			if !input.closed {
				t.Fatal("window retained input")
			}
			released(t, q, dir)
		})
	}
}

func TestWindowLagResultCollationDiffersFromArguments(t *testing.T) {
	q, dir := query(t, false)
	defaultLayout, outputLayout := text, text
	defaultLayout.Type.Collation = 951
	outputLayout.Type.Collation = 0
	o := options(t, q, []model.TypeLayout{text, int4, defaultLayout}, []model.TypeLayout{outputLayout}, []window.Call{{Function: 3108, Arguments: []int{0, 1, 2}}})
	input := newSource(t, o.InputLayouts, [][]model.Value{
		{model.StringVal("first"), iv(1), model.StringVal("fallback")},
		{model.StringVal("second"), iv(1), model.StringVal("unused")},
	})
	got, err := drain(t, open(t, input, o))
	if err != nil || len(got) != 2 || got[0][0].StrVal != "fallback" || got[1][0].StrVal != "first" {
		t.Fatalf("lag rows=%v, error=%v", got, err)
	}
	released(t, q, dir)
}
