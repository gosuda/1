package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (g *Graph) validateRowsFrom(ctx context.Context, n Rel, outputs []model.SlotID) error {
	data := outputs
	if n.RowsFromOrdinality {
		if len(data) == 0 {
			return invalid("ROWS FROM ordinality output is missing")
		}
		ordinal := g.slots[data[len(data)-1]-1]
		layout := g.layouts[ordinal.Layout-1]
		canonical := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
		if ordinal.Nullable || layout.children.count != 0 || layout.typ != canonical || g.inputHas(n, data[len(data)-1]) {
			return invalid("ROWS FROM ordinality must be newly declared non-null int8")
		}
		data = data[:len(data)-1]
	}
	var width uint64
	for _, id := range sliceAt(g.relRefs, n.inputs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		width += uint64(g.rels[id-1].outputs.count)
		if width > uint64(len(data)) {
			return invalid("ROWS FROM output width mismatch")
		}
	}
	if width != uint64(len(data)) {
		return invalid("ROWS FROM output width mismatch")
	}
	at := 0
	for _, id := range sliceAt(g.relRefs, n.inputs) {
		for _, source := range sliceAt(g.slotRefs, g.rels[id-1].outputs) {
			if err := ctx.Err(); err != nil {
				return err
			}
			out, in := g.slots[data[at]-1], g.slots[source-1]
			if !out.Nullable || !g.sameLayout(out.Layout, in.Layout) || g.inputHas(n, data[at]) {
				return invalid("ROWS FROM outputs require newly declared nullable matching layouts")
			}
			at++
		}
	}
	return nil
}
