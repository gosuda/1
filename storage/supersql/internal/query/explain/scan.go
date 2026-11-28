package explain

import (
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (w *jsonWriter) scan(scan physical.ScanSpec) {
	request := scan.Request
	w.raw(`{"Source":`)
	w.fixed(scan.Source)
	w.raw(`,"Column Map":`)
	w.list(len(scan.ColumnMap), func(i int) { w.fixed(scan.ColumnMap[i]) })
	w.raw(`,"Target":`)
	w.fixed(request.Target)
	w.raw(`,"Index":`)
	w.fixed(request.IndexID)
	w.raw(`,"Direction":`)
	w.fixed(request.Direction)
	w.raw(`,"Capability Epoch":`)
	w.fixed(scan.CapabilityEpoch)
	w.raw(`,"Requested Projection":`)
	w.list(len(request.ColumnIDs), func(i int) { w.fixed(request.ColumnIDs[i]) })
	w.raw(`,"Projection":`)
	w.list(len(scan.Projection), func(i int) { w.fixed(scan.Projection[i]) })
	w.raw(`,"Layout":`)
	w.list(len(request.Layout), func(i int) {
		column := request.Layout[i]
		w.raw(`{"ID":`)
		w.fixed(column.ID)
		w.raw(`,"Type":`)
		w.layout(model.TypeLayout{Type: column.Type, Children: column.Children}, 0)
		w.raw(`,"Nullable":`)
		w.fixed(column.Nullable)
		if column.HasMissingValue {
			w.raw(`,"Missing Value":`)
			w.datum(column.MissingValue, 0)
		}
		w.raw("}")
	})
	w.raw(`,"Key Layout":`)
	w.list(len(request.KeyLayout), func(i int) { w.fixed(request.KeyLayout[i]) })
	w.raw(`,"Spans":`)
	w.list(len(request.Spans), func(i int) {
		w.raw(`{"Lower":`)
		w.bound(request.Spans[i].Lower)
		w.raw(`,"Upper":`)
		w.bound(request.Spans[i].Upper)
		w.raw("}")
	})
	w.raw(`,"Requested Predicate":`)
	w.predicate(request.Predicate)
	w.raw(`,"Pushed Predicate":`)
	w.predicate(scan.Accepted)
	w.raw(`,"Residual Predicate":`)
	w.predicate(scan.Residual)
	w.raw(`,"Ordering":`)
	w.list(len(scan.Ordering), func(i int) { w.fixed(scan.Ordering[i]) })
	w.raw(`,"Requested Limit":`)
	w.fixed(request.SafeLimit)
	w.raw(`,"Applied Limit":`)
	w.fixed(scan.AppliedLimit)
	w.raw(`,"Limit Owner":`)
	w.fixed(scan.LimitOwner)
	w.raw(`,"Estimate":`)
	w.fixed(scan.Estimate)
	w.raw(`,"Lookup":`)
	w.lookup(scan.Lookup)
	w.raw("}")
}

func (w *jsonWriter) lookup(lookup physical.LookupScanSpec) {
	w.raw(`{"Enabled":`)
	w.fixed(lookup.Enabled)
	if !lookup.Enabled {
		w.raw("}")
		return
	}
	request := lookup.BaseRequest
	w.raw(`,"Owner Column":`)
	w.fixed(lookup.OwnerColumn)
	w.raw(`,"Base Target":`)
	w.fixed(request.Target)
	w.raw(`,"Base Column Map":`)
	w.list(len(lookup.BaseColumnMap), func(i int) { w.fixed(lookup.BaseColumnMap[i]) })
	w.raw(`,"Requested Projection":`)
	w.list(len(request.ColumnIDs), func(i int) { w.fixed(request.ColumnIDs[i]) })
	w.raw(`,"Projection":`)
	w.list(len(lookup.BaseProjection), func(i int) { w.fixed(lookup.BaseProjection[i]) })
	w.raw(`,"Key Layout":`)
	w.list(len(request.KeyLayout), func(i int) { w.fixed(request.KeyLayout[i]) })
	w.raw("}")
}

func (w *jsonWriter) layout(layout model.TypeLayout, depth uint32) {
	if w.err != nil {
		return
	}
	if depth >= w.depth {
		w.err = ErrLimit
		return
	}
	w.raw(`{"Type":`)
	w.fixed(layout.Type)
	w.raw(`,"Children":`)
	w.list(len(layout.Children), func(i int) { w.layout(layout.Children[i], depth+1) })
	w.raw("}")
}

func (w *jsonWriter) bound(bound storage.KeyBound) {
	w.raw(`{"Unbounded":`)
	w.fixed(bound.Unbounded)
	w.raw(`,"Inclusive":`)
	w.fixed(bound.Inclusive)
	w.raw(`,"Values":`)
	w.list(len(bound.Values), func(i int) { w.datum(bound.Values[i], 0) })
	w.raw("}")
}

func (w *jsonWriter) predicate(predicate storage.Predicate) {
	if predicate.Empty() {
		w.raw(`{"Constant":true}`)
		return
	}
	w.raw(`{"Root":`)
	w.fixed(predicate.Root())
	w.raw(`,"Nodes":`)
	w.list(predicate.NodeCount(), func(i int) {
		node := predicate.Node(uint32(i))
		w.raw(`{"ID":`)
		w.fixed(i)
		w.raw(`,"Kind":`)
		w.text(predicateName(node.Kind))
		w.raw(`,"Metadata":`)
		w.fixed(node)
		if node.Kind == storage.PredicateConstant {
			w.raw(`,"Constant":`)
			w.datum(predicate.Constant(node.ConstantIndex), 0)
		}
		if node.Kind == storage.PredicateIn {
			w.raw(`,"Arguments":`)
			w.list(int(node.ArgumentCount), func(j int) { w.fixed(predicate.Argument(node.FirstArgument + uint32(j))) })
		}
		w.raw("}")
	})
	w.raw("}")
}

func predicateName(kind storage.PredicateKind) string {
	switch kind {
	case storage.PredicateColumn:
		return "Column"
	case storage.PredicateConstant:
		return "Constant"
	case storage.PredicateComparison:
		return "Comparison"
	case storage.PredicateNullTest:
		return "NullTest"
	case storage.PredicateIn:
		return "In"
	case storage.PredicateAnd:
		return "And"
	case storage.PredicateOr:
		return "Or"
	default:
		return "Unknown"
	}
}
