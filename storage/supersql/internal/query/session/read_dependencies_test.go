package session

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestPreparedReadDependenciesPreserveScanOccurrence(t *testing.T) {
	limits := ir.Limits{MaxNodes: 64, MaxOperands: 256, MaxSlots: 32, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 64}
	parsed, err := zpgparse.Parse("SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits}, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := ir.NewBuilder(limits)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := builder.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	layout, err := builder.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Object: 31, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	relation, err := builder.AddRel(t.Context(), ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Scan, Scope: scope, Occurrence: 9, Object: 31, DescriptorVersion: 3, Generation: 2, EncodingVersion: 1},
		Outputs: []model.SlotID{slot},
	})
	if err != nil {
		t.Fatal(err)
	}
	logical, err := builder.Build(t.Context(), relation)
	if err != nil {
		t.Fatal(err)
	}
	target := storage.ObjectRef{ObjectID: 31, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1}
	properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
	request := storage.PushdownRequest{
		Target: target, Occurrence: 9, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}},
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: typ}}, ColumnIDs: []model.ColumnID{1},
		Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{
		Graphs: []*ir.Graph{logical},
		Nodes: []physical.NodeSpec{{
			Logical: physical.RelRef{Graph: 1, Rel: relation}, Algorithm: physical.PrimaryKeyScan,
			Output: []physical.Slot{{Graph: 1, ID: slot, Layout: layout}}, Delivered: properties, Break: physical.Streaming,
			Scan: physical.ScanSpec{
				Source: target, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, CapabilityEpoch: 1,
				Request: request, Projection: []model.ColumnID{1}, Ordering: []storage.ScanOrdering{{KeyOrdinal: 0, Type: typ}},
			},
		}},
		Root: 1,
	}, physical.Limits{MaxNodes: 16, MaxOperands: 128, MaxProperties: 128, MaxDepth: 16, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 16, MaxEdges: 32, MaxDepth: 16, MaxWork: 256, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	prepared := &Prepared{bound: bound, graph: fragments}
	dependencies, _, err := prepared.ReadDependencies(t.Context(), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 1 || dependencies[0].Target != target || dependencies[0].Occurrence != 9 || len(dependencies[0].Spans) != 1 {
		t.Fatalf("prepared scan dependencies = %#v", dependencies)
	}
}
