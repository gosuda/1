package aggregate_test

import (
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestMoneyBitAggregateWindowFramesAndPartitionReset(t *testing.T) {
	for _, moving := range []bool{false, true} {
		t.Run(fmt.Sprintf("moving=%v", moving), func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			layouts := []model.TypeLayout{int8, money, fixedBits}
			base := options(t, q, layouts, nil, nil, nil)
			o := window.Options{
				InputLayouts: layouts, OutputLayouts: []model.TypeLayout{money, fixedBits, fixedBits, fixedBits},
				PartitionColumns: []int{0},
				Calls: []window.Call{{Function: 2112, Arguments: []int{1}}, {Function: 2242, Arguments: []int{2}}, {Function: 2243, Arguments: []int{2}}, {Function: 6167, Arguments: []int{2}}},
				Frame: ir.Frame{Mode: ir.RowsFrame, Start: ir.FrameBound{Kind: ir.UnboundedPreceding}, End: ir.FrameBound{Kind: ir.CurrentRow}},
				Encoder: base.Encoder, Query: q, BatchRows: 2, BatchBytes: 1 << 16,
				MaxRecordBytes: 1024, PartitionMemoryBytes: 0, MaxPartitionBytes: 16 << 20, MaxPartitionRows: 1000, WorkBytes: 32 << 20,
			}
			if moving {
				o.Frame.Start.Kind = ir.OffsetPreceding
				o.StartOffset = new(model.Int64Val(1))
			}
			input := [][]model.Value{
				{model.Int64Val(1), model.NullVal(model.TypeInt64), model.NullVal(model.TypeBytes)},
				{model.Int64Val(1), model.Int64Val(100), packedBits(9, 0xa0)},
				{model.Int64Val(1), model.Int64Val(200), packedBits(9, 0xc0)},
				{model.Int64Val(1), model.Int64Val(300), packedBits(9, 0xf0)},
				{model.Int64Val(2), model.Int64Val(700), packedBits(3, 0xe0)},
			}
			c, err := window.New(t.Context(), newSource(t, layouts, input), o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			rows, err := drain(t, c)
			if err != nil || len(rows) != len(input) {
				t.Fatalf("money/bit window=%v, error=%v", rows, err)
			}
			for _, value := range rows[0] {
				if !value.IsNull {
					t.Fatalf("all-NULL first frame=%v", rows[0])
				}
			}
			sums := []int64{0, 100, 300, 600, 700}
			ands, ors, xors := []byte{0, 0xa0, 0x80, 0x80, 0xe0}, []byte{0, 0xa0, 0xe0, 0xf0, 0xe0}, []byte{0, 0xa0, 0x60, 0x90, 0xe0}
			if moving {
				sums[3], ands[3], xors[3] = 500, 0xc0, 0x30
			}
			for i := 1; i < len(rows); i++ {
				if rows[i][0].IsNull || rows[i][0].IntVal != sums[i] {
					t.Fatalf("money frame %d=%+v, want %d", i, rows[i][0], sums[i])
				}
				length := 9
				if i == 4 {
					length = 3
				}
				requireBits(t, rows[i][1], packedBits(length, ands[i]))
				requireBits(t, rows[i][2], packedBits(length, ors[i]))
				requireBits(t, rows[i][3], packedBits(length, xors[i]))
			}
			if q.Usage().Spill.Peak == 0 {
				t.Fatal("money/bit window did not spill")
			}
			released(t, q, dir)
		})
	}
}
