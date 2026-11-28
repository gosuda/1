package ir

import "github.com/gosuda/supersql-alpha/internal/model"

func (g *Graph) validateSeries(n Rel, outputs []model.SlotID) error {
	if len(outputs) < 1 || len(outputs) > 2 || n.expressions.count < 2 || n.expressions.count > 4 {
		return invalid("series needs two to four arguments and one or two outputs")
	}
	value := g.slots[outputs[0]-1]
	layout := g.layouts[value.Layout-1]
	timestamp := layout.typ.ID == 1114 && layout.typ.Storage == model.TypeTimestamp
	timestamptz := layout.typ == (model.Type{ID: 1184, Modifier: -1, Storage: model.TypeTimestamp})
	supportedWidth := layout.typ.ID == 23 || layout.typ.ID == 20
	integer := supportedWidth && layout.typ.Storage == model.TypeInt64
	numeric := layout.typ == (model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric})
	supported := integer || timestamp || timestamptz || numeric
	if value.Nullable || layout.children.count != 0 || !supported {
		return invalid("series output must be non-null int4, int8, numeric, timestamp or timestamptz")
	}
	temporal := timestamp || timestamptz
	if temporal && n.expressions.count < 3 {
		return invalid("timestamp series requires an interval step")
	}
	if n.expressions.count == 4 && !timestamptz {
		return invalid("only timestamptz series accepts an explicit timezone")
	}
	for index, id := range sliceAt(g.scalarRefs, n.expressions) {
		if (timestamp || timestamptz) && index == 2 {
			step := g.layouts[g.scalars[id-1].Layout-1]
			if step.typ.ID != 1186 || step.typ.Storage != model.TypeInterval || step.children.count != 0 {
				return invalid("timestamp series step must be an interval")
			}
			continue
		}
		if index == 3 {
			zone := g.layouts[g.scalars[id-1].Layout-1]
			if zone.typ.ID != 25 || zone.typ.Modifier != -1 || zone.typ.Storage != model.TypeString || zone.children.count != 0 {
				return invalid("timestamptz series timezone must be text")
			}
			continue
		}
		if !g.sameLayout(g.scalars[id-1].Layout, value.Layout) {
			return invalid("series bound layout differs from output")
		}
	}
	return g.validateSeriesOrdinality(outputs)
}

func (g *Graph) validateSubscripts(n Rel, outputs []model.SlotID) error {
	if len(outputs) < 1 || len(outputs) > 2 || n.expressions.count < 2 || n.expressions.count > 3 {
		return invalid("array subscripts need two or three arguments and one or two outputs")
	}
	value := g.slots[outputs[0]-1]
	layout := g.layouts[value.Layout-1]
	canonical := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	if value.Nullable || layout.children.count != 0 || layout.typ != canonical {
		return invalid("array subscripts output must be non-null int4")
	}
	args := sliceAt(g.scalarRefs, n.expressions)
	array := g.layouts[g.scalars[args[0]-1].Layout-1]
	if array.typ.Storage != model.TypeArray || array.children.count != 1 {
		return invalid("array subscripts require an array with an element layout")
	}
	if !g.sameLayout(g.scalars[args[1]-1].Layout, value.Layout) {
		return invalid("array subscripts dimension must be int4")
	}
	if len(args) == 3 {
		reverse := g.layouts[g.scalars[args[2]-1].Layout-1]
		canonical := model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}
		if reverse.children.count != 0 || reverse.typ != canonical {
			return invalid("array subscripts reverse must be boolean")
		}
	}
	return g.validateSeriesOrdinality(outputs)
}

func (g *Graph) validateSeriesOrdinality(outputs []model.SlotID) error {
	if len(outputs) == 2 {
		ordinal := g.slots[outputs[1]-1]
		layout := g.layouts[ordinal.Layout-1]
		canonical := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
		if ordinal.Nullable || layout.children.count != 0 || layout.typ != canonical {
			return invalid("series ordinality must be non-null int8")
		}
	}
	return nil
}
