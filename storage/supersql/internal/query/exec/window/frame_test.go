package window_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestFrameModesAndExclusions(t *testing.T) {
	rows := [][]model.Value{{iv(1), iv(10), iv(2)}, {iv(1), iv(11), iv(2)}, {iv(2), null(), iv(2)}, {iv(3), iv(30), iv(2)}, {iv(3), iv(31), iv(2)}}
	tests := []struct {
		name                   string
		mode                   ir.FrameMode
		start, end             ir.BoundKind
		startOffset, endOffset int64
		exclusion              ir.FrameExclusion
		want                   [][]model.Value
	}{
		{name: "range cumulative peers", mode: ir.RangeFrame, start: ir.UnboundedPreceding, end: ir.CurrentRow,
			want: [][]model.Value{{iv(10), iv(11), iv(11)}, {iv(10), iv(11), iv(11)}, {iv(10), null(), iv(11)}, {iv(10), iv(31), iv(11)}, {iv(10), iv(31), iv(11)}}},
		{name: "rows neighboring positions", mode: ir.RowsFrame, start: ir.OffsetPreceding, end: ir.OffsetFollowing, startOffset: 1, endOffset: 1,
			want: [][]model.Value{{iv(10), iv(11), iv(11)}, {iv(10), null(), iv(11)}, {iv(11), iv(30), null()}, {null(), iv(31), iv(30)}, {iv(30), iv(31), iv(31)}}},
		{name: "groups preceding peer group", mode: ir.GroupsFrame, start: ir.OffsetPreceding, end: ir.CurrentRow, startOffset: 1,
			want: [][]model.Value{{iv(10), iv(11), iv(11)}, {iv(10), iv(11), iv(11)}, {iv(10), null(), iv(11)}, {null(), iv(31), iv(30)}, {null(), iv(31), iv(30)}}},
		{name: "groups following peer group", mode: ir.GroupsFrame, start: ir.CurrentRow, end: ir.OffsetFollowing, endOffset: 1,
			want: [][]model.Value{{iv(10), null(), iv(11)}, {iv(10), null(), iv(11)}, {null(), iv(31), iv(30)}, {iv(30), iv(31), iv(31)}, {iv(30), iv(31), iv(31)}}},
		{name: "range only peers", mode: ir.RangeFrame, start: ir.CurrentRow, end: ir.CurrentRow,
			want: [][]model.Value{{iv(10), iv(11), iv(11)}, {iv(10), iv(11), iv(11)}, {null(), null(), null()}, {iv(30), iv(31), iv(31)}, {iv(30), iv(31), iv(31)}}},
		{name: "exclude current", mode: ir.RangeFrame, start: ir.UnboundedPreceding, end: ir.CurrentRow, exclusion: ir.ExcludeCurrent,
			want: [][]model.Value{{iv(11), iv(11), null()}, {iv(10), iv(10), null()}, {iv(10), iv(11), iv(11)}, {iv(10), iv(31), iv(11)}, {iv(10), iv(30), iv(11)}}},
		{name: "exclude group", mode: ir.RangeFrame, start: ir.UnboundedPreceding, end: ir.CurrentRow, exclusion: ir.ExcludeGroup,
			want: [][]model.Value{{null(), null(), null()}, {null(), null(), null()}, {iv(10), iv(11), iv(11)}, {iv(10), null(), iv(11)}, {iv(10), null(), iv(11)}}},
		{name: "exclude ties retaining current", mode: ir.RangeFrame, start: ir.UnboundedPreceding, end: ir.CurrentRow, exclusion: ir.ExcludeTies,
			want: [][]model.Value{{iv(10), iv(10), null()}, {iv(11), iv(11), null()}, {iv(10), null(), iv(11)}, {iv(10), iv(30), iv(11)}, {iv(10), iv(31), iv(11)}}},
		{name: "rows all preceding partition", mode: ir.RowsFrame, start: ir.OffsetPreceding, end: ir.OffsetPreceding, startOffset: 8, endOffset: 3,
			want: [][]model.Value{{null(), null(), null()}, {null(), null(), null()}, {null(), null(), null()}, {iv(10), iv(10), null()}, {iv(10), iv(11), iv(11)}}},
		{name: "rows inverted offset interval", mode: ir.RowsFrame, start: ir.OffsetFollowing, end: ir.OffsetFollowing, startOffset: 2, endOffset: 1,
			want: [][]model.Value{{null(), null(), null()}, {null(), null(), null()}, {null(), null(), null()}, {null(), null(), null()}, {null(), null(), null()}}},
		{name: "groups saturating offsets", mode: ir.GroupsFrame, start: ir.OffsetPreceding, end: ir.OffsetFollowing, startOffset: math.MaxInt64, endOffset: math.MaxInt64,
			want: [][]model.Value{{iv(10), iv(31), iv(11)}, {iv(10), iv(31), iv(11)}, {iv(10), iv(31), iv(11)}, {iv(10), iv(31), iv(11)}, {iv(10), iv(31), iv(11)}}},
		{name: "rows ties exclude peers outside row bounds", mode: ir.RowsFrame, start: ir.OffsetFollowing, end: ir.UnboundedFollowing, startOffset: 1, exclusion: ir.ExcludeTies,
			want: [][]model.Value{{null(), iv(31), iv(30)}, {null(), iv(31), iv(30)}, {iv(30), iv(31), iv(31)}, {null(), null(), null()}, {null(), null(), null()}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []uint64{0, 1 << 20} {
				q, dir := query(t, false)
				layouts := []model.TypeLayout{int8, int8, int4}
				calls := []window.Call{{Function: 3112, Arguments: []int{1}}, {Function: 3113, Arguments: []int{1}}, {Function: 3114, Arguments: []int{1, 2}}}
				o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8}, calls)
				o.PartitionMemoryBytes = memory
				o.OrderBy = []window.OrderKey{{Column: 0}}
				o.Frame = ir.Frame{Mode: tc.mode, Start: ir.FrameBound{Kind: tc.start}, End: ir.FrameBound{Kind: tc.end}, Exclusion: tc.exclusion}
				if tc.start == ir.OffsetPreceding || tc.start == ir.OffsetFollowing {
					value := iv(tc.startOffset)
					o.StartOffset = &value
				}
				if tc.end == ir.OffsetPreceding || tc.end == ir.OffsetFollowing {
					value := iv(tc.endOffset)
					o.EndOffset = &value
				}
				assertRows(t, open(t, newSource(t, layouts, rows), o), tc.want)
				released(t, q, dir)
			}
		})
	}
}

func TestNoOrderKeysMakesWholePartitionPeers(t *testing.T) {
	q, dir := query(t, false)
	layouts := []model.TypeLayout{int8, int4}
	o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8}, []window.Call{{Function: 3112, Arguments: []int{0}}, {Function: 3113, Arguments: []int{0}}, {Function: 3114, Arguments: []int{0, 1}}})
	assertRows(t, open(t, newSource(t, layouts, [][]model.Value{{iv(1), null()}, {iv(2), iv(9)}, {iv(3), iv(2)}}), o), [][]model.Value{{iv(1), iv(3), null()}, {iv(1), iv(3), null()}, {iv(1), iv(3), iv(2)}})
	released(t, q, dir)
}

func TestNumericAndNaNPeersUseSQLOrdering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout model.TypeLayout
		values []model.Value
	}{
		{name: "float signed zero and nan", layout: float8, values: []model.Value{fv(math.Copysign(0, -1)), fv(0), fv(2), fv(math.NaN()), fv(math.Float64frombits(0x7ff8000000000002)), model.NullVal(model.TypeFloat64)}},
		{name: "numeric scale and nan", layout: numeric, values: []model.Value{{Type: model.TypeNumeric, StrVal: "1.0"}, {Type: model.TypeNumeric, StrVal: "1.00"}, {Type: model.TypeNumeric, StrVal: "2"}, {Type: model.TypeNumeric, StrVal: "NaN"}, {Type: model.TypeNumeric, StrVal: "NaN"}, model.NullVal(model.TypeNumeric)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false)
			layouts := []model.TypeLayout{tc.layout}
			o := options(t, q, layouts, []model.TypeLayout{int8, int8}, []window.Call{{Function: 3101}, {Function: 3102}})
			o.OrderBy = []window.OrderKey{{Column: 0}}
			o.PartitionMemoryBytes = 0
			rows := make([][]model.Value, len(tc.values))
			for i, value := range tc.values {
				rows[i] = []model.Value{value}
			}
			assertRows(t, open(t, newSource(t, layouts, rows), o), [][]model.Value{{iv(1), iv(1)}, {iv(1), iv(1)}, {iv(3), iv(2)}, {iv(4), iv(3)}, {iv(4), iv(3)}, {iv(6), iv(4)}})
			released(t, q, dir)
		})
	}
}

func TestDescendingNullFirstPeerOrder(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3101}})
	o.OrderBy = []window.OrderKey{{Column: 0, Descending: true, NullsFirst: true}}
	assertRows(t, open(t, newSource(t, o.InputLayouts, [][]model.Value{{null()}, {null()}, {iv(3)}, {iv(3)}, {iv(1)}}), o), [][]model.Value{{iv(1)}, {iv(1)}, {iv(3)}, {iv(3)}, {iv(5)}})
	released(t, q, dir)
}
