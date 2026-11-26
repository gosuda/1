package ir

import "github.com/gosuda/supersql-alpha/internal/model"

func (g *Graph) validateUnnest(n Rel, outputs []model.SlotID) error {
	if n.JSONTableRoot != 0 {
		if err := g.validateJSONTable(n); err != nil {
			return err
		}
		return g.validateRowSource(n, outputs)
	}
	if n.RowFunction != 0 {
		return g.validateRowSource(n, outputs)
	}
	if n.expressions.count != 1 {
		return invalid("unnest requires one argument")
	}
	argument := g.scalarRefs[n.expressions.start]
	array := g.layouts[g.scalars[argument-1].Layout-1]
	var element LayoutID
	if n.UnnestRange != (model.Type{}) {
		if array.typ.Storage != model.TypeBytes || array.children.count != 0 || n.UnnestRange.ID == 0 || n.UnnestRange.Storage != model.TypeBytes {
			return invalid("multirange unnest requires a scalar byte representation and range result")
		}
	} else {
		if array.typ.Storage != model.TypeArray || array.children.count != 1 {
			return invalid("unnest requires an array with an element layout")
		}
		element = g.layoutRefs[array.children.start]
	}
	data := outputs
	if n.UnnestOrdinality {
		if len(data) == 0 {
			return invalid("unnest ordinality output is missing")
		}
		ordinal := g.slots[data[len(data)-1]-1]
		layout := g.layouts[ordinal.Layout-1]
		canonical := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
		if ordinal.Nullable || layout.children.count != 0 || layout.typ != canonical {
			return invalid("unnest ordinality must be non-null int8")
		}
		data = data[:len(data)-1]
	}
	if n.UnnestRange != (model.Type{}) {
		if len(data) != 1 {
			return invalid("multirange unnest requires one range output")
		}
		result := g.layouts[g.slots[data[0]-1].Layout-1]
		if result.typ != n.UnnestRange || result.children.count != 0 {
			return invalid("multirange unnest output must match its declared range")
		}
	} else if g.layouts[element-1].typ.Storage != model.TypeComposite {
		if len(data) != 1 || !g.sameLayout(g.slots[data[0]-1].Layout, element) {
			return invalid("unnest scalar output must match its element layout")
		}
	}
	// Record definitions are checked against the first non-NULL record at runtime.
	for _, id := range data {
		if !g.slots[id-1].Nullable {
			return invalid("unnest data outputs must be nullable")
		}
	}
	return nil
}

func (g *Graph) validateRowSource(n Rel, outputs []model.SlotID) error {
	if !validID(n.RowResult, g.layouts) {
		return invalid("row source result layout is missing")
	}
	data := outputs
	if n.UnnestOrdinality {
		if len(data) == 0 {
			return invalid("row source ordinality output is missing")
		}
		ordinal := g.slots[data[len(data)-1]-1]
		layout := g.layouts[ordinal.Layout-1]
		canonical := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
		if ordinal.Nullable || layout.children.count != 0 || layout.typ != canonical {
			return invalid("row source ordinality must be non-null int8")
		}
		data = data[:len(data)-1]
	}
	result := g.layouts[n.RowResult-1]
	if result.typ.Storage == model.TypeComposite {
		single := len(data) == 1 && g.sameLayout(g.slots[data[0]-1].Layout, n.RowResult)
		if !single {
			if len(data) != int(result.children.count) {
				return invalid("row source composite output width mismatch")
			}
			for i, output := range data {
				child := g.layoutRefs[uint64(result.children.start)+uint64(i)]
				if !g.sameLayout(g.slots[output-1].Layout, child) {
					return invalid("row source composite output layout mismatch")
				}
			}
		}
	} else if len(data) != 1 || !g.sameLayout(g.slots[data[0]-1].Layout, n.RowResult) {
		return invalid("row source scalar output layout mismatch")
	}
	for _, output := range data {
		if !g.slots[output-1].Nullable {
			return invalid("row source data outputs must be nullable")
		}
	}
	return nil
}

func (g *Graph) validateJSONTable(n Rel) error {
	if n.JSONTableFlags&^(SQLJSONCaptureError|SQLJSONUseTZ) != 0 || n.JSONTableArguments == 0 || uint32(n.JSONTableArguments) > n.expressions.count ||
		n.JSONTableFormat < SQLJSONDefaultFormat || n.JSONTableFormat > SQLJSONBinaryFormat ||
		n.JSONTableEncoding < SQLJSONDefaultEncoding || n.JSONTableEncoding > SQLJSONUTF32 {
		return invalid("invalid JSON_TABLE metadata")
	}
	if int(n.JSONTableArguments) != 1+4*int(n.JSONTablePassing) || n.JSONTableRoot > n.expressions.count || n.JSONTableRoot <= uint32(n.JSONTableArguments) {
		return invalid("invalid JSON_TABLE argument or root path range")
	}
	if !g.jsonPathConstant(n, n.JSONTableRoot) {
		return invalid("JSON_TABLE root path must be a jsonpath constant")
	}
	result := g.layouts[n.RowResult-1]
	if result.typ.Storage != model.TypeComposite || int(result.children.count) != int(n.outputs.count) || n.jsonTableColumns.count == 0 {
		return invalid("invalid JSON_TABLE row result")
	}
	outputs := make([]bool, result.children.count)
	columns := sliceAt(g.jsonTableColumns, n.jsonTableColumns)
	for i, column := range columns {
		if column.Kind < JSONTableOrdinality || column.Kind > JSONTableNested || int(column.Parent) > i {
			return invalid("invalid JSON_TABLE column tree")
		}
		if column.Parent != 0 && columns[column.Parent-1].Kind != JSONTableNested {
			return invalid("JSON_TABLE column parent is not nested")
		}
		if column.JSONFormat < SQLJSONDefaultFormat || column.JSONFormat > SQLJSONBinaryFormat ||
			column.JSONEncoding < SQLJSONDefaultEncoding || column.JSONEncoding > SQLJSONUTF32 ||
			column.JSONWrapper < SQLJSONUnspecifiedWrapper || column.JSONWrapper > SQLJSONUnconditionalWrapper ||
			column.JSONQuotes < SQLJSONUnspecifiedQuotes || column.JSONQuotes > SQLJSONOmitQuotes ||
			column.JSONFlags&^(SQLJSONCaptureEmpty|SQLJSONCaptureError) != 0 {
			return invalid("invalid JSON_TABLE column metadata")
		}
		if column.Kind == JSONTableNested {
			if column.Output != 0 || column.Layout != 0 || !g.jsonPathConstant(n, column.Path) {
				return invalid("invalid JSON_TABLE nested column")
			}
			continue
		}
		if column.Output == 0 || int(column.Output) > len(outputs) || outputs[column.Output-1] || !validID(column.Layout, g.layouts) {
			return invalid("invalid JSON_TABLE output column")
		}
		outputs[column.Output-1] = true
		actual := g.layoutRefs[uint64(result.children.start)+uint64(column.Output-1)]
		if column.Kind == JSONTableOrdinality {
			layout := g.layouts[column.Layout-1]
			if column.Path != 0 || layout.typ != (model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}) || !g.sameLayout(actual, column.Layout) {
				return invalid("invalid JSON_TABLE ordinality column")
			}
			continue
		}
		if !g.jsonPathConstant(n, column.Path) {
			return invalid("JSON_TABLE value path must be a jsonpath constant")
		}
		status := g.layouts[actual-1]
		if status.typ.Storage != model.TypeComposite || status.children.count != 2 {
			return invalid("JSON_TABLE value column needs internal status layout")
		}
		code := g.layoutRefs[status.children.start]
		codeLayout := g.layouts[code-1]
		if codeLayout.typ != (model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}) || !g.sameLayout(g.layoutRefs[status.children.start+1], column.Layout) {
			return invalid("invalid JSON_TABLE status layout")
		}
	}
	for _, present := range outputs {
		if !present {
			return invalid("JSON_TABLE output mapping is incomplete")
		}
	}
	return nil
}

func (g *Graph) jsonPathConstant(n Rel, ordinal uint32) bool {
	if ordinal == 0 || ordinal > n.expressions.count {
		return false
	}
	id := g.scalarRefs[uint64(n.expressions.start)+uint64(ordinal-1)]
	scalar := g.scalars[id-1]
	layout := g.layouts[scalar.Layout-1]
	return scalar.Kind == Const && !scalar.Nullable && layout.typ.ID == 4072 && layout.typ.Storage == model.TypeBytes
}
